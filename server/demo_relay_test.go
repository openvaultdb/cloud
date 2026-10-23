package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const demoEncryptionKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY"

var demoFixedNow = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

const demoStablePath = "/users/owner_1/demo-sneat-space/ovdb/v1/databases/demo-sneat-space/records/lists/do%3Ademo"

func demoConfig() map[string]string {
	return map[string]string{
		"OVDB_DEMO_ENABLED":             "true",
		"OVDB_DEMO_CONTROL_SECRET":      "control-secret",
		"OVDB_DEMO_ENCRYPTION_KEY":      demoEncryptionKey,
		"OVDB_DEMO_FIREBASE_PROJECT_ID": "sneat-eur3-1",
		"OVDB_DEMO_CORS_ORIGIN":         "https://listus.app",
		"OVDB_DEMO_ORIGIN_HOST_SUFFIX":  "openvaultdb.com",
	}
}

func newTestRelay(now time.Time, store *memoryDemoStore, env map[string]string) *demoRelay {
	if env == nil {
		env = demoConfig()
	}
	relay := newDemoRelay(func(name string) string { return env[name] })
	relay.now = func() time.Time { return now }
	if store != nil {
		relay.store = store
	}
	relay.origin = demoRoundTrip(func(*http.Request) (*http.Response, error) {
		return jsonRoundTrip(http.StatusOK, map[string]any{"ok": true}), nil
	})
	return relay
}

func jsonRoundTrip(status int, body any) *http.Response {
	encoded, _ := json.Marshal(body)
	return &http.Response{
		StatusCode:    status,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(encoded)),
		ContentLength: int64(len(encoded)),
	}
}

type demoRoundTrip func(*http.Request) (*http.Response, error)

func (f demoRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func demoSession(id, origin string, expires time.Time) map[string]any {
	if origin == "" {
		origin = "https://ovdb-demo-" + id + ".openvaultdb.com/"
	}
	if expires.IsZero() {
		expires = demoFixedNow.Add(time.Minute)
	}
	return map[string]any{
		"sessionId": id, "ownerUserId": "owner_1", "spaceId": "space_1",
		"databaseId": "demo-sneat-space", "expiresAt": expires.UTC().Format("2006-01-02T15:04:05.000Z"),
		"originUrl": origin, "originToken": "database-secret",
	}
}

func controlHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json", demoControlHeader: "control-secret"}
}

func relayDo(d *demoRelay, method, host, path string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "https://"+host+path, body)
	req.Host = host
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	if !d.handle(rec, req, path) {
		http.NotFound(rec, req)
	}
	return rec
}

func putDemoSession(t *testing.T, relay *demoRelay, value map[string]any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	rec := relayDo(relay, http.MethodPut, cloudHost, "/internal/demo/sessions/"+value["sessionId"].(string), bytes.NewReader(body), controlHeaders())
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put session: %d %s", rec.Code, rec.Body.String())
	}
}

func demoErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status %d body %q: %v", rec.Code, rec.Body.String(), err)
	}
	return body["error"]
}

