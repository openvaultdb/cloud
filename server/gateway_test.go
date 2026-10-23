package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const gatewayTestSecret = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type gatewayRoundTrip func(*http.Request) (*http.Response, error)

func (f gatewayRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testGateway(t *testing.T, inner http.Handler, transport http.RoundTripper) http.Handler {
	t.Helper()
	h, e := newGateway(inner, func(name string) string {
		switch name {
		case originSecretEnv:
			return gatewayTestSecret
		case "SNEAT_API_ORIGIN":
			return "https://sneat.example"
		case "OVDB_DEVICE_AUTH_PROXY_SECRET":
			return "separate-proxy-credential"
		}
		return ""
	}, transport)
	if e != nil {
		t.Fatal(e)
	}
	return h
}
func gatewayRequest(t *testing.T, h http.Handler, method, host, p string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "https://"+host+p, body)
	r.Header.Set(defaultOriginSecretHeader, gatewayTestSecret)
	r.Header.Set("CF-Connecting-IP", "192.0.2.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestGatewayRequiresEdgeAndStripsBeforeInner(t *testing.T) {
	calls := 0
	h := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		for _, name := range []string{defaultOriginSecretHeader, "CF-Connecting-IP", "Authorization", "X-OVDB-Proxy-Secret", "X-Forwarded-For"} {
			if r.Header.Get(name) != "" {
				t.Errorf("inner received %s", name)
			}
		}
		w.WriteHeader(200)
	}), nil)
	for _, values := range [][]string{nil, {"wrong"}, {gatewayTestSecret, gatewayTestSecret}} {
		r := httptest.NewRequest("GET", "https://"+cloudHost+"/v1", nil)
		r.Header[defaultOriginSecretHeader] = values
		r.Header.Set("CF-Connecting-IP", "192.0.2.1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	if calls != 0 {
		t.Fatal("unauthenticated inner access")
	}
	r := httptest.NewRequest("GET", "https://"+cloudHost+"/v1", nil)
	r.Header.Set(defaultOriginSecretHeader, gatewayTestSecret)
	r.Header.Set("Authorization", "Bearer private")
	r.Header.Set("CF-Connecting-IP", "192.0.2.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || calls != 1 {
		t.Fatal(w.Code, calls)
	}
	if _, e := newGateway(http.NotFoundHandler(), func(string) string { return "" }, nil); e == nil {
		t.Fatal("unset origin credential accepted")
	}
	if got := gatewayRequest(t, h, "GET", "unrecognized.example", "/v1", nil).Code; got != 421 {
		t.Fatal(got)
	}
}
func TestGatewayCanonicalMethodsAndBodies(t *testing.T) {
	calls := 0
	var gotPath string
	h := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; gotPath = r.URL.Path; w.WriteHeader(200) }), nil)
	for _, p := range []string{"/v1//tugql", "/v1/%2e%2e/ovdb", "/v1/databases/chinook/tugql;foo", "/v1/%2Ftugql", "/v1/%5ctugql", "/v1/%3Btugql"} {
		if got := gatewayRequest(t, h, "GET", cloudHost, p, nil).Code; got != 400 {
			t.Errorf("%s: %d", p, got)
		}
	}
	if got := gatewayRequest(t, h, "POST", cloudHost, "/v1/databases/chinook/tug%71l", strings.NewReader("{}")).Code; got != 200 || gotPath != "/v1/databases/chinook/tugql" {
		t.Fatal(got, gotPath)
	}
	before := calls
	r := httptest.NewRequest("POST", "https://"+cloudHost+"/v1/databases/chinook/tugql", strings.NewReader(strings.Repeat("x", gatewayBodyLimit+1)))
	r.ContentLength = -1
	r.Header.Set(defaultOriginSecretHeader, gatewayTestSecret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 413 || calls != before {
		t.Fatal(w.Code, calls)
	}
	for _, p := range []string{"/ecb/v1/databases/ecb/tugql", "/iana/v1/databases/iana-http-status/tugql", "/ecb-public"} {
		if got := gatewayRequest(t, h, "POST", cloudHost, p, nil).Code; got != 503 {
			t.Fatal(p, got)
		}
	}
}
func TestGatewayDemoDBMetadataAllowlistAndCORS(t *testing.T) {
	calls := 0
	h := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("OVDB-Source-SHA256", "source-pin")
		w.Header().Set("Access-Control-Allow-Origin", "wrong")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "chinook", "endpoints": map[string]string{"tugql": "https://" + cloudHost + "/v1/databases/chinook/tugql"}})
	}), nil)
	w := gatewayRequest(t, h, "GET", demoHost, "/v1", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "https://"+demoHost+"/v1") || calls != 0 {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
	w = gatewayRequest(t, h, "GET", demoHost, "/v1/databases/chinook", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "https://"+demoHost+"/v1/databases/chinook/tugql") || w.Header().Get("OVDB-Source-SHA256") != "source-pin" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	for _, p := range []string{"/v1/databases/not-admitted", "/oauth/token", "/api/databases", "/ovdb/dbs/chinook"} {
		if got := gatewayRequest(t, h, "GET", demoHost, p, nil).Code; got != 404 {
			t.Fatal(p, got)
		}
	}
	if got := gatewayRequest(t, h, "DELETE", demoHost, "/v1/databases/chinook/read", nil).Code; got != 403 {
		t.Fatal(got)
	}
	before := calls
	w = gatewayRequest(t, h, "OPTIONS", demoHost, "/v1/databases/chinook/tugql", nil)
	if w.Code != 204 || calls != before {
		t.Fatal(w.Code, calls)
	}
}
func TestGatewayStaticAndUnknownRoutes(t *testing.T) {
	h := testGateway(t, http.NotFoundHandler(), nil)
	for _, host := range []string{cloudHost, demoHost} {
		w := gatewayRequest(t, h, "GET", host, "/", nil)
		if w.Code != 302 || w.Header().Get("Location") != "/device/" {
			t.Fatal(w.Code, w.Header())
		}
		w = gatewayRequest(t, h, "GET", host, "/device/", nil)
		if w.Code != 200 || w.Header().Get("Content-Security-Policy") != gatewayCSP {
			t.Fatal(w.Code)
		}
		w = gatewayRequest(t, h, "GET", host, "/.well-known/oauth-authorization-server", nil)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), cloudHost) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, p := range []string{"/_headers", "/_redirects", "/unknown", "/api/unknown"} {
		if got := gatewayRequest(t, h, "GET", cloudHost, p, nil).Code; got != 404 {
			t.Fatal(p, got)
		}
	}
}
func TestGatewayNarrowProxyHeadersAndQuery(t *testing.T) {
	calls := 0
	h := testGateway(t, http.NotFoundHandler(), gatewayRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "sneat.example" || r.URL.Path != "/v0/ovdb/cloud/databases" || r.URL.RawQuery != "pageSize=2&space=s" {
			t.Errorf("%s", r.URL)
		}
		if r.Header.Get(defaultOriginSecretHeader) != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-OVDB-Proxy-Secret") != "separate-proxy-credential" {
			t.Error(r.Header)
		}
		return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": {"Bearer"}, "Set-Cookie": {"private"}, "Retry-After": {"3"}}, Body: io.NopCloser(strings.NewReader("denied"))}, nil
	}))
	w := gatewayRequest(t, h, "GET", cloudHost, "/api/databases?space=s&pageSize=2&upstream=evil", nil)
	if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Set-Cookie") != "" {
		t.Fatal(w.Code, w.Header())
	}
	before := calls
	if got := gatewayRequest(t, h, "POST", cloudHost, "/api/databases", nil).Code; got != 405 || calls != before {
		t.Fatal(got, calls)
	}
}
func TestGatewayLimiterReplicaScopeCapacityAndIPv6(t *testing.T) {
	now := time.Now()
	makeLimiter := func() *gatewayLimiter {
		return &gatewayLimiter{buckets: map[string]gatewayBucket{}, now: func() time.Time { return now }, capacity: 1}
	}
	a, b := makeLimiter(), makeLimiter()
	if !a.allow("query", "ip", 1) || a.allow("query", "ip", 1) || !b.allow("query", "ip", 1) || a.allow("query", "other", 1) {
		t.Fatal("replica/capacity scope")
	}
	now = now.Add(time.Minute)
	if !a.allow("query", "other", 1) {
		t.Fatal("expired capacity not reclaimed")
	}
	if gatewayActor("2001:db8:1:2::1") != gatewayActor("2001:db8:1:2::abcd") || gatewayActor("::ffff:192.0.2.1") != "192.0.2.1" || gatewayActor("spoof") != "unknown" {
		t.Fatal("IP normalization")
	}
}
func TestGatewayAuthStreamCancellationAndPartialWrite(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	h := testGateway(t, http.NotFoundHandler(), gatewayRoundTrip(func(r *http.Request) (*http.Response, error) {
		close(started)
		reader, writer := io.Pipe()
		go func() {
			_, _ = writer.Write([]byte("first"))
			<-r.Context().Done()
			close(cancelled)
			_ = writer.CloseWithError(r.Context().Err())
		}()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "https://"+cloudHost+"/api/devices", nil).WithContext(ctx)
	r.Header.Set(defaultOriginSecretHeader, gatewayTestSecret)
	done := make(chan struct{})
	var body bytes.Buffer
	w := &gatewayStreamWriter{header: make(http.Header), body: &body, first: make(chan struct{})}
	go func() { defer close(done); defer func() { _ = recover() }(); h.ServeHTTP(w, r) }()
	<-started
	select {
	case <-w.first:
	case <-time.After(time.Second):
		t.Fatal("stream buffered")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream context not cancelled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not finish")
	}
}

type gatewayStreamWriter struct {
	header http.Header
	body   *bytes.Buffer
	first  chan struct{}
	once   bool
}

func (w *gatewayStreamWriter) Header() http.Header { return w.header }
func (w *gatewayStreamWriter) WriteHeader(int)     {}
func (w *gatewayStreamWriter) Write(p []byte) (int, error) {
	n, e := w.body.Write(p)
	if !w.once {
		w.once = true
		close(w.first)
	}
	return n, e
}
func (w *gatewayStreamWriter) Flush() {}

func TestGatewayCORSDoesNotInheritInnerPermission(t *testing.T) {
	h := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.WriteHeader(200)
	}), nil)
	for _, origin := range []string{"https://attacker.example", "https://datatug.app"} {
		r := httptest.NewRequest("GET", "https://"+cloudHost+"/v1", nil)
		r.Header.Set(defaultOriginSecretHeader, gatewayTestSecret)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		expected := ""
		if origin == "https://datatug.app" {
			expected = origin
		}
		if w.Header().Get("Access-Control-Allow-Origin") != expected || w.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatal(w.Header())
		}
	}
}
func TestGatewayQueryClassIsCanonicalAndPreflightIndependent(t *testing.T) {
	calls := 0
	h := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }), nil)
	for i := 0; i < 60; i++ {
		if w := gatewayRequest(t, h, "GET", cloudHost, "/v1/databases/chinook/tug%71l", nil); w.Code != 200 {
			t.Fatal(i, w.Code)
		}
	}
	w := gatewayRequest(t, h, "GET", cloudHost, "/v1/databases/chinook/tugql", nil)
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Header())
	}
	before := calls
	if w := gatewayRequest(t, h, "OPTIONS", cloudHost, "/v1/databases/chinook/tugql", nil); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if calls != before {
		t.Fatal("preflight executed query")
	}
	if w := gatewayRequest(t, h, "GET", cloudHost, "/v1", nil); w.Code != 200 {
		t.Fatal("query exhausted read", w.Code)
	}
}
func TestGatewayMetadataFailsClosedAndDoesNotBufferQueries(t *testing.T) {
	for _, body := range []string{`{"id":"other","endpoints":{"tugql":"https://cloud.openvaultdb.com/v1/databases/chinook/tugql"}}`, `{"id":"chinook","endpoints":{"tugql":"https://attacker.example"}}`, strings.Repeat("x", gatewayBodyLimit+1)} {
		h := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }), nil)
		w := gatewayRequest(t, h, "GET", demoHost, "/v1/databases/chinook", nil)
		if w.Code != 502 || strings.Contains(w.Body.String(), "attacker") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestGatewayEmbeddedPolicyMatchesHTTPAndRedirects(t *testing.T) {
	policy, err := gatewayAssets.ReadFile("gatewayassets/public/_headers")
	if err != nil {
		t.Fatal(err)
	}
	var csp string
	for _, line := range strings.Split(string(policy), "\n") {
		if strings.HasPrefix(line, "  Content-Security-Policy: ") {
			csp = strings.TrimPrefix(line, "  Content-Security-Policy: ")
			break
		}
	}
	h := testGateway(t, http.NotFoundHandler(), nil)
	w := gatewayRequest(t, h, "HEAD", demoHost, "/device/", nil)
	if csp == "" || w.Header().Get("Content-Security-Policy") != csp || w.Header().Get("X-Robots-Tag") != "noindex" || w.Body.Len() != 0 {
		t.Fatal(w.Header(), w.Body.String())
	}
	w = gatewayRequest(t, h, "GET", cloudHost, "/device?code=public-code", nil)
	if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != "/device/?code=public-code" {
		t.Fatal(w.Code, w.Header())
	}
}

