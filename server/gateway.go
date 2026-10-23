package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const cloudHost = "cloud.openvaultdb.com"
const demoHost = "ovdb.demodb.dev"
const gatewayBodyLimit = 1 << 20
const gatewayCSP = "default-src 'self'; script-src 'self' https://www.gstatic.com https://apis.google.com; connect-src 'self' https://www.gstatic.com https://*.googleapis.com https://securetoken.googleapis.com https://identitytoolkit.googleapis.com https://auth.sneat.co; frame-src https://auth.sneat.co; img-src 'self' data:; style-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

//go:embed all:gatewayassets
var gatewayAssets embed.FS

// legacyDTQLSegment is the pre-rename query route name. Deployed browser clients
// (DataTug apps, demodb.dev sites) still call it until they move to /tugql, and
// openvaultdb-go keeps serving it behind its own shim. Removed in TugQL cutover step 9.
const legacyDTQLSegment = "dtql"

var publicQueryPath = regexp.MustCompile(`^/v1/databases/[^/]+/(tugql|` + legacyDTQLSegment + `|query|batch|read)/?$`)
var demoDBPath = regexp.MustCompile(`^/v1/databases/([a-z][a-z0-9-]{0,39})(/.*)?$`)
var demoRecordPath = regexp.MustCompile(`^/records/[^/]+(/[^/]+)+$`)
var gatewayDatabaseID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var gatewayAllowedOrigins = strings.Fields("https://adventureworks.demodb.dev https://chinook.demodb.dev https://chinookdb.com https://countries.demodb.dev https://datatug.app https://demodb.dev https://employees.demodb.dev https://northwind.demodb.dev https://pubs.demodb.dev https://sakila.demodb.dev https://shipping.demodb.dev https://shipping-logs.demodb.dev https://www.chinookdb.com https://www.demodb.dev")

// Limits are independent per process/revision, not a global distributed quota.
// Keys expire in one minute; exhausted key capacity refuses new keys.
type gatewayBucket struct {
	start time.Time
	count int
}
type gatewayLimiter struct {
	mu       sync.Mutex
	buckets  map[string]gatewayBucket
	now      func() time.Time
	capacity int
}

func (l *gatewayLimiter) allow(class, actor string, maximum int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for key, b := range l.buckets {
		if now.Sub(b.start) >= time.Minute {
			delete(l.buckets, key)
		}
	}
	key := class + ":" + actor
	b, exists := l.buckets[key]
	if !exists {
		if len(l.buckets) >= l.capacity {
			return false
		}
		b.start = now
	}
	if b.count >= maximum {
		return false
	}
	b.count++
	l.buckets[key] = b
	return true
}
func gatewayActor(value string) string {
	address, err := netip.ParseAddr(value)
	if err != nil {
		return "unknown"
	}
	address = address.Unmap()
	if address.Is6() {
		return netip.PrefixFrom(address, 64).Masked().String()
	}
	return address.String()
}

type gateway struct {
	inner       http.Handler
	limiter     *gatewayLimiter
	proxy       *httputil.ReverseProxy
	metadata    map[string]json.RawMessage
	query       map[string]bool
	serverIndex []byte
	listus      *demoRelay
}

