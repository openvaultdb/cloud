package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const syntheticPublicSecret = "synthetic-public-proxy-key-32-bytes-long"
const syntheticPublicOrigin = "https://synthetic.directory.invalid"

func configureSyntheticECBPublic(t *testing.T, reads *int) (ecbPublicAdmission, ecbHostConfig, string) {
	t.Helper()
	inventory, _ := selectedInventoryFixture(t)
	t.Setenv("SAMPLE_DATABASES_INVENTORY", inventory)
	t.Setenv("OVDB_SELECTED_STORAGE", string(sealedCopy))
	t.Setenv("OVDB_ECB_ENABLED", "")
	config, path := ecbCandidateFixture(t)
	config.DecoderModule = "v0.4.0"
	writeECBConfig(t, path, config)
	raw, _ := os.ReadFile(path)
	a := ecbPublicAdmission{Format: "ovdb-ecb-public-free-admission/1", Decision: "public-free-transient-read-only", ApprovedBy: "synthetic-review", ApprovedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), CostOwner: "synthetic-cost-owner", HostConfigSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), PublisherManifestSHA256: ecbPublisherSHA, DecoderModuleVersion: "v0.4.0", DecoderSHA256: ecbDecoderSHA, RightsDigest: ecbRightsSHA, RequestProfile: "ecb-public-free/1", WorkerPath: ecbPublicPath, GoPath: ecbQueryPath, DirectoryOrigin: syntheticPublicOrigin, BackendOrigin: "https://synthetic-ecb.a.run.app", Audience: "public-free", MaxReads: 1, MaxRows: 50, MaxConcurrent: 1, ExecutionsPerMinute: 6}
	encoded, _ := json.Marshal(a)
	admissionPath := filepath.Join(t.TempDir(), "public-admission.json")
	if err := os.WriteFile(admissionPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(encoded))
	t.Setenv("OVDB_ECB_PUBLIC_ENABLED", "true")
	t.Setenv("OVDB_ECB_PUBLIC_HOST_CONFIG", path)
	t.Setenv("OVDB_ECB_PUBLIC_ADMISSION_FILE", admissionPath)
	t.Setenv("OVDB_ECB_PUBLIC_ADMISSION_SHA256", digest)
	t.Setenv("OVDB_ECB_PUBLIC_PROXY_SECRET", syntheticPublicSecret)
	previous := selectedECBPublicMount
	selectedECBPublicMount = syntheticECBMount(t, reads)
	t.Cleanup(func() { selectedECBPublicMount = previous })
	return a, config, digest
}
func publicRequest(handler http.Handler, digest, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", ecbQueryPath, strings.NewReader(query))
	r.Header.Set("Content-Type", "application/yaml")
	r.Header.Set("Origin", syntheticPublicOrigin)
	r.Header.Set("OVDB-Execution-ID", strings.Repeat("a", 32))
	r.Header.Set(ecbPublicSecretHeader, syntheticPublicSecret)
	r.Header.Set(ecbPublicAdmissionHeader, digest)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

const publicNativeQuery = "from: {name: daily}\ncolumns: [{field: time}, {field: currency}, {field: rate}]\nlimit: 1\n"

func TestECBPublicSelectedCompleteEvidence(t *testing.T) {
	reads := 0
	_, _, digest := configureSyntheticECBPublic(t, &reads)
	_, h, closeH, err := configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeH() }()
	if reads != 0 {
		t.Fatal("passive read")
	}
	w := publicRequest(h, digest, publicNativeQuery)
	if w.Code != 200 || reads != 1 {
		t.Fatalf("status=%d reads=%d body=%s", w.Code, reads, w.Body)
	}
	proof := ecbCompletionProof(syntheticPublicSecret, digest, strings.Repeat("a", 32), 200, "application/json", w.Body.Bytes())
	if w.Header().Get(ecbPublicProofHeader) != proof || !strings.Contains(w.Body.String(), "001.23000") {
		t.Fatal("missing authenticated lexical output")
	}
	empty := publicNativeQuery + "where: {op: '==', left: {field: currency}, right: {value: ZZZ}}\n"
	w = publicRequest(h, digest, empty)
	if w.Code != 200 || reads != 2 || !strings.Contains(w.Body.String(), `"records":[]`) || !strings.Contains(w.Body.String(), `"providerReads"`) {
		t.Fatalf("empty evidence: %d %s", w.Code, w.Body)
	}
}
func TestECBPublicRefusalBeforeReads(t *testing.T) {
	for name, mutate := range map[string]func(*http.Request){"operator": func(r *http.Request) { r.Header.Set(ecbProxySecretHeader, syntheticPublicSecret) }, "paid": func(r *http.Request) { r.Header.Set("X-Paid-Capability", "marker") }, "auth": func(r *http.Request) { r.Header.Set("Authorization", "marker") }, "wrong secret": func(r *http.Request) { r.Header.Set(ecbPublicSecretHeader, "marker") }, "duplicate secret": func(r *http.Request) { r.Header.Add(ecbPublicSecretHeader, syntheticPublicSecret) }, "wrong id": func(r *http.Request) { r.Header.Set("OVDB-Execution-ID", "marker") }, "origin": func(r *http.Request) { r.Header.Set("Origin", "null") }, "query": func(r *http.Request) { r.URL.ForceQuery = true }} {
		t.Run(name, func(t *testing.T) {
			reads := 0
			_, _, digest := configureSyntheticECBPublic(t, &reads)
			_, h, closeH, err := configuredHandler()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = closeH() }()
			r := httptest.NewRequest("POST", ecbQueryPath, strings.NewReader(publicNativeQuery))
			r.Header = http.Header{"Content-Type": {"application/yaml"}, "Origin": {syntheticPublicOrigin}, "OVDB-Execution-ID": {strings.Repeat("a", 32)}, ecbPublicSecretHeader: {syntheticPublicSecret}, ecbPublicAdmissionHeader: {digest}}
			mutate(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code == 200 || reads != 0 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d reads=%d", w.Code, reads)
			}
		})
	}
	reads := 0
	_, _, digest := configureSyntheticECBPublic(t, &reads)
	_, h, closeH, err := configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeH() }()
	for _, query := range []string{strings.Replace(publicNativeQuery, "limit: 1", "limit: 51", 1), publicNativeQuery + "orderBy: [{field: rate}]\n", publicNativeQuery + "where: {op: '<', left: {field: rate}, right: {value: '2'}}\n", publicNativeQuery + "limit: 1\n", strings.Repeat("x", ecbPublicRequestBytes+1)} {
		w := publicRequest(h, digest, query)
		if w.Code == 200 || reads != 0 {
			t.Fatalf("unsupported query read: %d %d", w.Code, reads)
		}
	}
}
func TestECBPublicAdmissionIsolation(t *testing.T) {
	for _, mode := range []string{"operator simultaneous", "paid", "missing", "expired", "wrong config", "duplicate", "bad flag"} {
		t.Run(mode, func(t *testing.T) {
			reads := 0
			_, _, _ = configureSyntheticECBPublic(t, &reads)
			switch mode {
			case "operator simultaneous":
				t.Setenv("OVDB_ECB_ENABLED", "true")
			case "missing":
				t.Setenv("OVDB_ECB_PUBLIC_ADMISSION_FILE", "")
			case "bad flag":
				t.Setenv("OVDB_ECB_PUBLIC_ENABLED", "TRUE")
			default:
				path := os.Getenv("OVDB_ECB_PUBLIC_ADMISSION_FILE")
				raw, _ := os.ReadFile(path)
				s := string(raw)
				switch mode {
				case "paid":
					s = strings.Replace(s, `"paidAccess":false`, `"paidAccess":true`, 1)
				case "expired":
					s = strings.Replace(s, `"expiresAt":"`, `"expiresAt":"2000-01-01T00:00:00Z","discarded":"`, 1)
				case "wrong config":
					s = strings.Replace(s, `"hostConfigSHA256":"`, `"hostConfigSHA256":"0`, 1)
				case "duplicate":
					s = strings.Replace(s, `"paidAccess":false`, `"paidAccess":false,"paidAccess":false`, 1)
				}
				_ = os.WriteFile(path, []byte(s), 0600)
				t.Setenv("OVDB_ECB_PUBLIC_ADMISSION_SHA256", fmt.Sprintf("%x", sha256.Sum256([]byte(s))))
			}
			_, h, closeH, err := configuredHandler()
			if err == nil || h != nil || closeH != nil || reads != 0 {
				t.Fatal("inadmissible public config accepted")
			}
		})
	}
}
func TestECBPublicRowsThenInvalidNeverRelease(t *testing.T) {
	reads := 0
	a, c, digest := configureSyntheticECBPublic(t, &reads)
	candidate, closeH, err := assembleECBHostVersion(os.Getenv("OVDB_ECB_PUBLIC_HOST_CONFIG"), selectedECBPublicMount, io.Discard, true, a.HostConfigSHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeH() }()
	good := publicRequest(newECBPublicGate(candidate, a, c, digest, syntheticPublicSecret), digest, publicNativeQuery)
	if good.Code != 200 {
		t.Fatalf("valid control: %s", good.Body)
	}
	raw := good.Body.Bytes()
	for name, body := range map[string][]byte{"missing footer": []byte(`{"records":[{"key":"marker","data":{"rate":"MARKER"}}]}`), "partial": []byte(`{"records":[{"data":{"rate":"MARKER"}}`), "failure": bytes.Replace(raw, []byte(`"complete":true`), []byte(`"complete":false`), 1), "rights": bytes.Replace(raw, []byte(ecbRightsSHA), []byte(strings.Repeat("0", 64)), 1), "observation": bytes.Replace(raw, []byte(`"resourceId":"ecb-daily"`), []byte(`"resourceId":"wrong"`), 1), "duplicate": bytes.Replace(raw, []byte(`"complete":true`), []byte(`"complete":true,"complete":true`), 1), "trailing": append(append([]byte{}, raw...), []byte(`{}`)...), "large": []byte(strings.Repeat("x", ecbPublicResponseBytes+1))} {
		t.Run(name, func(t *testing.T) {
			fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = w.Write(body[:len(body)/2])
				w.(http.Flusher).Flush()
				_, _ = w.Write(body[len(body)/2:])
			})
			w := publicRequest(newECBPublicGate(fake, a, c, digest, syntheticPublicSecret), digest, publicNativeQuery)
			if w.Code == 200 || w.Header().Get(ecbPublicProofHeader) != "" || strings.Contains(w.Body.String(), "MARKER") || strings.Contains(w.Body.String(), "records") || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("bad footer escaped: %d %s", w.Code, w.Body)
			}
		})
	}
	cancelled := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
		panic(http.ErrAbortHandler)
	})
	w := publicRequest(newECBPublicGate(cancelled, a, c, digest, syntheticPublicSecret), digest, publicNativeQuery)
	if w.Code == 200 {
		t.Fatal("abort released body")
	}
}

