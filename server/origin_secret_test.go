package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func originSecretEnvMap(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestOriginSecretUnsetLeavesHandlerUnchanged(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	handler, err := withOriginSecret(inner, originSecretEnvMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/databases/chinook", nil))
	if response.Code != http.StatusTeapot {
		t.Fatalf("unset secret changed behaviour: %d", response.Code)
	}
}

func TestOriginSecretRequiresTheWorkerHeader(t *testing.T) {
	const secret = "0123456789abcdefghijklmnopqrstuvwxyz_-AB"
	var seen http.Header
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	})
	for _, c := range []struct {
		name       string
		headerName string
		send       map[string][]string
		want       int
	}{
		{"default header", "", map[string][]string{"X-OVDB-Origin-Secret": {secret}}, http.StatusOK},
		{"configured header", "X-Demo-Origin", map[string][]string{"X-Demo-Origin": {secret}}, http.StatusOK},
		{"missing", "", nil, http.StatusForbidden},
		{"wrong value", "", map[string][]string{"X-OVDB-Origin-Secret": {strings.Repeat("a", 40)}}, http.StatusForbidden},
		{"prefix of the value", "", map[string][]string{"X-OVDB-Origin-Secret": {secret[:32]}}, http.StatusForbidden},
		{"repeated header", "", map[string][]string{"X-OVDB-Origin-Secret": {secret, secret}}, http.StatusForbidden},
		{"default header when another is configured", "X-Demo-Origin", map[string][]string{"X-OVDB-Origin-Secret": {secret}}, http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			seen = nil
			handler, err := withOriginSecret(inner, originSecretEnvMap(map[string]string{originSecretEnv: secret, originSecretHeaderEnv: c.headerName}))
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/databases/chinook/dtql", nil)
			for name, values := range c.send {
				for _, value := range values {
					request.Header.Add(name, value)
				}
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != c.want {
				t.Fatalf("status = %d, want %d", response.Code, c.want)
			}
			if strings.Contains(response.Body.String(), secret) {
				t.Fatal("response echoed the secret")
			}
			if c.want == http.StatusOK {
				for name, values := range seen {
					for _, value := range values {
						if value == secret {
							t.Fatalf("the library handler received the secret in %s", name)
						}
					}
				}
			} else if seen != nil {
				t.Fatal("a refused request reached the library handler")
			}
		})
	}
}

func TestOriginSecretRejectsWeakConfiguration(t *testing.T) {
	inner := http.NotFoundHandler()
	for _, env := range []map[string]string{
		{originSecretEnv: "short"},
		{originSecretEnv: strings.Repeat("a", 40) + "!"},
		{originSecretEnv: strings.Repeat("a", 40), originSecretHeaderEnv: "Authorization"},
		{originSecretEnv: strings.Repeat("a", 40), originSecretHeaderEnv: "X-Bad Header"},
		{originSecretHeaderEnv: "Host"},
		{originSecretEnv: strings.Repeat("a", 40), originSecretHeaderEnv: "X-OVDB-ECB-Proxy-Secret"},
		{originSecretEnv: strings.Repeat("a", 40), originSecretHeaderEnv: demoControlHeader},
	} {
		if _, err := withOriginSecret(inner, originSecretEnvMap(env)); err == nil {
			t.Errorf("configuration %v was accepted", env)
		} else if value := env[originSecretEnv]; value != "" && strings.Contains(err.Error(), value) {
			t.Errorf("error disclosed the secret: %v", err)
		}
	}
}
