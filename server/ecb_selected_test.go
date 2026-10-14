package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

const syntheticECBProxySecret = "synthetic-ecb-proxy-secret-32-bytes-long"

func configureSyntheticECB(t *testing.T, mount ecbMount) string {
	t.Helper()
	inventory, _ := selectedInventoryFixture(t)
	t.Setenv("SAMPLE_DATABASES_INVENTORY", inventory)
	t.Setenv("OVDB_SELECTED_STORAGE", string(sealedCopy))
	config, path := ecbCandidateFixture(t)
	writeECBConfig(t, path, config)
	t.Setenv("OVDB_ECB_HOST_CONFIG", path)
	configBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	admission := ecbAdmission{
		Format: "ovdb-ecb-b1-operator-admission/1", Decision: "operator-free-transient-read-only",
		ApprovedBy: "synthetic-test-operator", ApprovedAt: "2026-10-07T00:00:00Z",
		HostConfigSHA256:        fmt.Sprintf("%x", sha256.Sum256(configBytes)),
		PublisherManifestSHA256: ecbPublisherSHA, RightsDigest: ecbRightsSHA,
		ExecutorID: "openvaultdb-cloud", ResourceID: "ecb-daily", Method: http.MethodPost,
		Path: ecbQueryPath, MaxReadsPerExecution: 1, MaxRows: 50,
	}
	admissionBytes, err := json.Marshal(admission)
	if err != nil {
		t.Fatal(err)
	}
	admissionPath := filepath.Join(t.TempDir(), "operator-admission.json")
	if err := os.WriteFile(admissionPath, admissionBytes, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OVDB_ECB_ADMISSION_FILE", admissionPath)
	t.Setenv("OVDB_ECB_ADMISSION_SHA256", fmt.Sprintf("%x", sha256.Sum256(admissionBytes)))
	t.Setenv("OVDB_ECB_PROXY_SECRET", syntheticECBProxySecret)
	previous := selectedECBMount
	selectedECBMount = mount
	t.Cleanup(func() { selectedECBMount = previous })
	return path
}

func selectedECBRequest(h http.Handler, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, ecbQueryPath, strings.NewReader(query))
	r.Header.Set(ecbProxySecretHeader, syntheticECBProxySecret)
	r.Header.Set("OVDB-Execution-ID", strings.Repeat("a", 32))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestConfiguredHandlerECBSelectedQuery(t *testing.T) {
	reads := 0
	configureSyntheticECB(t, syntheticECBMount(t, &reads))
	t.Setenv("OVDB_ECB_ENABLED", "")
	_, defaultHandler, closeDefault, err := configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	if w := ecbRequest(defaultHandler, "from: {name: daily}\nlimit: 1\n"); w.Code == http.StatusOK || reads != 0 {
		t.Fatal("candidate file alone activated provider")
	}
	_ = closeDefault()
	t.Setenv("OVDB_ECB_ENABLED", "true")
	_, handler, closeHandler, err := configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeHandler() })
	if reads != 0 {
		t.Fatal("selected startup made a passive provider read")
	}
	for _, secret := range []string{"", "wrong", syntheticECBProxySecret + ",wrong"} {
		r := httptest.NewRequest(http.MethodPost, ecbQueryPath, strings.NewReader("from: {name: daily}\nlimit: 1\n"))
		if secret != "" {
			r.Header.Set(ecbProxySecretHeader, secret)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound || reads != 0 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("direct Go request without trusted Worker secret reached ECB: status=%d reads=%d", w.Code, reads)
		}
	}
	duplicate := httptest.NewRequest(http.MethodPost, ecbQueryPath, nil)
	duplicate.Header.Add(ecbProxySecretHeader, syntheticECBProxySecret)
	duplicate.Header.Add(ecbProxySecretHeader, syntheticECBProxySecret)
	refused := httptest.NewRecorder()
	handler.ServeHTTP(refused, duplicate)
	if refused.Code != http.StatusNotFound || reads != 0 {
		t.Fatal("duplicate proxy secret reached ECB")
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, ecbQueryPath, nil),
		httptest.NewRequest(http.MethodPost, ecbQueryPath+"?q=marker", nil),
		httptest.NewRequest(http.MethodPost, "/v1/databases/%65cb/dtql", nil),
		httptest.NewRequest(http.MethodPost, "/v1/databases/ecb/records/daily/USD", nil),
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, request)
		if w.Code == http.StatusOK || reads != 0 {
			t.Fatalf("passive or alternate ECB route read provider: %s %s", request.Method, request.URL)
		}
	}
	query := "from: {name: daily}\ncolumns: [{field: time}, {field: currency}, {field: rate}]\nlimit: 1\n"
	w := selectedECBRequest(handler, query)
	if w.Code != http.StatusOK || reads != 1 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "001.23000") {
		t.Fatalf("selected query: status=%d reads=%d cache=%q", w.Code, reads, w.Header().Get("Cache-Control"))
	}
	w = selectedECBRequest(handler, query)
	if w.Code != http.StatusOK || reads != 2 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("repeat query reused a retained result: status=%d reads=%d", w.Code, reads)
	}
	for _, query := range []string{
		"from: {name: daily}\nlimit: 51\n",
		"from: {name: daily}\norderBy: [{field: rate}]\nlimit: 1\n",
	} {
		w = selectedECBRequest(handler, query)
		if w.Code == http.StatusOK || reads != 2 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unbounded query reached provider: status=%d reads=%d", w.Code, reads)
		}
	}
	sample := httptest.NewRecorder()
	handler.ServeHTTP(sample, httptest.NewRequest(http.MethodGet, "/v1/databases/final", nil))
	if sample.Code != http.StatusOK || reads != 2 {
		t.Fatal("selected ECB changed sample routing")
	}
}

