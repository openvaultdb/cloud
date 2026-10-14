package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"gopkg.in/yaml.v3"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const ecbPublicPath = "/ecb-public/v1/databases/ecb/dtql"
const ecbPublicSecretHeader = "X-OVDB-ECB-Public-Secret"
const ecbPublicAdmissionHeader = "X-OVDB-ECB-Public-Admission"
const ecbPublicProofHeader = "X-OVDB-ECB-Public-Completion"

// Deliberately tighter than approved request/output ceilings to bound parsing.
const ecbPublicRequestBytes = 8 << 10
const ecbPublicResponseBytes = 64 << 10
const ecbPublicContentType = "application/json"

var selectedECBPublicMount ecbMount = selectedECBMount
var ecbPublicID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var ecbNativeCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
var ecbNativeRate = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)

type ecbPublicAdmission struct {
	Format                  string `json:"format"`
	Decision                string `json:"decision"`
	ApprovedBy              string `json:"approvedBy"`
	ApprovedAt              string `json:"approvedAt"`
	ExpiresAt               string `json:"expiresAt"`
	CostOwner               string `json:"costOwner"`
	HostConfigSHA256        string `json:"hostConfigSHA256"`
	PublisherManifestSHA256 string `json:"publisherManifestSHA256"`
	DecoderModuleVersion    string `json:"decoderModuleVersion"`
	DecoderSHA256           string `json:"decoderSHA256"`
	RightsDigest            string `json:"rightsDigest"`
	RequestProfile          string `json:"requestProfile"`
	WorkerPath              string `json:"workerPath"`
	GoPath                  string `json:"goPath"`
	DirectoryOrigin         string `json:"directoryOrigin"`
	BackendOrigin           string `json:"backendOrigin"`
	Audience                string `json:"audience"`
	PaidAccess              bool   `json:"paidAccess"`
	MaxReads                int    `json:"maxReads"`
	MaxRows                 int    `json:"maxRows"`
	MaxConcurrent           int    `json:"maxConcurrent"`
	ExecutionsPerMinute     int    `json:"executionsPerMinute"`
}

