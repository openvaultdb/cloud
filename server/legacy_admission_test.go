package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Existing digest-pinned operator admissions still name the pre-rename path in
// their path fields. They stay valid; requests are served on /tugql only.

func TestAdmissionPathSets(t *testing.T) {
	if !isECBAdmissionPath(ecbQueryPath) || !isECBAdmissionPath(legacyECBQueryPath) || isECBAdmissionPath("/v1/databases/ecb/query") {
		t.Error("ECB admission path set mismatch")
	}
	if !isIANAAdmissionPath(ianaQueryPath) || !isIANAAdmissionPath(legacyIANAQueryPath) || isIANAAdmissionPath("/v1/databases/iana-http-status/query") {
		t.Error("IANA admission path set mismatch")
	}
	if legacyECBQueryPath != "/v1/databases/ecb/dtql" || legacyIANAQueryPath != "/v1/databases/iana-http-status/dtql" || legacyECBPublicPath != "/ecb-public/v1/databases/ecb/dtql" {
		t.Error("legacy admission paths changed: deployed admissions still name them")
	}
}

func TestSelectedECBLegacyAdmissionServesTugQLOnly(t *testing.T) {
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
	for path, want := range map[string]int{ecbQueryPath: http.StatusOK, legacyECBQueryPath: -1} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("from: {name: daily}\nlimit: 1\n"))
		r.Header.Set(ecbProxySecretHeader, syntheticECBProxySecret)
		r.Header.Set("OVDB-Execution-ID", strings.Repeat("a", 32))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if (want > 0 && w.Code != want) || (want < 0 && w.Code == http.StatusOK) {
			t.Errorf("%s: status %d", path, w.Code)
		}
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
