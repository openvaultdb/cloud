package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
)

const ianaQueryPath = "/v1/databases/iana-http-status/dtql"
const ianaProxySecretHeader = "X-OVDB-IANA-Proxy-Secret"

// Production mounts only the fixed checked manifest. Tests inject invented CSV.
var selectedIANAMount ecbMount = mount.File
var selectedIANADiagnostic io.Writer = os.Stderr

func configuredHandlerWithRuntime(startup runtimeStartup) ([]runtimeDatabase, http.Handler, func() error, error) {
	enabled := os.Getenv("OVDB_IANA_ENABLED")
	if enabled != "" && enabled != "false" && enabled != "true" {
		return nil, nil, nil, errIANAConfig
	}
	if enabled != "true" {
		return configuredHandlerWithECB(startup)
	}
	configPath := os.Getenv("OVDB_IANA_HOST_CONFIG")
	admission, err := checkedIANAAdmission(os.Getenv("OVDB_IANA_ADMISSION_FILE"), os.Getenv("OVDB_IANA_ADMISSION_SHA256"))
	if err != nil {
		return nil, nil, nil, errIANAConfig
	}
	secret := os.Getenv("OVDB_IANA_PROXY_SECRET")
	if !validECBProxySecret(secret) {
		return nil, nil, nil, errIANAConfig
	}
	secretHash := sha256.Sum256([]byte(secret))
	iana, closeIANA, err := assembleIANAHost(configPath, admission, selectedIANAMount, selectedIANADiagnostic)
	if err != nil {
		return nil, nil, nil, errIANAConfig
	}
	providers, existing, closeExisting, err := configuredHandlerWithECB(startup)
	if err != nil {
		_ = closeIANA()
		return nil, nil, nil, err
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != ianaQueryPath {
			existing.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		secrets := r.Header.Values(ianaProxySecretHeader)
		if len(secrets) != 1 {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		presented := sha256.Sum256([]byte(secrets[0]))
		if subtle.ConstantTimeCompare(presented[:], secretHash[:]) != 1 {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.URL.ForceQuery {
			http.Error(w, "IANA query request refused", http.StatusMethodNotAllowed)
			return
		}
		iana.ServeHTTP(w, r)
	})
	return providers, handler, func() error { return errors.Join(closeIANA(), closeExisting()) }, nil
}