func TestECBPublicCancellationAndServiceBudget(t *testing.T) {
	reads := 0
	a, c, digest := configureSyntheticECBPublic(t, &reads)
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"records":[{"data":{"rate":"MARKER"}}]`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	gate := newECBPublicGate(fake, a, c, digest, syntheticPublicSecret)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("POST", ecbQueryPath, strings.NewReader(publicNativeQuery)).WithContext(ctx)
	r.Header = http.Header{"Content-Type": {"application/yaml"}, "Origin": {syntheticPublicOrigin}, "OVDB-Execution-ID": {strings.Repeat("a", 32)}, ecbPublicSecretHeader: {syntheticPublicSecret}, ecbPublicAdmissionHeader: {digest}}
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, r)
	if w.Code == 200 || strings.Contains(w.Body.String(), "MARKER") || gate.active {
		t.Fatal("cancel failed to dispose gate")
	}
	gate.count = 6
	gate.minute = time.Now()
	w = publicRequest(gate, digest, publicNativeQuery)
	if w.Code != 429 {
		t.Fatal("service rate budget missing")
	}
	gate.count = 0
	gate.active = true
	w = publicRequest(gate, digest, publicNativeQuery)
	if w.Code != 429 {
		t.Fatal("service concurrency budget missing")
	}
}
