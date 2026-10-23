package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The legacy dtql route stays reachable for deployed browser clients until
// TugQL cutover step 9 removes legacyDTQLSegment and everything built on it.

func TestLegacyQueryPathHelpers(t *testing.T) {
	for _, p := range []string{"/v1/tugql", "/v1/tugql/", "/v1/" + legacyDTQLSegment, "/v1/" + legacyDTQLSegment + "/"} {
		if !isCrossDatabaseQueryPath(p) {
			t.Errorf("%s not a cross-database query path", p)
		}
	}
	if isCrossDatabaseQueryPath("/v1/query") {
		t.Error("/v1/query is not a cross-database query path")
	}
	if !isECBQueryPath(ecbQueryPath) || !isECBQueryPath(legacyECBQueryPath) || isECBQueryPath("/v1/databases/ecb/query") {
		t.Error("ECB query path set mismatch")
	}
	if !isIANAQueryPath(ianaQueryPath) || !isIANAQueryPath(legacyIANAQueryPath) || isIANAQueryPath("/v1/databases/iana-http-status/query") {
		t.Error("IANA query path set mismatch")
	}
	if legacyECBQueryPath != "/v1/databases/ecb/dtql" || legacyIANAQueryPath != "/v1/databases/iana-http-status/dtql" || legacyECBPublicPath != "/ecb-public/v1/databases/ecb/dtql" {
		t.Error("legacy paths changed: deployed admissions and Workers still name them")
	}
}

func TestGatewayServesLegacyQueryRouteAndRebrandsBothEndpoints(t *testing.T) {
	var paths []string
	h := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/databases/chinook" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "chinook", "endpoints": map[string]string{
				"tugql":           "https://" + cloudHost + "/v1/databases/chinook/tugql",
				legacyDTQLSegment: "https://" + cloudHost + "/v1/databases/chinook/" + legacyDTQLSegment,
			}})
			return
		}
		w.WriteHeader(200)
	}), nil)
	w := gatewayRequest(t, h, "GET", demoHost, "/v1/databases/chinook", nil)
	for _, want := range []string{"https://" + demoHost + "/v1/databases/chinook/tugql", "https://" + demoHost + "/v1/databases/chinook/" + legacyDTQLSegment} {
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) || strings.Contains(w.Body.String(), cloudHost) {
			t.Fatalf("metadata not rebranded to %s: %d %s", want, w.Code, w.Body.String())
		}
	}
	for _, p := range []string{"/v1/databases/chinook/" + legacyDTQLSegment, "/v1/databases/chinook/tugql"} {
		if got := gatewayRequest(t, h, "POST", demoHost, p, strings.NewReader("{}")).Code; got != 200 {
			t.Errorf("%s: %d", p, got)
		}
	}
	if got := gatewayRequest(t, h, "POST", cloudHost, "/v1/"+legacyDTQLSegment, strings.NewReader("{}")).Code; got != 200 {
		t.Errorf("cross-database legacy route: %d", got)
	}
}

func TestGatewayMetadataLegacyEndpointOptionalButChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		endpoints map[string]string
		code      int
	}{
		"legacy absent":  {map[string]string{"tugql": "https://" + cloudHost + "/v1/databases/chinook/tugql"}, 200},
		"legacy foreign": {map[string]string{"tugql": "https://" + cloudHost + "/v1/databases/chinook/tugql", legacyDTQLSegment: "https://attacker.example"}, 502},
		"tugql missing":  {map[string]string{legacyDTQLSegment: "https://" + cloudHost + "/v1/databases/chinook/" + legacyDTQLSegment}, 502},
	} {
		h := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "chinook", "endpoints": tc.endpoints})
		}), nil)
		w := gatewayRequest(t, h, "GET", demoHost, "/v1/databases/chinook", nil)
		if w.Code != tc.code || strings.Contains(w.Body.String(), "attacker") {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
}

func TestSelectedECBAcceptsLegacyAdmissionAndRoute(t *testing.T) {
	reads := 0
	configureSyntheticECB(t, syntheticECBMount(t, &reads))
	file := os.Getenv("OVDB_ECB_ADMISSION_FILE")
	raw, err := os.ReadFile(file)
	if err != nil || !bytes.Contains(raw, []byte(ecbQueryPath)) {
		t.Fatal("synthetic admission does not name the current path", err)
	}
	raw = bytes.Replace(raw, []byte(ecbQueryPath), []byte(legacyECBQueryPath), 1)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OVDB_ECB_ADMISSION_SHA256", fmt.Sprintf("%x", sha256.Sum256(raw)))
	t.Setenv("OVDB_ECB_ENABLED", "true")
	_, handler, closeHandler, err := configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeHandler() })
	r := httptest.NewRequest(http.MethodPost, legacyECBQueryPath, strings.NewReader("from: {name: daily}\nlimit: 1\n"))
	r.Header.Set(ecbProxySecretHeader, syntheticECBProxySecret)
	r.Header.Set("OVDB-Execution-ID", strings.Repeat("a", 32))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || reads != 1 {
		t.Fatalf("legacy ECB route: status=%d reads=%d", w.Code, reads)
	}
}

func TestIANAAdmissionAcceptsLegacyPath(t *testing.T) {
	_, _, admission := configureSyntheticIANA(t, syntheticIANAMount(t, probeTransport(func(r *http.Request) (*http.Response, error) {
		t.Fatal("admission check read the provider")
		return nil, nil
	})))
	admission.Path = legacyIANAQueryPath
	path := filepath.Join(t.TempDir(), "legacy-admission.json")
	digest := writeIANAJSON(t, path, admission)
	if got, err := checkedIANAAdmission(path, digest); err != nil || got.Path != legacyIANAQueryPath {
		t.Fatalf("legacy IANA admission refused: %v", err)
	}
	admission.Path = "/v1/databases/iana-http-status/query"
	digest = writeIANAJSON(t, path, admission)
	if _, err := checkedIANAAdmission(path, digest); err == nil {
		t.Fatal("unrelated path admitted")
	}
}

func TestECBPublicAdmissionAcceptsLegacyWorkerAndGoPaths(t *testing.T) {
	reads := 0
	_, _, _ = configureSyntheticECBPublic(t, &reads)
	file := os.Getenv("OVDB_ECB_PUBLIC_ADMISSION_FILE")
	raw, err := os.ReadFile(file)
	if err != nil || !bytes.Contains(raw, []byte(ecbPublicPath)) || !bytes.Contains(raw, []byte(ecbQueryPath)) {
		t.Fatal("synthetic admission does not name the current paths", err)
	}
	raw = bytes.Replace(raw, []byte(ecbPublicPath), []byte(legacyECBPublicPath), 1)
	raw = bytes.Replace(raw, []byte(ecbQueryPath), []byte(legacyECBQueryPath), 1)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	if a, _, err := checkedECBPublicAdmission(file, digest, os.Getenv("OVDB_ECB_PUBLIC_HOST_CONFIG")); err != nil || a.WorkerPath != legacyECBPublicPath || a.GoPath != legacyECBQueryPath {
		t.Fatalf("legacy public admission refused: %v", err)
	}
}