// Production composition requires an origin credential. The only route
// admitted without it is the Cloud Run startup probe (withStartupProbeRoute),
// which reveals only whether the instance is ready.
func newGateway(inner http.Handler, getenv func(string) string, transport http.RoundTripper) (http.Handler, error) {
	if getenv(originSecretEnv) == "" {
		return nil, errors.New("OVDB_ORIGIN_SECRET is required")
	}
	g := &gateway{inner: inner, limiter: &gatewayLimiter{buckets: map[string]gatewayBucket{}, now: time.Now, capacity: 4096}}
	raw, err := gatewayAssets.ReadFile("gatewayassets/demodb-ovdb.json")
	if err != nil {
		return nil, err
	}
	var config struct {
		Server map[string]json.RawMessage `json:"server"`
		Query  map[string]bool            `json:"query"`
	}
	if err = json.Unmarshal(raw, &config); err != nil {
		return nil, err
	}
	var databases []map[string]json.RawMessage
	if err = json.Unmarshal(config.Server["databases"], &databases); err != nil || len(databases) == 0 {
		return nil, errors.New("missing DemoDB metadata")
	}
	g.metadata = map[string]json.RawMessage{}
	g.query = config.Query
	for _, database := range databases {
		var id string
		if json.Unmarshal(database["localId"], &id) != nil || !runtimeDatabaseIDPattern.MatchString(id) {
			return nil, errors.New("invalid DemoDB database")
		}
		if _, ok := g.query[id]; !ok {
			return nil, errors.New("missing DemoDB query capability")
		}
		database["apiUrl"], _ = json.Marshal("https://" + demoHost + "/v1/databases/" + id)
		g.metadata[id], _ = json.Marshal(database)
	}
	if len(g.metadata) != len(g.query) {
		return nil, errors.New("inconsistent DemoDB capability catalog")
	}
	config.Server["apiUrl"], _ = json.Marshal("https://" + demoHost + "/v1")
	config.Server["databases"], _ = json.Marshal(databases)
	g.serverIndex, _ = json.Marshal(config.Server)
	origin := getenv("SNEAT_API_ORIGIN")
	proxySecret := getenv("OVDB_DEVICE_AUTH_PROXY_SECRET")
	if origin != "" || proxySecret != "" {
		u, e := url.Parse(origin)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || u.Opaque != "" || u.Path != "" && u.Path != "/" || proxySecret == "" || len(proxySecret) > 4096 || strings.ContainsAny(proxySecret, "\r\n") {
			return nil, errors.New("invalid Sneat proxy configuration")
		}
		if transport == nil {
			transport = &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 60 * time.Second}
		}
		g.proxy = &httputil.ReverseProxy{Transport: transport, FlushInterval: -1, Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = u.Host
			pr.Out.Header = make(http.Header)
			for _, name := range []string{"Authorization", "Content-Type", "Accept"} {
				if v := pr.In.Header.Get(name); v != "" {
					pr.Out.Header.Set(name, v)
				}
			}
			pr.Out.Header.Set("X-OVDB-Proxy-Secret", proxySecret)
		}, ModifyResponse: func(response *http.Response) error {
			headers := make(http.Header)
			for _, name := range []string{"Content-Type", "Retry-After", "WWW-Authenticate"} {
				if v := response.Header.Get(name); v != "" {
					headers.Set(name, v)
				}
			}
			headers.Set("Cache-Control", "no-store")
			headers.Set("Pragma", "no-cache")
			response.Header = headers
			return nil
		}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			gatewayError(w, http.StatusServiceUnavailable, "Authorization service unavailable")
		}}
	}
	g.listus = newDemoRelay(getenv)
	public, err := withOriginSecret(g, getenv)
	if err != nil {
		return nil, err
	}
	return withStartupProbeRoute(public, inner), nil
}

func gatewayError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
func gatewaySecurity(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}
func gatewayCORS(w http.ResponseWriter, r *http.Request, demo bool, methods string) {
	h := w.Header()
	h.Del("Access-Control-Allow-Origin")
	h.Del("Access-Control-Allow-Credentials")
	h.Set("Vary", "Origin")
	origin := r.Header.Get("Origin")
	if demo {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		for _, allowed := range gatewayAllowedOrigins {
			if origin == allowed {
				h.Set("Access-Control-Allow-Origin", origin)
				break
			}
		}
	}
	h.Set("Access-Control-Allow-Methods", methods)
	h.Set("Access-Control-Allow-Headers", "Content-Type, OVDB-Execution-ID, OVDB-Page-Size, OVDB-Page-Token, OVDB-Page-Close, OVDB-Provider-Revision, OVDB-Source-SHA256, OVDB-Serving-SHA256, OVDB-Manifest-SHA256")
	h.Set("Access-Control-Expose-Headers", "Cache-Control, ETag, Last-Modified, Link, Vary, OVDB-Page-Size, OVDB-Page-Token, OVDB-Provider-Revision, OVDB-Source-SHA256, OVDB-Serving-SHA256, OVDB-Manifest-SHA256, Retry-After")
	h.Set("Access-Control-Max-Age", "86400")
}