func checkedECBPublicAdmission(path, digest, configPath string) (ecbPublicAdmission, ecbHostConfig, error) {
	fail := func() (ecbPublicAdmission, ecbHostConfig, error) {
		return ecbPublicAdmission{}, ecbHostConfig{}, errECBCandidate
	}
	raw, err := readECBFile(path, 4096)
	if err != nil || len(digest) != 64 || fmt.Sprintf("%x", sha256.Sum256(raw)) != digest || rejectDuplicateJSON(raw) != nil {
		return fail()
	}
	fields := map[string]byte{}
	for _, k := range []string{"format", "decision", "approvedBy", "approvedAt", "expiresAt", "costOwner", "hostConfigSHA256", "publisherManifestSHA256", "decoderModuleVersion", "decoderSHA256", "rightsDigest", "requestProfile", "workerPath", "goPath", "directoryOrigin", "backendOrigin", "audience"} {
		fields[k] = '"'
	}
	for _, k := range []string{"maxReads", "maxRows", "maxConcurrent", "executionsPerMinute"} {
		fields[k] = 'n'
	}
	fields["paidAccess"] = 'b'
	if _, err = canonicalMembers(raw, fields, nil, true); err != nil {
		return fail()
	}
	var a ecbPublicAdmission
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&a) != nil || d.Decode(new(any)) != io.EOF {
		return fail()
	}
	approved, e1 := time.Parse(time.RFC3339, a.ApprovedAt)
	expires, e2 := time.Parse(time.RFC3339, a.ExpiresAt)
	now := time.Now()
	if e1 != nil || e2 != nil || approved.After(now) || !expires.After(now) || !expires.After(approved) || expires.Sub(approved) > 30*24*time.Hour || strings.TrimSpace(a.ApprovedBy) == "" || strings.TrimSpace(a.CostOwner) == "" {
		return fail()
	}
	backend, e1 := url.Parse(a.BackendOrigin)
	directory, e2 := url.Parse(a.DirectoryOrigin)
	if e1 != nil || e2 != nil || backend.Scheme != "https" || !strings.HasSuffix(backend.Hostname(), ".run.app") || backend.Port() != "" || backend.User != nil || backend.Path != "" || backend.RawQuery != "" || backend.ForceQuery || backend.Fragment != "" || directory.Scheme != "https" || directory.User != nil || directory.Path != "" || directory.RawQuery != "" || directory.ForceQuery || directory.Fragment != "" || directory.Host == "" {
		return fail()
	}
	if a.Format != "ovdb-ecb-public-free-admission/1" || a.Decision != "public-free-transient-read-only" || a.Audience != "public-free" || a.PaidAccess || a.PublisherManifestSHA256 != ecbPublisherSHA || a.DecoderModuleVersion != "v0.4.0" || a.DecoderSHA256 != ecbDecoderSHA || a.RightsDigest != ecbRightsSHA || a.RequestProfile != "ecb-public-free/1" || a.WorkerPath != ecbPublicPath || a.GoPath != ecbQueryPath || a.MaxReads != 1 || a.MaxRows != 50 || a.MaxConcurrent != 1 || a.ExecutionsPerMinute != 6 {
		return fail()
	}
	c, err := checkedECBConfigVersion(configPath, true, a.HostConfigSHA256)
	if err != nil {
		return fail()
	}
	return a, c, nil
}
func configuredHandlerWithECBPublic(startup runtimeStartup) ([]runtimeDatabase, http.Handler, func() error, error) {
	enabled := os.Getenv("OVDB_ECB_PUBLIC_ENABLED")
	if enabled != "" && enabled != "false" && enabled != "true" {
		return nil, nil, nil, errECBCandidate
	}
	if enabled != "true" {
		return configuredHandlerWithRuntime(startup)
	}
	if os.Getenv("OVDB_ECB_ENABLED") == "true" {
		return nil, nil, nil, errECBCandidate
	}
	digest := os.Getenv("OVDB_ECB_PUBLIC_ADMISSION_SHA256")
	path := os.Getenv("OVDB_ECB_PUBLIC_HOST_CONFIG")
	a, c, err := checkedECBPublicAdmission(os.Getenv("OVDB_ECB_PUBLIC_ADMISSION_FILE"), digest, path)
	secret := os.Getenv("OVDB_ECB_PUBLIC_PROXY_SECRET")
	if err != nil || !validECBProxySecret(secret) || secret == os.Getenv("OVDB_ECB_PROXY_SECRET") {
		return nil, nil, nil, errECBCandidate
	}
	candidate, closeCandidate, err := assembleECBHostVersion(path, selectedECBPublicMount, selectedECBDiagnostic, true, a.HostConfigSHA256)
	if err != nil {
		return nil, nil, nil, errECBCandidate
	}
	providers, samples, closeSamples, err := configuredHandlerWithRuntime(startup)
	if err != nil {
		_ = closeCandidate()
		return nil, nil, nil, err
	}
	public := newECBPublicGate(candidate, a, c, digest, secret)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == ecbQueryPath {
			public.ServeHTTP(w, r)
		} else {
			samples.ServeHTTP(w, r)
		}
	})
	return providers, h, func() error { return errors.Join(closeCandidate(), closeSamples()) }, nil
}

// Only transient service counters; no query, row, execution ID or user state.
// Global cost control also requires the Worker limiter and later deployment cap.
type ecbPublicGate struct {
	next           http.Handler
	admission      ecbPublicAdmission
	config         ecbHostConfig
	digest, secret string
	mu             sync.Mutex
	active         bool
	minute         time.Time
	count          int
}

