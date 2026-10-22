package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Only the container-local destination is selectable; public identity and
// credential cardinality are explicit so duplicate-header refusal is real.
func actualProductionRequest(path string, credentials []string) (*http.Request, error) {
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+path, nil)
	if err != nil {
		return nil, err
	}
	request.Host = cloudHost
	for _, value := range credentials {
		request.Header.Add(defaultOriginSecretHeader, value)
	}
	return request, nil
}

func TestActualCandidateProtectedProbeContract(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	calls := 0
	handler, err := newGateway(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get(defaultOriginSecretHeader) != "" {
			t.Error("origin credential reached inner handler")
		}
		w.WriteHeader(http.StatusOK)
	}), func(key string) string {
		if key == originSecretEnv {
			return token
		}
		return ""
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/databases", "/api/devices", "/device/"} {
		for _, values := range [][]string{nil, {"invalid-job-credential"}, {token, token}} {
			request, err := actualProductionRequest(path, values)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("rejection status=%d", response.Code)
			}
		}
	}
	if calls != 0 {
		t.Fatal("unauthorized inner requests")
	}
	request, err := actualProductionRequest("/v1/databases", []string{token})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || calls != 1 {
		t.Fatalf("protected probe status=%d calls=%d", response.Code, calls)
	}
	if _, err := newGateway(http.NotFoundHandler(), func(string) string { return "" }, nil); err == nil {
		t.Fatal("missing startup credential accepted")
	}
}