func TestGatewayDeclaredCompanionOriginsAcrossMethods(t *testing.T) {
	for _, origin := range []string{"https://shipping.demodb.dev", "https://shipping-logs.demodb.dev", "https://undeclared.invalid"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodOptions} {
			t.Run(origin+"/"+method, func(t *testing.T) {
				calls := 0
				h := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("Access-Control-Allow-Origin", "*")
					w.Header().Set("Access-Control-Allow-Credentials", "true")
					w.WriteHeader(http.StatusOK)
				}), nil)
				r := httptest.NewRequest(method, "https://"+cloudHost+"/v1/databases/shipping/tugql", strings.NewReader(`{"query":"from rows"}`))
				r.Header.Set(defaultOriginSecretHeader, gatewayTestSecret)
				r.Header.Set("Origin", origin)
				r.Header.Set("Access-Control-Request-Method", "POST")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				wantStatus, wantCalls := http.StatusOK, 1
				if method == http.MethodOptions {
					wantStatus, wantCalls = http.StatusNoContent, 0
				}
				wantOrigin := origin
				if origin == "https://undeclared.invalid" {
					wantOrigin = ""
				}
				if w.Code != wantStatus || calls != wantCalls || w.Header().Get("Access-Control-Allow-Origin") != wantOrigin || w.Header().Get("Access-Control-Allow-Credentials") != "" || w.Header().Get("Vary") != "Origin" {
					t.Fatal(w.Code, calls, w.Header())
				}
				if !strings.Contains(w.Header().Get("Access-Control-Allow-Methods"), "POST") || !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "OVDB-Page-Token") {
					t.Fatal(w.Header())
				}
			})
		}
	}
}