func TestConfiguredHandlerECBRefusesDriftBeforeMount(t *testing.T) {
	mounts := 0
	path := configureSyntheticECB(t, func(string) (*core.Database, error) { mounts++; return nil, errors.New("secret-marker") })
	t.Setenv("OVDB_ECB_ENABLED", "true")
	t.Setenv("OVDB_ECB_PROXY_SECRET", "short")
	if _, handler, closeHandler, err := configuredHandler(); err == nil || handler != nil || closeHandler != nil || mounts != 0 {
		t.Fatal("short proxy secret crossed selected startup")
	}
	t.Setenv("OVDB_ECB_PROXY_SECRET", syntheticECBProxySecret)
	if err := os.WriteFile(path, []byte(`{"format":"wrong"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, handler, closeHandler, err := configuredHandler()
	if err == nil || handler != nil || closeHandler != nil || mounts != 0 || strings.Contains(err.Error(), "secret-marker") {
		t.Fatalf("drift crossed mount: mounts=%d err=%v", mounts, err)
	}
}

func TestConfiguredHandlerECBRequiresExactOperatorAdmissionBeforeMount(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, string){
		"missing admission":      func(t *testing.T, _ string) { t.Setenv("OVDB_ECB_ADMISSION_FILE", "") },
		"wrong admission digest": func(t *testing.T, _ string) { t.Setenv("OVDB_ECB_ADMISSION_SHA256", strings.Repeat("0", 64)) },
		"publisher proposal": func(t *testing.T, _ string) {
			t.Setenv("OVDB_ECB_ADMISSION_FILE", "testdata/ecb/accepted-b1-proposal.json")
		},
		"host config drift": func(t *testing.T, _ string) {
			path := os.Getenv("OVDB_ECB_HOST_CONFIG")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"public access": func(t *testing.T, path string) {
			changeECBAdmission(t, path, `"publicAccess":false`, `"publicAccess":true`)
		},
		"paid access": func(t *testing.T, path string) {
			changeECBAdmission(t, path, `"paidAccess":false`, `"paidAccess":true`)
		},
		"extra read": func(t *testing.T, path string) {
			changeECBAdmission(t, path, `"maxReadsPerExecution":1`, `"maxReadsPerExecution":2`)
		},
		"extra rows": func(t *testing.T, path string) { changeECBAdmission(t, path, `"maxRows":50`, `"maxRows":51`) },
		"wrong executor": func(t *testing.T, path string) {
			changeECBAdmission(t, path, `"executorId":"openvaultdb-cloud"`, `"executorId":"other"`)
		},
		"duplicate member": func(t *testing.T, path string) {
			changeECBAdmission(t, path, `"publicAccess":false`, `"publicAccess":false,"publicAccess":false`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			mounts := 0
			configureSyntheticECB(t, func(string) (*core.Database, error) { mounts++; return nil, errors.New("mount-marker") })
			t.Setenv("OVDB_ECB_ENABLED", "true")
			mutate(t, os.Getenv("OVDB_ECB_ADMISSION_FILE"))
			_, handler, closeHandler, err := configuredHandler()
			if err == nil || handler != nil || closeHandler != nil || mounts != 0 || strings.Contains(err.Error(), "mount-marker") {
				t.Fatalf("inadmissible operator record crossed mount: mounts=%d err=%v", mounts, err)
			}
		})
	}
}

func changeECBAdmission(t *testing.T, path, old, replacement string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), old) {
		t.Fatalf("operator fixture missing member: %v", err)
	}
	changed := []byte(strings.Replace(string(data), old, replacement, 1))
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OVDB_ECB_ADMISSION_SHA256", fmt.Sprintf("%x", sha256.Sum256(changed)))
}

func TestSafeHTTPErrorLogDropsPanicMarker(t *testing.T) {
	var diagnostics bytes.Buffer
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("synthetic-private-panic-marker")
	}))
	srv.Config.ErrorLog = safeHTTPErrorLog(&diagnostics)
	srv.Start()
	defer srv.Close()
	response, err := srv.Client().Get(srv.URL)
	if err == nil {
		_ = response.Body.Close()
	}
	if got := diagnostics.String(); got != "ovdb_http_internal_error\n" {
		t.Fatalf("unsafe net/http ErrorLog: %q", got)
	}
}

func TestConfiguredHandlerECBTransportErrorMarker(t *testing.T) {
	reads := 0
	configureSyntheticECB(t, syntheticECBMountWithTransport(t, probeTransport(func(*http.Request) (*http.Response, error) {
		reads++
		return nil, errors.New("synthetic-private-error-marker")
	})))
	t.Setenv("OVDB_ECB_ENABLED", "true")
	_, handler, closeHandler, err := configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeHandler() })
	w := selectedECBRequest(handler, "from: {name: daily}\nlimit: 1\n")
	if reads != 1 || w.Code == http.StatusOK || strings.Contains(w.Body.String(), "synthetic-private-error-marker") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("selected error leaked marker: status=%d reads=%d", w.Code, reads)
	}
}
