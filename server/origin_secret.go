package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// Cloudflare overwrites the configured header on the two admitted public hosts.
// The production gateway requires this credential at startup and verifies it
// before all routing. The reusable wrapper retains its optional composition
// contract for internal handlers; newGateway enforces the public boundary.
const (
	originSecretEnv           = "OVDB_ORIGIN_SECRET"
	originSecretHeaderEnv     = "OVDB_ORIGIN_SECRET_HEADER"
	defaultOriginSecretHeader = "X-OVDB-Origin-Secret"
)

var (
	originSecretPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)
	originSecretHeaderPattern = regexp.MustCompile(`^X-[A-Za-z0-9-]{1,64}$`)
)

// withOriginSecret wraps handler with the shared-secret check configured by
// getenv. It returns handler unchanged when no secret is configured.
func withOriginSecret(handler http.Handler, getenv func(string) string) (http.Handler, error) {
	secret := getenv(originSecretEnv)
	headerName := strings.TrimSpace(getenv(originSecretHeaderEnv))
	if secret == "" {
		if headerName != "" && !originSecretHeaderPattern.MatchString(headerName) {
			return nil, errors.New(originSecretHeaderEnv + " must be an X- header name")
		}
		return handler, nil
	}
	if !originSecretPattern.MatchString(secret) {
		return nil, errors.New(originSecretEnv + " must be 32-128 characters of A-Z, a-z, 0-9, _ or -")
	}
	if headerName == "" {
		headerName = defaultOriginSecretHeader
	}
	if !originSecretHeaderPattern.MatchString(headerName) || reservedOriginSecretHeader(headerName) {
		return nil, errors.New(originSecretHeaderEnv + " must be an X- header name not used by another route")
	}
	expected := sha256.Sum256([]byte(secret))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(headerName)
		presented := ""
		if len(values) == 1 {
			presented = values[0]
		}
		actual := sha256.Sum256([]byte(presented))
		if subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"Use https://cloud.openvaultdb.com."}` + "\n"))
			return
		}
		// The library never sees the secret, so it cannot log or echo it.
		r.Header.Del(headerName)
		handler.ServeHTTP(w, r)
	}), nil
}

// reservedOriginSecretHeader reports a header another route authenticates with;
// stripping it here would break that route.
func reservedOriginSecretHeader(name string) bool {
	for _, reserved := range []string{ecbProxySecretHeader, ianaProxySecretHeader, ecbPublicSecretHeader, ecbPublicAdmissionHeader, ecbPublicProofHeader, "X-OVDB-ECB-Operator-Token", "X-OVDB-IANA-Operator-Token", demoControlHeader} {
		if http.CanonicalHeaderKey(name) == http.CanonicalHeaderKey(reserved) {
			return true
		}
	}
	return false
}
