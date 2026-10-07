package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
)

const ecbQueryPath = "/v1/databases/ecb/dtql"
const ecbProxySecretHeader = "X-OVDB-ECB-Proxy-Secret"

func validECBProxySecret(value string) bool {
	if len(value) < 32 || len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		allowed := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-'
		if !allowed {
			return false
		}
	}
	return true
}

// The mount seam lets the selected-path test use invented XML and an in-memory
// transport. Production always uses the library's normal pinned HTTP mount.
var selectedECBMount ecbMount = mount.File
var selectedECBDiagnostic io.Writer = os.Stderr

func configuredHandlerWithRuntime(startup runtimeStartup) ([]runtimeDatabase, http.Handler, func() error, error) {
	enabled := os.Getenv("OVDB_ECB_ENABLED")
	if enabled != "" && enabled != "false" && enabled != "true" {
		return nil, nil, nil, errECBCandidate
	}
	var ecbHandler http.Handler
	var closeECB func() error
	var proxySecret string
	var proxySecretHash [sha256.Size]byte
	if enabled == "true" {
		configPath := os.Getenv("OVDB_ECB_HOST_CONFIG")
		configDigest, err := checkedECBAdmission(os.Getenv("OVDB_ECB_ADMISSION_FILE"),
			os.Getenv("OVDB_ECB_ADMISSION_SHA256"), configPath)
		if err != nil {
			return nil, nil, nil, errECBCandidate
		}
		proxySecret = os.Getenv("OVDB_ECB_PROXY_SECRET")
		if !validECBProxySecret(proxySecret) {
			return nil, nil, nil, errECBCandidate
		}
		proxySecretHash = sha256.Sum256([]byte(proxySecret))
		ecbHandler, closeECB, err = assembleECBHostCandidate(configPath, selectedECBMount, selectedECBDiagnostic, configDigest)
		if err != nil {
			return nil, nil, nil, errECBCandidate
		}
	}
	samples, closeSamples, err := newHandlerWithStorage(startup.providers, startup.strategy, startup.image)
	if err != nil {
		if closeECB != nil {
			_ = closeECB()
		}
		return nil, nil, nil, err
	}
	if ecbHandler == nil {
		return startup.providers, samples, closeSamples, nil
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != ecbQueryPath {
			samples.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		secrets := r.Header.Values(ecbProxySecretHeader)
		if len(secrets) != 1 {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		providedHash := sha256.Sum256([]byte(secrets[0]))
		if subtle.ConstantTimeCompare(providedHash[:], proxySecretHash[:]) != 1 {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.URL.ForceQuery {
			http.Error(w, "ECB query request refused", http.StatusMethodNotAllowed)
			return
		}
		ecbHandler.ServeHTTP(w, r)
	})
	return startup.providers, handler, func() error {
		var ecbErr error
		if closeECB() != nil {
			ecbErr = errECBCandidate
		}
		return errors.Join(ecbErr, closeSamples())
	}, nil
}

// net/http logs panic values and stack traces by default. The selected host
// must never send provider text to that sink, even when a handler panics.
type fixedHTTPErrorWriter struct{ out io.Writer }

func (w fixedHTTPErrorWriter) Write(p []byte) (int, error) {
	_, err := io.WriteString(w.out, "ovdb_http_internal_error\n")
	return len(p), err
}

func safeHTTPErrorLog(out io.Writer) *log.Logger {
	return log.New(fixedHTTPErrorWriter{out: out}, "", 0)
}
