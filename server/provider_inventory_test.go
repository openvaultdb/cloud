package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestInventoryMountAndQueryEveryProvider(t *testing.T) {
	inventoryPath := os.Getenv("SAMPLE_DATABASES_INVENTORY")
	if inventoryPath == "" {
		t.Skip("set SAMPLE_DATABASES_INVENTORY to generated, checksum-verified serving fixtures")
	}
	providers, err := loadRuntimeInventory(inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	handler, closeDatabases, err := newHandlerWithProviders(providers)
	if err != nil {
		t.Fatalf("mount inventory providers: %v", err)
	}
	t.Cleanup(func() { _ = closeDatabases() })
	for _, provider := range providers {
		t.Run(provider.ID, func(t *testing.T) {
			profile := httptest.NewRecorder()
			handler.ServeHTTP(profile, httptest.NewRequest(http.MethodGet, "/v1/databases/"+url.PathEscape(provider.ID), nil))
			if profile.Code != http.StatusOK {
				t.Fatalf("database profile returned %d: %s", profile.Code, profile.Body.String())
			}

			recordsets := provider.SmokeRecordsets
			if len(recordsets) == 0 {
				recordsets = []string{provider.SmokeRecordset}
			}
			for _, recordset := range recordsets {
				collectionPath := "/ovdb/dbs/" + url.PathEscape(provider.ID) + "/collections/" + url.PathEscape(recordset)
				collection := httptest.NewRecorder()
				handler.ServeHTTP(collection, httptest.NewRequest(http.MethodGet, collectionPath, nil))
				if collection.Code != http.StatusOK {
					t.Fatalf("recordset profile %q returned %d: %s", recordset, collection.Code, collection.Body.String())
				}

				query := "from: {name: '" + strings.ReplaceAll(recordset, "'", "''") + "'}\nlimit: 1\n"
				body, err := json.Marshal(map[string]string{"query": query})
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/v1/databases/"+url.PathEscape(provider.ID)+"/dtql", strings.NewReader(string(body)))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Origin", provider.CORSOrigins[0])
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("query recordset %q returned %d: %s", recordset, response.Code, response.Body.String())
				}
				var result struct {
					Records []northwindQueryRow `json:"records"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Records) == 0 {
					t.Fatalf("query returned no readable rows from %q: records=%d err=%v body=%s", recordset, len(result.Records), err, response.Body.String())
				}
				if result.Records[0].Key == "" || strings.Contains(result.Records[0].Key, "<nil>") {
					t.Fatalf("recordset %q returned an invalid stable record key %q", recordset, result.Records[0].Key)
				}
				if got := response.Header().Get("Access-Control-Allow-Origin"); got != provider.CORSOrigins[0] {
					t.Fatalf("provider CORS origin %q, want %q", got, provider.CORSOrigins[0])
				}
			}

			write := httptest.NewRecorder()
			writeURL := fmt.Sprintf("/v1/databases/%s/records/%s/not-a-real-key", url.PathEscape(provider.ID), url.PathEscape(recordsets[0]))
			handler.ServeHTTP(write, httptest.NewRequest(http.MethodPut, writeURL, strings.NewReader(`{"data":{}}`)))
			if write.Code != http.StatusForbidden {
				t.Fatalf("read-only provider accepted or ambiguously handled a write: %d %s", write.Code, write.Body.String())
			}
		})
	}
}