func TestDemoRelayCleanupStaysAvailableWhileDisabled(t *testing.T) {
	store := newMemoryDemoStore()
	enabled := newTestRelay(demoFixedNow, store, nil)
	putDemoSession(t, enabled, demoSession("session-demo-000001", "", time.Time{}))
	disabledEnv := map[string]string{"OVDB_DEMO_ENABLED": "false", "OVDB_DEMO_CONTROL_SECRET": "control-secret"}
	disabled := newTestRelay(demoFixedNow, store, disabledEnv)
	path := "/internal/demo/sessions/session-demo-000001"
	if rec := relayDo(disabled, http.MethodDelete, cloudHost, path, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing secret: %d", rec.Code)
	}
	if rec := relayDo(disabled, http.MethodDelete, cloudHost, path, nil, map[string]string{demoControlHeader: "wrong"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d", rec.Code)
	}
	withoutSecret := newTestRelay(demoFixedNow, store, map[string]string{"OVDB_DEMO_ENABLED": "false"})
	if rec := relayDo(withoutSecret, http.MethodDelete, cloudHost, path, nil, controlHeaders()); rec.Code != http.StatusServiceUnavailable || demoErrorCode(t, rec) != "demo_unavailable" {
		t.Fatalf("removed secret: %d %s", rec.Code, rec.Body.String())
	}
	withoutStore := newTestRelay(demoFixedNow, store, disabledEnv)
	withoutStore.store = nil
	if rec := relayDo(withoutStore, http.MethodDelete, cloudHost, path, nil, controlHeaders()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("removed store: %d", rec.Code)
	}
	if _, ok := store.get(sessionKey("session-demo-000001"), demoFixedNow); !ok {
		t.Fatal("failed cleanup attempts deleted the session")
	}
	body, _ := json.Marshal(demoSession("session-demo-000001", "", time.Time{}))
	if rec := relayDo(disabled, http.MethodPut, cloudHost, path, bytes.NewReader(body), controlHeaders()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled put: %d", rec.Code)
	}
	if rec := relayDo(disabled, http.MethodPost, cloudHost, "/api/demo/sessions", nil, controlHeaders()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled create: %d", rec.Code)
	}
	if rec := relayDo(disabled, http.MethodGet, cloudHost, demoStablePath, nil, map[string]string{"Authorization": "Bearer irrelevant"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled data: %d", rec.Code)
	}
	if rec := relayDo(disabled, http.MethodDelete, cloudHost, path, nil, controlHeaders()); rec.Code != http.StatusNoContent || store.count(demoFixedNow) != 0 {
		t.Fatalf("cleanup: %d keys=%d", rec.Code, store.count(demoFixedNow))
	}
	if rec := relayDo(disabled, http.MethodDelete, cloudHost, path, nil, controlHeaders()); rec.Code != http.StatusNoContent {
		t.Fatalf("repeat cleanup: %d", rec.Code)
	}
}

func TestDemoRelayStoresEncryptedImmutableSession(t *testing.T) {
	store := newMemoryDemoStore()
	relay := newTestRelay(demoFixedNow, store, nil)
	putDemoSession(t, relay, demoSession("session-demo-000001", "", time.Time{}))
	raw, ok := store.get(sessionKey("session-demo-000001"), demoFixedNow)
	if !ok || strings.Contains(raw, "database-secret") {
		t.Fatalf("stored session leaked the origin credential: %s", raw)
	}
	putDemoSession(t, relay, demoSession("session-demo-000001", "", time.Time{}))
	store.delete(ownerKey("owner_1"))
	store.delete(spaceKey("space_1"))
	store.delete(originKey("ovdb-demo-session-demo-000001.openvaultdb.com"))
	putDemoSession(t, relay, demoSession("session-demo-000001", "", time.Time{}))
	owner, ok := store.get(ownerKey("owner_1"), demoFixedNow)
	if !ok || owner != "session-demo-000001" {
		t.Fatalf("index repair: %q", owner)
	}
	putDemoSession(t, relay, demoSession("session-demo-000002", "", time.Time{}))
	if _, ok := store.get(sessionKey("session-demo-000001"), demoFixedNow); ok {
		t.Fatal("replaced session was kept")
	}
	rec := relayDo(relay, http.MethodDelete, cloudHost, "/internal/demo/sessions/session-demo-000002", nil, controlHeaders())
	if rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if _, ok := store.get(sessionKey("session-demo-000002"), demoFixedNow); ok {
		t.Fatal("deleted session was kept")
	}
}

func TestDemoRelayRejectsInvalidControlAndExpiredLease(t *testing.T) {
	store := newMemoryDemoStore()
	relay := newTestRelay(demoFixedNow, store, nil)
	body, _ := json.Marshal(demoSession("session-demo-000001", "", time.Time{}))
	rec := relayDo(relay, http.MethodPut, cloudHost, "/internal/demo/sessions/session-demo-000001", bytes.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing control secret: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPut, "https://"+cloudHost+"/internal/demo/sessions/session-demo-000001", bytes.NewReader(body))
	req.Host = cloudHost
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(demoControlHeader, "control-secret")
	req.Header.Set("Content-Length", "70000")
	req.ContentLength = 70000
	huge := httptest.NewRecorder()
	relay.handle(huge, req, "/internal/demo/sessions/session-demo-000001")
	if huge.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: %d %s", huge.Code, huge.Body.String())
	}
	putDemoSession(t, relay, demoSession("session-demo-000001", "", demoFixedNow.Add(time.Second)))
	expired := newTestRelay(demoFixedNow.Add(time.Second), store, nil)
	rec = relayDo(expired, http.MethodGet, cloudHost, demoStablePath, nil, map[string]string{"Authorization": "Bearer invalid", "Origin": "https://listus.app"})
	if rec.Code != http.StatusGone || rec.Header().Get("Access-Control-Allow-Origin") != "https://listus.app" {
		t.Fatalf("expired: %d %s", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestDemoRelayVerifiesFirebaseBeforeProxying(t *testing.T) {
	store := newMemoryDemoStore()
	key := demoRSAKey(t)
	var received []*http.Request
	relay := newTestRelay(demoFixedNow.Add(-time.Millisecond), store, nil)
	relay.fetchJWKS = func(*http.Request) (*http.Response, error) {
		return demoJWKSResponse(t, key.PublicKey, "test_key", ""), nil
	}
	relay.origin = demoRoundTrip(func(r *http.Request) (*http.Response, error) {
		received = append(received, r.Clone(r.Context()))
		resp := jsonRoundTrip(http.StatusOK, map[string]any{"lists": []any{}})
		resp.Header.Set(demoControlHeader, "never-forward")
		return resp, nil
	})
	putDemoSession(t, relay, demoSession("session-demo-000001", "", time.Time{}))
	valid := signDemoToken(t, key, demoClaims(nil), "test_key")
	rec := relayDo(relay, http.MethodGet, cloudHost, demoStablePath, nil, map[string]string{
		"Authorization": "Bearer " + valid, "Origin": "https://listus.app", "Cookie": "must-not-forward",
	})
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "https://listus.app" || rec.Header().Get(demoControlHeader) != "" {
		t.Fatalf("proxy: %d %v", rec.Code, rec.Header())
	}
	if len(received) != 1 || received[0].URL.EscapedPath() != "/v1/databases/demo-sneat-space/records/lists/do%3Ademo" || received[0].Header.Get("Authorization") != "Bearer database-secret" || received[0].Header.Get("Cookie") != "" {
		t.Fatalf("origin request: %#v", received)
	}
	refused := relayDo(relay, http.MethodGet, cloudHost, demoStablePath, nil, map[string]string{"Authorization": "Bearer " + signDemoToken(t, key, demoClaims(map[string]any{"aud": "attacker"}), "test_key")})
	if refused.Code != http.StatusUnauthorized {
		t.Fatalf("audience: %d", refused.Code)
	}
	forbidden := relayDo(relay, http.MethodGet, cloudHost, demoStablePath, nil, map[string]string{"Authorization": "Bearer " + signDemoToken(t, key, demoClaims(map[string]any{"sub": "owner_2"}), "test_key")})
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("owner: %d", forbidden.Code)
	}
	malformed := relayDo(relay, http.MethodGet, cloudHost, demoStablePath, nil, map[string]string{"Authorization": "Bearer " + signDemoToken(t, key, demoClaims(map[string]any{"auth_time": "not-a-time"}), "test_key")})
	if malformed.Code != http.StatusUnauthorized {
		t.Fatalf("auth_time: %d", malformed.Code)
	}
	for _, override := range []map[string]any{
		{"iss": "https://securetoken.google.com/not-our-project"},
		{"exp": demoFixedNow.Add(-time.Millisecond).Unix()},
		{"iat": demoFixedNow.Unix() + 120},
	} {
		rejected := relayDo(relay, http.MethodGet, cloudHost, demoStablePath, nil, map[string]string{
			"Authorization": "Bearer " + signDemoToken(t, key, demoClaims(override), "test_key"),
			"Origin":        "https://listus.app",
		})
		if rejected.Code != http.StatusUnauthorized || rejected.Header().Get("Access-Control-Allow-Origin") != "https://listus.app" {
			t.Fatalf("claims %+v: %d %s", override, rejected.Code, rejected.Header().Get("Access-Control-Allow-Origin"))
		}
	}
	parts := strings.Split(valid, ".")
	if parts[2][0] == 'A' {
		parts[2] = "B" + parts[2][1:]
	} else {
		parts[2] = "A" + parts[2][1:]
	}
	tampered := relayDo(relay, http.MethodGet, cloudHost, demoStablePath, nil, map[string]string{"Authorization": "Bearer " + strings.Join(parts, ".")})
	if tampered.Code != http.StatusUnauthorized || len(received) != 1 {
		t.Fatalf("tampered status=%d origin calls=%d", tampered.Code, len(received))
	}
}

func TestDemoRelayRefreshesUnknownJWKOnce(t *testing.T) {
	store := newMemoryDemoStore()
	key := demoRSAKey(t)
	calls := 0
	relay := newTestRelay(demoFixedNow.Add(-time.Millisecond), store, nil)
	relay.fetchJWKS = func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return demoJWKSResponse(t, key.PublicKey, "test_key", "", true), nil
		}
		return demoJWKSResponse(t, key.PublicKey, "test_key", ""), nil
	}
	putDemoSession(t, relay, demoSession("session-demo-000001", "", time.Time{}))
	rec := relayDo(relay, http.MethodGet, cloudHost, demoStablePath, nil, map[string]string{"Authorization": "Bearer " + signDemoToken(t, key, demoClaims(nil), "test_key")})
	if rec.Code != http.StatusOK || calls != 2 {
		t.Fatalf("refresh: %d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
}

func TestDemoRelayUnknownJWKCooldown(t *testing.T) {
	store := newMemoryDemoStore()
	key := demoRSAKey(t)
	calls := 0
	relay := newTestRelay(demoFixedNow.Add(-time.Millisecond), store, nil)
	relay.fetchJWKS = func(*http.Request) (*http.Response, error) {
		calls++
		return demoJWKSResponse(t, key.PublicKey, "test_key", "public, max-age=60", true), nil
	}
	putDemoSession(t, relay, demoSession("session-demo-000001", "", time.Time{}))
	token := signDemoToken(t, key, demoClaims(nil), "cooldown-kid")
	headers := map[string]string{"Authorization": "Bearer " + token}
	if rec := relayDo(relay, http.MethodGet, cloudHost, demoStablePath, nil, headers); rec.Code != http.StatusUnauthorized {
		t.Fatal(rec.Code)
	}
	if rec := relayDo(relay, http.MethodGet, cloudHost, demoStablePath, nil, headers); rec.Code != http.StatusUnauthorized || calls != 2 {
		t.Fatalf("cooldown status=%d calls=%d", rec.Code, calls)
	}
}

func TestDemoRelayOriginHostUsesTheSameLease(t *testing.T) {
	store := newMemoryDemoStore()
	key := demoRSAKey(t)
	proxied := 0
	relay := newTestRelay(demoFixedNow.Add(-time.Millisecond), store, nil)
	relay.fetchJWKS = func(*http.Request) (*http.Response, error) {
		return demoJWKSResponse(t, key.PublicKey, "test_key", ""), nil
	}
	relay.origin = demoRoundTrip(func(*http.Request) (*http.Response, error) {
		proxied++
		return jsonRoundTrip(http.StatusOK, map[string]any{"ok": true}), nil
	})
	putDemoSession(t, relay, demoSession("session-demo-000001", "", time.Time{}))
	host := "ovdb-demo-session-demo-000001.openvaultdb.com"
	path := "/v1/databases/demo-sneat-space/records/lists/do%3Ademo"
	if rec := relayDo(relay, http.MethodGet, host, path, nil, nil); rec.Code != http.StatusUnauthorized || proxied != 0 {
		t.Fatalf("anonymous origin: %d proxied=%d", rec.Code, proxied)
	}
	owner := signDemoToken(t, key, demoClaims(nil), "test_key")
	if rec := relayDo(relay, http.MethodGet, host, path, nil, map[string]string{"Authorization": "Bearer " + owner}); rec.Code != http.StatusOK || proxied != 1 {
		t.Fatalf("owner origin: %d proxied=%d", rec.Code, proxied)
	}
	if rec := relayDo(relay, http.MethodGet, host, "/admin/tokens", nil, map[string]string{"Authorization": "Bearer " + owner}); rec.Code != http.StatusNotFound || proxied != 1 {
		t.Fatalf("admin origin: %d proxied=%d", rec.Code, proxied)
	}
}

func TestDemoRelayKeepsNearExpirySessionForThePlatformMinimum(t *testing.T) {
	store := newMemoryDemoStore()
	relay := newTestRelay(demoFixedNow, store, nil)
	putDemoSession(t, relay, demoSession("session-demo-000001", "", demoFixedNow.Add(40*time.Second)))
	expiration, ok := store.expiration(sessionKey("session-demo-000001"))
	if !ok || expiration < demoFixedNow.Unix()+60 {
		t.Fatalf("expiration %d ok=%v", expiration, ok)
	}
}

func TestDemoRelayFailsClosedWhenDisabledOrUnconfigured(t *testing.T) {
	disabled := newTestRelay(demoFixedNow, nil, map[string]string{"OVDB_DEMO_ENABLED": "false"})
	origin := "ovdb-demo-session-demo-000001.openvaultdb.com"
	path := "/v1/databases/demo-sneat-space/records/lists/do%3Ademo"
	rec := relayDo(disabled, http.MethodGet, origin, path, nil, nil)
	if rec.Code != http.StatusServiceUnavailable || demoErrorCode(t, rec) != "demo_unavailable" {
		t.Fatalf("disabled origin: %d %s", rec.Code, rec.Body.String())
	}
	rec = relayDo(disabled, http.MethodGet, cloudHost, "/api/demo/session?spaceId=space_1", nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled control: %d", rec.Code)
	}
	unconfigured := newTestRelay(demoFixedNow, nil, map[string]string{"OVDB_DEMO_ENABLED": "true"})
	rec = relayDo(unconfigured, http.MethodGet, origin, path, nil, nil)
	if rec.Code != http.StatusServiceUnavailable || demoErrorCode(t, rec) != "demo_unavailable" {
		t.Fatalf("unconfigured origin: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDemoRelayBoundsOriginFailures(t *testing.T) {
	store := newMemoryDemoStore()
	key := demoRSAKey(t)
	timeoutRelay := newTestRelay(demoFixedNow.Add(-time.Millisecond), store, nil)
	timeoutRelay.fetchJWKS = func(*http.Request) (*http.Response, error) {
		return demoJWKSResponse(t, key.PublicKey, "test_key", ""), nil
	}
	timeoutRelay.origin = demoRoundTrip(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	putDemoSession(t, timeoutRelay, demoSession("session-demo-000001", "", time.Time{}))
	token := signDemoToken(t, key, demoClaims(nil), "test_key")
	headers := map[string]string{"Authorization": "Bearer " + token}
	rec := relayDo(timeoutRelay, http.MethodGet, cloudHost, demoStablePath, nil, headers)
	if rec.Code != http.StatusGatewayTimeout || demoErrorCode(t, rec) != "origin_timeout" {
		t.Fatalf("timeout: %d %s", rec.Code, rec.Body.String())
	}
	redirect := newTestRelay(demoFixedNow.Add(-time.Millisecond), store, nil)
	redirect.fetchJWKS = timeoutRelay.fetchJWKS
	redirect.origin = demoRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://attacker.example"}}, Body: http.NoBody}, nil
	})
	rec = relayDo(redirect, http.MethodGet, cloudHost, demoStablePath, nil, headers)
	if rec.Code != http.StatusBadGateway || rec.Header().Get("Location") != "" || demoErrorCode(t, rec) != "origin_rejected" {
		t.Fatalf("redirect: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestDemoRelayAcceptsBodylessWriteAndRejectsArbitraryOrigin(t *testing.T) {
	store := newMemoryDemoStore()
	key := demoRSAKey(t)
	var paths []string
	relay := newTestRelay(demoFixedNow.Add(-time.Millisecond), store, nil)
	relay.fetchJWKS = func(*http.Request) (*http.Response, error) {
		return demoJWKSResponse(t, key.PublicKey, "test_key", ""), nil
	}
	relay.origin = demoRoundTrip(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.EscapedPath())
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})
	body, _ := json.Marshal(demoSession("session-demo-000001", "https://attacker.example/", time.Time{}))
	rec := relayDo(relay, http.MethodPut, cloudHost, "/internal/demo/sessions/session-demo-000001", bytes.NewReader(body), controlHeaders())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid origin: %d %s", rec.Code, rec.Body.String())
	}
	putDemoSession(t, relay, demoSession("session-demo-000001", "", time.Time{}))
	token := signDemoToken(t, key, demoClaims(nil), "test_key")
	rec = relayDo(relay, http.MethodPut, cloudHost, demoStablePath, strings.NewReader("{}"), map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"})
	if rec.Code != http.StatusNoContent || rec.Header().Get("Content-Type") != "" || len(paths) != 1 || paths[0] != "/v1/databases/demo-sneat-space/records/lists/do%3Ademo" {
		t.Fatalf("write: %d type=%q paths=%v", rec.Code, rec.Header().Get("Content-Type"), paths)
	}
}

func TestDemoRelayRejectsMaliciousStablePathsBeforeOrigin(t *testing.T) {
	store := newMemoryDemoStore()
	called := false
	relay := newTestRelay(demoFixedNow, store, nil)
	relay.origin = demoRoundTrip(func(*http.Request) (*http.Response, error) {
		called = true
		return jsonRoundTrip(http.StatusOK, map[string]any{"ok": true}), nil
	})
	putDemoSession(t, relay, demoSession("session-demo-000001", "", time.Time{}))
	for _, path := range []string{
		"/users/owner_1/demo-sneat-space/ovdb/v1/databases/demo-sneat-space/%2fsecret",
		"/users/owner_1/demo-sneat-space/ovdb/v1/databases/demo-sneat-space/../secret",
	} {
		rec := relayDo(relay, http.MethodGet, cloudHost, path, nil, nil)
		if rec.Code != http.StatusNotFound || called {
			t.Fatalf("%s: %d called=%v", path, rec.Code, called)
		}
	}
}

func TestDemoGatewayMapsControlRoutesAndKeepsThemDisabled(t *testing.T) {
	var calls []capturedDemoRequest
	transport := demoRoundTrip(func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		calls = append(calls, capturedDemoRequest{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, auth: r.Header.Get("Authorization"), cookie: r.Header.Get("Cookie"), proxy: r.Header.Get("X-OVDB-Proxy-Secret"), origin: r.Header.Get(defaultOriginSecretHeader), body: body})
		switch r.URL.Path {
		case "/v0/ovdb/demo/session/end":
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
		default:
			return jsonRoundTrip(http.StatusOK, map[string]any{"sessionId": "session_test", "spaceId": "space_1"}), nil
		}
	})
	disabled := newDemoGateway(t, nil, transport)
	if rec := demoGatewayDo(disabled, http.MethodPost, cloudHost, "/api/demo/sessions", strings.NewReader(`{"app":"listus"}`), map[string]string{"Authorization": "Bearer device-token", "Content-Type": "application/json"}); rec.Code != http.StatusServiceUnavailable || len(calls) != 0 {
		t.Fatalf("disabled create: %d calls=%d", rec.Code, len(calls))
	}
	origin := demoGatewayDo(disabled, http.MethodGet, "ovdb-demo-session-demo-000001.openvaultdb.com", "/v1/databases/demo-sneat-space/records/lists/do%3Ademo", nil, nil)
	if origin.Code != http.StatusServiceUnavailable || demoErrorCode(t, origin) != "demo_unavailable" || len(calls) != 0 {
		t.Fatalf("disabled origin: %d %s", origin.Code, origin.Body.String())
	}
	anonymous := httptest.NewRequest(http.MethodGet, "https://ovdb-demo-session-demo-000001.openvaultdb.com/v1/databases/demo-sneat-space/records/lists/x", nil)
	anonymous.Host = "ovdb-demo-session-demo-000001.openvaultdb.com"
	refused := httptest.NewRecorder()
	disabled.ServeHTTP(refused, anonymous)
	if refused.Code != http.StatusForbidden || len(calls) != 0 {
		t.Fatalf("missing edge secret: %d", refused.Code)
	}
	enabled := newDemoGateway(t, demoConfig(), transport)
	created := demoGatewayDo(enabled, http.MethodPost, cloudHost, "/api/demo/sessions", strings.NewReader(`{"app":"listus"}`), map[string]string{"Authorization": "Bearer device-token", "Content-Type": "application/json", "Cookie": "browser"})
	metadata := demoGatewayDo(enabled, http.MethodGet, cloudHost, "/api/demo/session?spaceId=space_1&upstream=attacker", nil, map[string]string{"Authorization": "Bearer firebase-token"})
	browser := demoGatewayDo(enabled, http.MethodGet, cloudHost, "/api/demo/session/browser?spaceId=space_1&upstream=attacker", nil, map[string]string{"Authorization": "Bearer firebase-id-token", "Origin": "https://listus.app"})
	ended := demoGatewayDo(enabled, http.MethodDelete, cloudHost, "/api/demo/sessions/session_test", nil, map[string]string{"Authorization": "Bearer device-token"})
	if created.Code != http.StatusOK || metadata.Code != http.StatusOK || browser.Code != http.StatusOK || browser.Header().Get("Access-Control-Allow-Origin") != "https://listus.app" || ended.Code != http.StatusNoContent {
		t.Fatalf("statuses %d %d %d %d", created.Code, metadata.Code, browser.Code, ended.Code)
	}
	if len(calls) != 4 {
		t.Fatalf("calls %#v", calls)
	}
	expectCall(t, calls[0], http.MethodPost, "/v0/ovdb/demo/sessions", "", "Bearer device-token")
	expectCall(t, calls[1], http.MethodGet, "/v0/ovdb/demo/session", "spaceId=space_1", "Bearer firebase-token")
	expectCall(t, calls[2], http.MethodGet, "/v0/ovdb/demo/session/browser", "spaceId=space_1", "Bearer firebase-id-token")
	expectCall(t, calls[3], http.MethodPost, "/v0/ovdb/demo/session/end", "", "Bearer device-token")
	var endBody map[string]string
	if json.Unmarshal(calls[3].body, &endBody) != nil || endBody["sessionId"] != "session_test" {
		t.Fatalf("end body %s", calls[3].body)
	}
	preflight := demoGatewayDo(enabled, http.MethodOptions, cloudHost, "/api/demo/session/browser?spaceId=space_1", nil, map[string]string{"Origin": "https://listus.app", "Access-Control-Request-Method": http.MethodGet})
	if preflight.Code != http.StatusNoContent || preflight.Header().Get("Access-Control-Allow-Origin") != "https://listus.app" || len(calls) != 4 {
		t.Fatalf("preflight %d calls=%d", preflight.Code, len(calls))
	}
	for _, path := range []string{"/users/owner_1/demo-sneat-space/ovdb/v1/databases/demo-sneat-space/%2fsecret", "/users/owner_1/demo-sneat-space/ovdb/v1/databases/demo-sneat-space/../secret"} {
		rec := demoGatewayDo(enabled, http.MethodGet, cloudHost, path, nil, nil)
		if rec.Code != http.StatusBadRequest || len(calls) != 4 {
			t.Fatalf("gateway rejected %s as %d", path, rec.Code)
		}
	}
}

func TestDemoGatewayInternalPutDoesNotRequireTheRetiredWorker(t *testing.T) {
	handler := newDemoGateway(t, demoConfig(), demoRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("internal put contacted an upstream")
		return nil, context.Canceled
	}))
	body, _ := json.Marshal(demoSession("session-demo-000001", "", time.Now().Add(30*time.Minute)))
	rec := demoGatewayDo(handler, http.MethodPut, cloudHost, "/internal/demo/sessions/session-demo-000001", bytes.NewReader(body), controlHeaders())
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	follow := demoGatewayDo(handler, http.MethodGet, cloudHost, demoStablePath, nil, map[string]string{"Authorization": "Bearer invalid"})
	if follow.Code != http.StatusUnauthorized {
		t.Fatalf("stored session was not visible: %d %s", follow.Code, follow.Body.String())
	}
}

type capturedDemoRequest struct {
	method, path, rawQuery, auth, cookie, proxy, origin string
	body                                                []byte
}

func expectCall(t *testing.T, got capturedDemoRequest, method, path, rawQuery, auth string) {
	t.Helper()
	if got.method != method || got.path != path || got.rawQuery != rawQuery || got.auth != auth || got.cookie != "" || got.proxy != "separate-proxy-credential" || got.origin != "" {
		t.Fatalf("upstream %+v", got)
	}
}

func newDemoGateway(t *testing.T, extra map[string]string, transport http.RoundTripper) http.Handler {
	t.Helper()
	handler, err := newGateway(http.NotFoundHandler(), func(name string) string {
		switch name {
		case originSecretEnv:
			return gatewayTestSecret
		case "SNEAT_API_ORIGIN":
			return "https://sneat.example"
		case "OVDB_DEVICE_AUTH_PROXY_SECRET":
			return "separate-proxy-credential"
		default:
			return extra[name]
		}
	}, transport)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func demoGatewayDo(handler http.Handler, method, host, path string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "https://"+host+path, body)
	req.Host = host
	req.Header.Set(defaultOriginSecretHeader, gatewayTestSecret)
	req.Header.Set("CF-Connecting-IP", "192.0.2.10")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func demoRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func demoClaims(override map[string]any) map[string]any {
	claims := map[string]any{
		"sub":       "owner_1",
		"aud":       "sneat-eur3-1",
		"iss":       "https://securetoken.google.com/sneat-eur3-1",
		"exp":       demoFixedNow.Add(time.Minute).Unix(),
		"iat":       demoFixedNow.Add(-time.Minute).Unix(),
		"auth_time": demoFixedNow.Add(-time.Minute).Unix(),
	}
	for name, value := range override {
		claims[name] = value
	}
	return claims
}

func signDemoToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any, kid string) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": kid})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func demoJWKSResponse(t *testing.T, public rsa.PublicKey, kid, cache string, empty ...bool) *http.Response {
	t.Helper()
	keys := []any{}
	if len(empty) == 0 || !empty[0] {
		keys = append(keys, map[string]string{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid,
			"n": base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(public.E)).Bytes()),
		})
	}
	body, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{"Content-Type": []string{"application/json"}}
	if cache != "" {
		header.Set("Cache-Control", cache)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}
