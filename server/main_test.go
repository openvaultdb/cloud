package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestPublicChinookJourney(t *testing.T) {
	manifest := os.Getenv("CHINOOK_MANIFEST")
	if manifest == "" {
		t.Skip("set CHINOOK_MANIFEST to a prepared fixture")
	}
	handler, closeDatabase, err := newHandler(manifest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDatabase() })
	for _, path := range []string{"/ovdb/", "/ovdb/dbs/", "/ovdb/dbs/chinook", "/ovdb/dbs/chinook/collections/Album", "/.well-known/openvaultdb", "/v1/databases/chinook"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Errorf("GET %s: %d %s", path, response.Code, response.Body.String())
		}
		if path == "/ovdb/dbs/chinook" && !strings.Contains(response.Body.String(), "https://cloud.openvaultdb.com/ovdb/dbs/chinook") {
			t.Error("database profile lacks canonical cloud URL")
		}
		if path == "/ovdb/dbs/chinook" && !strings.Contains(response.Body.String(), `href="/ovdb/dbs/chinook/collections/Album"`) {
			t.Error("database profile lacks Album collection link")
		}
		if path == "/ovdb/dbs/chinook/collections/Album" && (!strings.Contains(response.Body.String(), `<a href="/ovdb/dbs/chinook/collections/Artist">Artist</a>`) || !strings.Contains(response.Body.String(), `<a href="/ovdb/dbs/chinook/collections/Track">Track</a>`)) {
			t.Error("Album profile lacks both relationship directions")
		}
		if path == "/ovdb/dbs/chinook/collections/Album" && (!strings.Contains(response.Body.String(), "Database; enforcement: disabled") || strings.Contains(response.Body.String(), "OVDB declaration; enforcement:")) {
			t.Error("Album profile does not identify database foreign keys and their enforcement state")
		}
	}
	query := map[string]any{"query": "from: {name: Album}\nwhere: {op: '>=', left: {field: ArtistId}, right: {param: MinArtistID}}\norderBy: [{field: AlbumId}]\nlimit: 5\n", "parameters": map[string]any{"MinArtistID": 1}}
	body, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/databases/chinook/dtql", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://chinookdb.com")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("DTQL: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Records) != 5 {
		t.Fatalf("DTQL result: %d records, err=%v, body=%s", len(result.Records), err, response.Body.String())
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "https://chinookdb.com" {
		t.Error("ChinookDB CORS origin missing")
	}
	get := httptest.NewRecorder()
	getURL := "/v1/databases/chinook/dtql?" + url.Values{
		"q":          {query["query"].(string)},
		"parameters": {`{"MinArtistID":1}`},
	}.Encode()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, getURL, nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET DTQL: %d %s", get.Code, get.Body.String())
	}
	if got := get.Header().Get("Cache-Control"); got != "public, max-age=86400, s-maxage=86400" {
		t.Fatalf("GET DTQL Cache-Control: %q", got)
	}
	if got := strings.Join(get.Header().Values("Vary"), ", "); !strings.Contains(got, "Origin") || !strings.Contains(got, "OVDB-Page-Size") {
		t.Fatalf("GET DTQL Vary: %q", got)
	}
	write := httptest.NewRecorder()
	handler.ServeHTTP(write, httptest.NewRequest(http.MethodPut, "/v1/databases/chinook/records/Album/1", strings.NewReader(`{"data":{"Title":"changed"}}`)))
	if write.Code != http.StatusForbidden {
		t.Errorf("write was not rejected: %d %s", write.Code, write.Body.String())
	}
}
