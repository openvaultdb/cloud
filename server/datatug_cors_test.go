package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestCloudDataTugCORS(t *testing.T) {
	const appOrigin = "https://datatug.app"
	pins := []string{"OVDB-Provider-Revision", "OVDB-Source-SHA256", "OVDB-Serving-SHA256", "OVDB-Manifest-SHA256"}
	requestHeaders := append([]string{"Content-Type", "OVDB-Page-Size", "OVDB-Page-Token", "OVDB-Page-Close"}, pins...)
	data, err := os.ReadFile("providers.json")
	if err != nil {
		t.Fatal(err)
	}
	var declared struct {
		Databases []runtimeDatabase `json:"databases"`
	}
	if err := json.Unmarshal(data, &declared); err != nil {
		t.Fatal(err)
	}
	if len(declared.Databases) != 6 {
		t.Fatalf("expected six declared providers, got %d", len(declared.Databases))
	}
	var origins []string
	for _, provider := range declared.Databases {
		if !slices.Contains(provider.CORSOrigins, appOrigin) || provider.CORSOrigins[0] != "https://"+provider.ID+".demodb.dev" {
			t.Fatalf("%s must retain its first site origin and include DataTug: %v", provider.ID, provider.CORSOrigins)
		}
		origins = append(origins, provider.CORSOrigins...)
	}
	// Mount tiny, checksum-verified selected fixtures through the real service
	// handler; use production origin declarations without downloading samples.
	_, fixtures := selectedInventoryFixture(t)
	var providers []runtimeDatabase
	for _, fixture := range fixtures {
		if fixture.ID == "final" {
			fixture.CORSOrigins = origins
			providers = append(providers, fixture)
		}
	}
	if len(providers) != 1 {
		t.Fatal("tiny final fixture missing")
	}
	handler, closeDBs, err := newHandlerWithProviders(providers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeDBs(); err != nil {
			t.Error(err)
		}
	})
	queryPath := "/v1/databases/final/dtql"
	collection := providers[0].SmokeRecordset
	query := "from: {name: " + collection + "}\nlimit: 1\n"
	call := func(method, path, origin, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if body != "" {
			request.Header.Set("Content-Type", "application/yaml")
		}
		if method == http.MethodOptions {
			request.Header.Set("Access-Control-Request-Method", "POST")
			request.Header.Set("Access-Control-Request-Headers", strings.Join(requestHeaders, ","))
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	assertOrigin := func(response *httptest.ResponseRecorder, origin string) {
		t.Helper()
		values := response.Header().Values("Access-Control-Allow-Origin")
		if len(values) != 1 || values[0] != origin {
			t.Fatalf("ACAO %v, want one exact %q", values, origin)
		}
		if !strings.Contains(strings.Join(response.Header().Values("Vary"), ","), "Origin") {
			t.Fatal("Vary: Origin missing")
		}
		if response.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatal("credentials unexpectedly allowed")
		}
	}
	slices.Sort(origins)
	for _, origin := range slices.Compact(origins) {
		for _, path := range []string{"/ovdb/dbs/final", "/ovdb/dbs/final/collections/" + collection} {
			response := call(http.MethodGet, path, origin, "")
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s: %d %s", path, response.Code, response.Body.String())
			}
			assertOrigin(response, origin)
			for _, pin := range pins {
				if !slices.Contains(strings.Split(response.Header().Get("Access-Control-Expose-Headers"), ","), pin) {
					t.Fatalf("GET response exposure missing %s", pin)
				}
			}
		}
		preflight := call(http.MethodOptions, queryPath, origin, "")
		if preflight.Code != http.StatusNoContent {
			t.Fatalf("preflight: %d", preflight.Code)
		}
		assertOrigin(preflight, origin)
		if !slices.Contains(strings.Split(preflight.Header().Get("Access-Control-Allow-Methods"), ","), "POST") {
			t.Fatal("preflight POST missing")
		}
		for _, header := range requestHeaders {
			if !slices.Contains(strings.Split(preflight.Header().Get("Access-Control-Allow-Headers"), ","), header) {
				t.Fatalf("preflight header %s missing", header)
			}
		}
	}
	response := call(http.MethodPost, queryPath, appOrigin, query)
	if response.Code != http.StatusOK {
		t.Fatalf("bounded DTQL: %d %s", response.Code, response.Body.String())
	}
	assertOrigin(response, appOrigin)
	for _, pin := range pins {
		if !slices.Contains(strings.Split(response.Header().Get("Access-Control-Expose-Headers"), ","), pin) {
			t.Fatalf("POST response exposure missing %s", pin)
		}
		// The tiny runtime fixture currently produces no serving pin headers;
		// allowing/exposing a header must never fabricate immutable identity.
		if response.Header().Get(pin) != "" {
			t.Fatalf("unexpected fabricated runtime pin %s", pin)
		}
	}
	var page northwindQueryPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Records) != 1 {
		t.Fatalf("bounded page: %v %s", err, response.Body.String())
	}
	for _, origin := range []string{"https://datatug.app.evil.example", "https://evil.datatug.app", "http://datatug.app", "https://datatug.app:444", "null", ""} {
		for _, method := range []string{http.MethodGet, http.MethodOptions, http.MethodPost} {
			path := "/ovdb/dbs/final"
			if method == http.MethodOptions || method == http.MethodPost {
				path = queryPath
			}
			response := call(method, path, origin, "")
			if response.Header().Get("Access-Control-Allow-Origin") != "" || response.Header().Get("Access-Control-Allow-Credentials") != "" || response.Header().Get("Access-Control-Allow-Headers") != "" || response.Header().Get("Access-Control-Expose-Headers") != "" {
				t.Fatalf("%s granted CORS to %q", method, origin)
			}
		}
	}
	write := call(http.MethodPut, "/v1/databases/final/records/"+collection+"/simple", appOrigin, `{"data":{}}`)
	if write.Code != http.StatusForbidden {
		t.Fatalf("write accepted: %d %s", write.Code, write.Body.String())
	}
	assertOrigin(write, appOrigin)
}