func newECBPublicGate(next http.Handler, a ecbPublicAdmission, c ecbHostConfig, digest, secret string) *ecbPublicGate {
	return &ecbPublicGate{next: next, admission: a, config: c, digest: digest, secret: secret}
}
func publicECBError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", ecbPublicContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"ECB public request refused."}`)
}
func oneHeader(h http.Header, name string) string {
	var all []string
	for k, v := range h {
		if strings.EqualFold(k, name) {
			all = append(all, v...)
		}
	}
	if len(all) != 1 {
		return ""
	}
	return all[0]
}
func (g *ecbPublicGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for k := range r.Header {
		lower := strings.ToLower(k)
		if lower == "authorization" || lower == "cookie" || strings.Contains(lower, "paid") || strings.Contains(lower, "entitlement") || strings.Contains(lower, "billing") || strings.Contains(lower, "operator") || strings.Contains(lower, "page") || lower == strings.ToLower(ecbProxySecretHeader) {
			publicECBError(w, 404)
			return
		}
	}
	supplied := oneHeader(r.Header, ecbPublicSecretHeader)
	expected := sha256.Sum256([]byte(g.secret))
	actual := sha256.Sum256([]byte(supplied))
	id := oneHeader(r.Header, "OVDB-Execution-ID")
	origin := oneHeader(r.Header, "Origin")
	if subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 || oneHeader(r.Header, ecbPublicAdmissionHeader) != g.digest || !ecbPublicID.MatchString(id) || origin != g.admission.DirectoryOrigin || r.Method != http.MethodPost || r.URL.EscapedPath() != ecbQueryPath || r.URL.RawQuery != "" || r.URL.ForceQuery || (oneHeader(r.Header, "Content-Type") != "application/yaml" && oneHeader(r.Header, "Content-Type") != "text/plain") {
		publicECBError(w, 404)
		return
	}
	expires, _ := time.Parse(time.RFC3339, g.admission.ExpiresAt)
	if !time.Now().Before(expires) {
		publicECBError(w, 503)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 9*time.Second)
	defer cancel()
	controller := http.NewResponseController(w)
	cutoff, _ := ctx.Deadline()
	if err := controller.SetReadDeadline(cutoff); err != nil && !errors.Is(err, http.ErrNotSupported) {
		publicECBError(w, 503)
		return
	}
	if err := controller.SetWriteDeadline(cutoff); err != nil && !errors.Is(err, http.ErrNotSupported) {
		publicECBError(w, 503)
		return
	}
	defer func() { _ = controller.SetReadDeadline(time.Time{}); _ = controller.SetWriteDeadline(time.Time{}) }()
	r.Body = http.MaxBytesReader(w, r.Body, ecbPublicRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil || ctx.Err() != nil || !ecbEqualityOnly(body) {
		publicECBError(w, 422)
		return
	}
	g.mu.Lock()
	now := time.Now()
	if now.Sub(g.minute) >= time.Minute {
		g.minute = now
		g.count = 0
	}
	if g.active || g.count >= 6 {
		g.mu.Unlock()
		publicECBError(w, 429)
		return
	}
	g.active = true
	g.count++
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.active = false; g.mu.Unlock() }()
	capture := &ecbBoundedWriter{header: make(http.Header), ctx: ctx, body: make([]byte, 0, ecbPublicResponseBytes)}
	request := r.Clone(ctx)
	request.Body = io.NopCloser(bytes.NewReader(body))
	completed := false
	defer func() {
		if !completed {
			publicECBError(w, 503)
		}
	}()
	// Absorb aborts/panics without flushing rows or diagnostic payloads.
	func() {
		defer func() {
			if recover() != nil {
				capture.failed = true
			}
		}()
		g.next.ServeHTTP(capture, request)
	}()
	if ctx.Err() != nil || capture.failed || capture.status != 200 || capture.header.Get("Content-Type") != ecbPublicContentType || !validateECBPublicResponse(capture.body, g.config, id) {
		return
	}
	if ctx.Err() != nil {
		return
	}
	proof := ecbCompletionProof(g.secret, g.digest, id, 200, ecbPublicContentType, capture.body)
	if ctx.Err() != nil {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", ecbPublicContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set(ecbPublicProofHeader, proof)
	completed = true
	w.WriteHeader(200)
	_, _ = w.Write(capture.body)
}

type ecbBoundedWriter struct {
	header http.Header
	status int
	body   []byte
	ctx    context.Context
	failed bool
}

func (w *ecbBoundedWriter) Header() http.Header { return w.header }
func (w *ecbBoundedWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *ecbBoundedWriter) Write(p []byte) (int, error) {
	if w.ctx.Err() != nil || len(p) > ecbPublicResponseBytes-len(w.body) {
		w.failed = true
		return 0, errECBCandidate
	}
	if w.status == 0 {
		w.status = 200
	}
	w.body = append(w.body, p...)
	return len(p), nil
}
func (w *ecbBoundedWriter) Flush() {} // No caller commitment before validation.
func ecbCompletionProof(secret, digest, id string, status int, ct string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "ovdb-ecb-public-completion/1\n%s\n%s\n%d\n%s\n", digest, id, status, ct)
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Adds equality-only policy; the released original-wire guard checks all other
// native fields/projections/limit/unsupported operations before provider I/O.
func ecbEqualityOnly(body []byte) bool {
	if len(body) > ecbPublicRequestBytes {
		return false
	}
	d := yaml.NewDecoder(bytes.NewReader(body))
	var root yaml.Node
	if d.Decode(&root) != nil || d.Decode(new(yaml.Node)) != io.EOF {
		return false
	}
	nodes := 0
	var walk func(*yaml.Node, int) bool
	walk = func(n *yaml.Node, depth int) bool {
		nodes++
		if nodes > 1024 || depth > 16 || n.Kind == yaml.AliasNode || n.Anchor != "" || len(n.Value) > 1024 {
			return false
		}
		if n.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				key := n.Content[i].Value
				if seen[key] || key == "<<" {
					return false
				}
				seen[key] = true
				if key == "op" && n.Content[i+1].Value != "==" {
					return false
				}
			}
		}
		for _, child := range n.Content {
			if !walk(child, depth+1) {
				return false
			}
		}
		return true
	}
	return walk(&root, 0) && len(root.Content) == 1 && publicECBQueryNode(root.Content[0])
}
func validateECBPublicResponse(raw []byte, c ecbHostConfig, id string) bool {
	if len(raw) > ecbPublicResponseBytes || !utf8.Valid(raw) || !boundedECBJSON(raw) {
		return false
	}
	var response struct {
		Records []struct {
			Key  string            `json:"key"`
			Data map[string]string `json:"data"`
		} `json:"records"`
		Complete bool                    `json:"complete"`
		Rights   []license.SourceRight   `json:"sourceRights"`
		Used     []string                `json:"usedSourceIds"`
		Reads    *providerreads.Envelope `json:"providerReads"`
	}
	if _, err := canonicalMembers(raw, map[string]byte{"records": '[', "complete": 'b', "sourceRights": '[', "usedSourceIds": '[', "providerReads": '{'}, nil, true); err != nil {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&response) != nil || d.Decode(new(any)) != io.EOF || !response.Complete || response.Records == nil || len(response.Records) > 50 {
		return false
	}
	plan := providerreads.Plan{Execution: providerreads.Execution{ID: id, Mode: "proxy", ExecutorID: "openvaultdb-cloud"}, Bindings: []providerreads.Binding{c.Binding}, Requests: []providerreads.Request{{ResourceID: "ecb-daily", Method: "GET", UpstreamURL: manifest.ECBDailyURL, Params: map[string]any{}}}, SourceRights: []license.SourceRight{c.SourceRight}, MaxReads: new(1), MaxMetadataBytes: providerreads.MaxMetadataBytes}
	if providerreads.ValidateMetadata(providerreads.Metadata{SourceRights: response.Rights, UsedSourceIDs: response.Used, ProviderReads: response.Reads}, plan, []string{c.SourceRight.SourceID}) != nil || response.Reads == nil || len(response.Reads.Reads) != 1 {
		return false
	}
	observed := response.Reads.Reads[0]
	if observed.Bytes > 2<<20 || observed.ReferenceDate == "" {
		return false
	}
	reference, err := time.Parse("2006-01-02", observed.ReferenceDate)
	if err != nil || reference.Format("2006-01-02") != observed.ReferenceDate {
		return false
	}
	for _, record := range response.Records {
		if len(record.Key) > 128 || len(record.Data) < 1 || len(record.Data) > 3 {
			return false
		}
		for field, value := range record.Data {
			if len(value) > 128 {
				return false
			}
			switch field {
			case "time":
				if value != observed.ReferenceDate {
					return false
				}
			case "currency":
				if !ecbNativeCurrency.MatchString(value) {
					return false
				}
			case "rate":
				if !ecbNativeRate.MatchString(value) || strings.Trim(value, "0.") == "" {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

// Token/depth/string caps run before typed object expansion. Reuse existing
// duplicate-key guard rather than implementing a second evidence protocol.
func boundedECBJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	depth := 0
	for tokens := 0; ; tokens++ {
		if tokens > 2048 {
			return false
		}
		token, err := d.Token()
		if err == io.EOF {
			return depth == 0 && rejectDuplicateJSON(raw) == nil
		}
		if err != nil {
			return false
		}
		if s, ok := token.(string); ok && len(s) > 4096 {
			return false
		}
		if delim, ok := token.(json.Delim); ok {
			if delim == '{' || delim == '[' {
				depth++
				if depth > 16 {
					return false
				}
			} else {
				depth--
				if depth < 0 {
					return false
				}
			}
		}
	}
}

func publicECBMapping(n *yaml.Node, allowed ...string) (map[string]*yaml.Node, bool) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, false
	}
	m := map[string]*yaml.Node{}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i].Value
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
			}
		}
		if !found || n.Content[i].Tag != "!!str" {
			return nil, false
		}
		m[k] = n.Content[i+1]
	}
	return m, true
}
func publicECBField(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str" && (n.Value == "time" || n.Value == "currency" || n.Value == "rate")
}
func publicECBQueryNode(n *yaml.Node) bool {
	m, ok := publicECBMapping(n, "from", "columns", "where", "limit")
	if !ok {
		return false
	}
	from, ok := publicECBMapping(m["from"], "name")
	if !ok || len(from) != 1 || from["name"].Value != "daily" || from["name"].Tag != "!!str" {
		return false
	}
	limit := m["limit"]
	if limit == nil || limit.Kind != yaml.ScalarNode || limit.Tag != "!!int" {
		return false
	}
	number, err := strconv.Atoi(limit.Value)
	if err != nil || number < 1 || number > 50 {
		return false
	}
	if columns := m["columns"]; columns != nil {
		if columns.Kind != yaml.SequenceNode || len(columns.Content) < 1 || len(columns.Content) > 3 {
			return false
		}
		seen := map[string]bool{}
		for _, c := range columns.Content {
			fields, ok := publicECBMapping(c, "field")
			if !ok || len(fields) != 1 || !publicECBField(fields["field"]) || seen[fields["field"].Value] {
				return false
			}
			seen[fields["field"].Value] = true
		}
	}
	return m["where"] == nil || publicECBCondition(m["where"])
}
func publicECBCondition(n *yaml.Node) bool {
	m, ok := publicECBMapping(n, "op", "left", "right", "and", "or")
	if !ok {
		return false
	}
	for _, group := range []string{"and", "or"} {
		if g := m[group]; g != nil {
			if len(m) != 1 || g.Kind != yaml.SequenceNode || len(g.Content) < 1 || len(g.Content) > 10 {
				return false
			}
			for _, child := range g.Content {
				if !publicECBCondition(child) {
					return false
				}
			}
			return true
		}
	}
	if len(m) != 3 || m["op"] == nil || m["op"].Tag != "!!str" || m["op"].Value != "==" {
		return false
	}
	left, ok := publicECBMapping(m["left"], "field")
	if !ok || len(left) != 1 || !publicECBField(left["field"]) {
		return false
	}
	right, ok := publicECBMapping(m["right"], "value")
	return ok && len(right) == 1 && right["value"] != nil && right["value"].Kind == yaml.ScalarNode && right["value"].Tag == "!!str" && len(right["value"].Value) <= 128
}