// Use the same spelling for classification and dispatch; never let ServeMux
// normalize an ambiguous escaped path after it receives the cheaper read class.
func canonicalGatewayPath(raw string) (string, error) {
	out := ""
	for i := 0; i < len(raw); i++ {
		if raw[i] != '%' {
			out += raw[i : i+1]
			continue
		}
		if i+2 >= len(raw) {
			return "", errors.New("escape")
		}
		value, e := strconv.ParseUint(raw[i+1:i+3], 16, 8)
		if e != nil {
			return "", e
		}
		c := byte(value)
		if c == '/' || c == '\\' || c == ';' || c == 0 {
			return "", errors.New("separator")
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", rune(c)) {
			out += string(c)
		} else {
			out += fmt.Sprintf("%%%02X", c)
		}
		i += 2
	}
	if !strings.HasPrefix(out, "/") || strings.Contains(out, "//") || strings.ContainsAny(out, "\\;") {
		return "", errors.New("path")
	}
	for _, segment := range strings.Split(out, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("segment")
		}
	}
	return out, nil
}
func (g *gateway) admit(r *http.Request, class string, limit int) bool {
	ip := r.Header.Values("CF-Connecting-IP")
	actor := "unknown"
	if len(ip) == 1 {
		actor = gatewayActor(ip[0])
	}
	return g.limiter.allow(class, actor, limit)
}
func (g *gateway) limited(w http.ResponseWriter, r *http.Request, class string, limit int) bool {
	if g.admit(r, class, limit) {
		return false
	}
	w.Header().Set("Retry-After", "60")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(429)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "rate_limited", "message": "Too many requests. Try again in one minute."}})
	return true
}
func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	gatewaySecurity(w.Header())
	ctx, cancel := context.WithTimeout(r.Context(), 115*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Second))
	host := r.Host
	// Session origins are not the two catalog hosts. Admit them only far enough
	// for the disabled relay to fail closed. The origin-secret wrapper has
	// already run. Every other unknown host is still rejected before path parsing.
	if !potentialDemoOriginHost(host) && host != cloudHost && host != demoHost {
		gatewayError(w, 421, "Unknown public host")
		return
	}
	canonical, e := canonicalGatewayPath(r.URL.EscapedPath())
	if e != nil {
		gatewayError(w, 400, "Invalid canonical path")
		return
	}
	r = r.Clone(r.Context())
	r.URL.RawPath = canonical
	r.URL.Path, _ = url.PathUnescape(canonical)
	if g.listus != nil && g.listus.handle(w, r, canonical) {
		return
	}
	if host != cloudHost && host != demoHost {
		gatewayError(w, 421, "Unknown public host")
		return
	}
	if strings.HasPrefix(canonical, "/ecb") || strings.HasPrefix(canonical, "/iana/") {
		if host == demoHost {
			http.NotFound(w, r)
			return
		}
		gatewayError(w, 503, "Service is not configured")
		return
	}
	public := canonical == "/v1" || strings.HasPrefix(canonical, "/v1/") || canonical == "/ovdb" || strings.HasPrefix(canonical, "/ovdb/") || canonical == "/.well-known/openvaultdb"
	if host == demoHost && !strings.HasPrefix(canonical, "/v1") {
		if strings.HasPrefix(canonical, "/api/") || strings.HasPrefix(canonical, "/oauth/") || public {
			http.NotFound(w, r)
			return
		}
	}
	if public {
		w = &gatewayCORSWriter{ResponseWriter: w, request: r, demo: host == demoHost, methods: "GET, HEAD, POST, OPTIONS"}
		gatewayCORS(w, r, host == demoHost, "GET, HEAD, POST, OPTIONS")
		class, limit := "read", 300
		if isCrossDatabaseQueryPath(canonical) || publicQueryPath.MatchString(canonical) {
			class, limit = "query", 60
		}
		if r.Method == "OPTIONS" {
			class, limit = "preflight", 600
		}
		if g.limited(w, r, class, limit) {
			return
		}
		if host == demoHost {
			g.demo(w, r, canonical)
			return
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		if !gatewayReadBody(w, r) {
			return
		}
		g.serveInner(w, r)
		return
	}
	if strings.HasPrefix(canonical, "/oauth/") || strings.HasPrefix(canonical, "/api/") {
		g.auth(w, r, canonical)
		return
	}
	g.asset(w, r, canonical)
}
func gatewayReadBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return true
	}
	reader := http.MaxBytesReader(w, r.Body, gatewayBodyLimit)
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		gatewayError(w, 413, "Request body is too large or invalid")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return true
}
func (g *gateway) auth(w http.ResponseWriter, r *http.Request, p string) {
	if demoAPIPath(p) {
		g.serveDemoAPI(w, r, p)
		return
	}
	routes := map[string][2]string{"/oauth/device/code": {"POST", "/v0/ovdb/device_auth/code"}, "/oauth/token": {"POST", "/v0/ovdb/device_auth/token"}, "/oauth/userinfo": {"GET", "/v0/ovdb/device_auth/userinfo"}, "/oauth/revoke": {"POST", "/v0/ovdb/device_auth/revoke"}, "/api/device-authorization": {"GET", "/v0/ovdb/device_auth/preview"}, "/api/device-authorization/decision": {"POST", "/v0/ovdb/device_auth/decision"}, "/api/devices": {"GET", "/v0/ovdb/device_auth/devices"}, "/api/devices/revoke": {"POST", "/v0/ovdb/device_auth/devices/revoke"}, "/api/databases": {"GET", "/v0/ovdb/cloud/databases"}}
	route, ok := routes[p]
	query := r.URL.Query()
	if strings.HasPrefix(p, "/api/databases/") {
		id := strings.TrimPrefix(p, "/api/databases/")
		if !gatewayDatabaseID.MatchString(id) {
			gatewayError(w, 400, "Invalid database id")
			return
		}
		route = [2]string{"GET", "/v0/ovdb/cloud/database"}
		query = url.Values{"id": {id}}
		ok = true
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != route[0] {
		w.Header().Set("Allow", route[0])
		gatewayError(w, 405, "Method not allowed")
		return
	}
	limit := 60
	if p == "/oauth/device/code" {
		limit = 10
	}
	// The key comes from the fixed owning route, never a caller-selected ID.
	if !g.admit(r, "device:"+route[1], limit) {
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		body := map[string]string{"error": "Too many attempts. Try again in one minute."}
		switch p {
		case "/oauth/token":
			body = map[string]string{"error": "slow_down", "error_description": "Polling limit exceeded."}
		case "/oauth/device/code":
			body = map[string]string{"error": "temporarily_unavailable", "error_description": "Try again in one minute."}
		case "/oauth/userinfo", "/oauth/revoke":
			body = map[string]string{"error": "temporarily_unavailable"}
		}
		w.WriteHeader(429)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	if g.proxy == nil {
		gatewayError(w, 503, "Authorization is not configured")
		return
	}
	if !gatewayReadBody(w, r) {
		return
	}
	if p == "/api/databases" {
		filtered := url.Values{}
		for _, name := range []string{"space", "pageSize", "pageToken"} {
			for _, value := range query[name] {
				filtered.Add(name, value)
			}
		}
		query = filtered
	}
	r.URL.Path = route[1]
	r.URL.RawPath = ""
	r.URL.RawQuery = query.Encode()
	g.proxy.ServeHTTP(w, r)
}
func (g *gateway) demo(w http.ResponseWriter, r *http.Request, p string) {
	methods := "GET, HEAD, OPTIONS"
	if cw, ok := w.(*gatewayCORSWriter); ok {
		cw.methods = methods
	}
	gatewayCORS(w, r, true, methods)
	if p == "/v1" || p == "/v1/" {
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			w.Header().Set("Allow", methods)
			gatewayError(w, 405, "Method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		if r.Method != "HEAD" {
			_, _ = w.Write(g.serverIndex)
		}
		return
	}
	match := demoDBPath.FindStringSubmatch(p)
	if match == nil {
		http.NotFound(w, r)
		return
	}
	id, suffix := match[1], match[2]
	if _, ok := g.metadata[id]; !ok {
		http.NotFound(w, r)
		return
	}
	metadata := suffix == "" || suffix == "/"
	query := suffix == "/tugql" || suffix == "/"+legacyDTQLSegment || suffix == "/query"
	if !metadata && !query && suffix != "/read" && suffix != "/inferred-schema" && !demoRecordPath.MatchString(suffix) {
		http.NotFound(w, r)
		return
	}
	if query && !g.query[id] {
		http.NotFound(w, r)
		return
	}
	if query {
		methods = "GET, HEAD, POST, OPTIONS"
		if cw, ok := w.(*gatewayCORSWriter); ok {
			cw.methods = methods
		}
		gatewayCORS(w, r, true, methods)
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" && (!query || r.Method != "POST") {
		w.Header().Set("Allow", methods)
		status := 405
		if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" || r.Method == "DELETE" {
			status = 403
		}
		gatewayError(w, status, "OpenVaultDB is read-only")
		return
	}
	if !gatewayReadBody(w, r) {
		return
	}
	if metadata {
		g.demoMetadata(w, r, id)
		return
	}
	g.serveInner(w, r)
}

// isCrossDatabaseQueryPath reports the cross-database query route, current or legacy.
func isCrossDatabaseQueryPath(canonical string) bool {
	trimmed := strings.TrimSuffix(canonical, "/")
	return trimmed == "/v1/tugql" || trimmed == "/v1/"+legacyDTQLSegment
}

// rebrandEndpoint checks that the backend advertised the endpoint at its own
// host and points it at the demo host. An absent key is accepted only when
// optional, as the legacy key is once the library drops it.
func rebrandEndpoint(endpoints map[string]string, key, id string, optional bool) bool {
	value, present := endpoints[key]
	if !present {
		return optional
	}
	if value != "https://"+cloudHost+"/v1/databases/"+id+"/"+key {
		return false
	}
	endpoints[key] = "https://" + demoHost + "/v1/databases/" + id + "/" + key
	return true
}

// Only the small descriptor is buffered/rebranded, never result streams.
func (g *gateway) demoMetadata(w http.ResponseWriter, r *http.Request, id string) {
	capture := &gatewayMetadataWriter{header: make(http.Header)}
	g.serveInner(capture, r)
	if capture.status == 0 {
		capture.status = 200
	}
	for name, values := range capture.header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	gatewayCORS(w, r, true, "GET, HEAD, OPTIONS")
	if capture.status >= 300 && capture.status < 400 {
		gatewayError(w, 502, "Unexpected backend redirect")
		return
	}
	if capture.status >= 400 || r.Method == "HEAD" {
		w.WriteHeader(capture.status)
		if r.Method != "HEAD" {
			_, _ = w.Write(capture.body.Bytes())
		}
		return
	}
	var descriptor map[string]json.RawMessage
	var endpoints map[string]string
	var gotID string
	if capture.failed || json.Unmarshal(capture.body.Bytes(), &descriptor) != nil || json.Unmarshal(descriptor["id"], &gotID) != nil || gotID != id || json.Unmarshal(descriptor["endpoints"], &endpoints) != nil || !rebrandEndpoint(endpoints, "tugql", id, false) || !rebrandEndpoint(endpoints, legacyDTQLSegment, id, true) {
		gatewayError(w, 502, "Invalid backend metadata")
		return
	}
	descriptor["endpoints"], _ = json.Marshal(endpoints)
	w.Header().Del("ETag")
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(descriptor)
}

type gatewayMetadataWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
	failed bool
}

