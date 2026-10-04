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
	legacyRecord := httptest.NewRecorder()
	handler.ServeHTTP(legacyRecord, httptest.NewRequest(http.MethodGet, "/v1/databases/chinook/records/Album/1", nil))
	if legacyRecord.Code != http.StatusOK {
		t.Fatalf("legacy Chinook record key 1: %d %s", legacyRecord.Code, legacyRecord.Body.String())
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

func TestPublicNorthwindJourney(t *testing.T) {
	chinookManifest := os.Getenv("CHINOOK_MANIFEST")
	northwindManifest := os.Getenv("NORTHWIND_MANIFEST")
	if chinookManifest == "" || northwindManifest == "" {
		t.Skip("set CHINOOK_MANIFEST and NORTHWIND_MANIFEST to prepared fixtures")
	}
	handler, closeDatabases, err := newHandlerWithManifests(map[string]string{
		"chinook": chinookManifest, "northwind": northwindManifest,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDatabases() })

	for _, path := range []string{"/ovdb/dbs/", "/ovdb/dbs/northwind", "/ovdb/dbs/northwind/collections/Order%20Details", "/ovdb/dbs/northwind/collections/Employees"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, response.Code, response.Body.String())
		}
		if path == "/ovdb/dbs/" && (!strings.Contains(response.Body.String(), "chinook") || !strings.Contains(response.Body.String(), "northwind")) {
			t.Fatalf("database discovery omitted a sample database: %s", response.Body.String())
		}
		if path == "/ovdb/dbs/northwind/collections/Order%20Details" {
			for _, expected := range []string{"Order Details", "OrderID", "ProductID", "Orders", "Products"} {
				if !strings.Contains(response.Body.String(), expected) {
					t.Fatalf("spaced collection schema omits %q: %s", expected, response.Body.String())
				}
			}
		}
		if path == "/ovdb/dbs/northwind/collections/Employees" && (!strings.Contains(response.Body.String(), "ReportsTo") || !strings.Contains(response.Body.String(), "Employees")) {
			t.Fatalf("Northwind self-referential employee relationship was omitted: %s", response.Body.String())
		}
	}

	query := map[string]any{"query": "from: {name: 'Order Details'}\nlimit: 2\n"}
	body, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/databases/northwind/dtql", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://northwind.demodb.dev")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("Northwind DTQL: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Records) != 2 {
		t.Fatalf("Northwind DTQL result: %d records, err=%v, body=%s", len(result.Records), err, response.Body.String())
	}
	var firstRows struct {
		Records []struct {
			Data map[string]any `json:"data"`
		} `json:"records"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &firstRows); err != nil || len(firstRows.Records) == 0 {
		t.Fatalf("Northwind row data: err=%v body=%s", err, response.Body.String())
	}
	adapterID, ok := firstRows.Records[0].Data["id"].(string)
	if !ok || adapterID == "" {
		t.Fatalf("Northwind composite row has no adapter id: %s", response.Body.String())
	}
	compositeRecord := httptest.NewRecorder()
	collection := url.PathEscape("Order Details")
	key := url.PathEscape(adapterID)
	handler.ServeHTTP(compositeRecord, httptest.NewRequest(http.MethodGet, "/v1/databases/northwind/records/"+collection+"/"+key, nil))
	if compositeRecord.Code != http.StatusOK {
		t.Fatalf("Northwind composite-key record lookup: %d %s", compositeRecord.Code, compositeRecord.Body.String())
	}
	headRecord := httptest.NewRecorder()
	handler.ServeHTTP(headRecord, httptest.NewRequest(http.MethodHead, "/v1/databases/northwind/records/"+collection+"/"+key, nil))
	if headRecord.Code != http.StatusOK {
		t.Fatalf("Northwind composite-key HEAD lookup: %d %s", headRecord.Code, headRecord.Body.String())
	}
	readQuery := url.Values{"key": {url.PathEscape("Order Details") + "/" + key}}
	queryRecord := httptest.NewRecorder()
	handler.ServeHTTP(queryRecord, httptest.NewRequest(http.MethodGet, "/v1/databases/northwind/read?"+readQuery.Encode(), nil))
	if queryRecord.Code != http.StatusOK {
		t.Fatalf("Northwind composite-key query read: %d %s", queryRecord.Code, queryRecord.Body.String())
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "https://northwind.demodb.dev" {
		t.Error("Northwind site CORS origin missing")
	}
	orderRecord := httptest.NewRecorder()
	handler.ServeHTTP(orderRecord, httptest.NewRequest(http.MethodGet, "/v1/databases/northwind/records/Orders/10248", nil))
	if orderRecord.Code != http.StatusOK {
		t.Fatalf("Northwind natural-key record lookup: %d %s", orderRecord.Code, orderRecord.Body.String())
	}
	for _, origin := range []string{"https://demodb.dev", "https://chinook.demodb.dev"} {
		cors := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/ovdb/dbs/", nil)
		request.Header.Set("Origin", origin)
		handler.ServeHTTP(cors, request)
		if cors.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("CORS origin %s missing", origin)
		}
	}

	blobRequest := httptest.NewRequest(http.MethodPost, "/v1/databases/northwind/dtql", strings.NewReader(`{"query":"from: {name: Employees}\ncolumns: [{field: Photo}]\nlimit: 1\n"}`))
	blobRequest.Header.Set("Content-Type", "application/json")
	blobResponse := httptest.NewRecorder()
	handler.ServeHTTP(blobResponse, blobRequest)
	if blobResponse.Code != http.StatusOK {
		t.Fatalf("Northwind BLOB query: %d %s", blobResponse.Code, blobResponse.Body.String())
	}
	var blobResult struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(blobResponse.Body.Bytes(), &blobResult); err != nil || len(blobResult.Records) != 1 {
		t.Fatalf("Northwind BLOB result: records=%d err=%v body=%s", len(blobResult.Records), err, blobResponse.Body.String())
	}
	data, ok := blobResult.Records[0]["data"].(map[string]any)
	if !ok {
		t.Fatalf("Northwind BLOB result has no record data: %s", blobResponse.Body.String())
	}
	if _, ok := data["Photo"]; !ok {
		t.Fatalf("Northwind BLOB column was not returned: %s", blobResponse.Body.String())
	}

	write := httptest.NewRecorder()
	handler.ServeHTTP(write, httptest.NewRequest(http.MethodPut, "/v1/databases/northwind/records/Orders/1", strings.NewReader(`{"data":{"ShipName":"changed"}}`)))
	if write.Code != http.StatusForbidden {
		t.Errorf("Northwind write was not rejected: %d %s", write.Code, write.Body.String())
	}
}