func (w *gatewayMetadataWriter) Header() http.Header { return w.header }
func (w *gatewayMetadataWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *gatewayMetadataWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.body.Len()+len(body) > gatewayBodyLimit {
		w.failed = true
		return 0, errors.New("metadata capacity")
	}
	return w.body.Write(body)
}
func (g *gateway) asset(w http.ResponseWriter, r *http.Request, p string) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		gatewayError(w, 405, "Method not allowed")
		return
	}
	if p == "/" {
		target := "/device/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	if r.Host == demoHost {
		w.Header().Set("X-Robots-Tag", "noindex")
	}
	name := strings.TrimPrefix(p, "/")
	if name == "device" || name == "devices" {
		target := p + "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}
	if strings.HasSuffix(name, "/") {
		name += "index.html"
	}
	if name == "_headers" || name == "_redirects" || !fs.ValidPath(name) || path.Base(name) == "." {
		http.NotFound(w, r)
		return
	}
	body, e := gatewayAssets.ReadFile("gatewayassets/public/" + name)
	if e != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", gatewayCSP)
		w.WriteHeader(404)
		if r.Method != "HEAD" {
			body, _ = gatewayAssets.ReadFile("gatewayassets/public/404.html")
			_, _ = w.Write(body)
		}
		return
	}
	w.Header().Set("Content-Security-Policy", gatewayCSP)
	content := mime.TypeByExtension(path.Ext(name))
	if name == ".well-known/oauth-authorization-server" {
		content = "application/json; charset=utf-8"
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
	}
	if content != "" {
		w.Header().Set("Content-Type", content)
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
}

func (g *gateway) serveInner(w http.ResponseWriter, r *http.Request) {
	copy := r.Clone(r.Context())
	copy.Header = make(http.Header)
	for _, name := range []string{"Accept", "Content-Type", "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers", "OVDB-Execution-ID", "OVDB-Page-Size", "OVDB-Page-Token", "OVDB-Page-Close", "OVDB-Provider-Revision", "OVDB-Source-SHA256", "OVDB-Serving-SHA256", "OVDB-Manifest-SHA256"} {
		for _, value := range r.Header.Values(name) {
			copy.Header.Add(name, value)
		}
	}
	g.inner.ServeHTTP(w, copy)
}

type gatewayCORSWriter struct {
	http.ResponseWriter
	request *http.Request
	demo    bool
	methods string
	written bool
}

func (w *gatewayCORSWriter) WriteHeader(status int) {
	if w.written {
		return
	}
	w.written = true
	gatewayCORS(w.ResponseWriter, w.request, w.demo, w.methods)
	w.ResponseWriter.WriteHeader(status)
}
func (w *gatewayCORSWriter) Write(p []byte) (int, error) {
	if !w.written {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(p)
}
func (w *gatewayCORSWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *gatewayCORSWriter) Flush() {
	if !w.written {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
