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

type northwindQueryRow struct {
	Key  string         `json:"key"`
	Data map[string]any `json:"data"`
}

type northwindQueryPage struct {
	Records       []northwindQueryRow `json:"records"`
	SnapshotToken string              `json:"snapshotToken"`
	NextPageToken string              `json:"nextPageToken"`
}

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

	queryText := "from: {name: 'Order Details'}\norderBy: [{field: OrderID}, {field: ProductID}]\nlimit: 2\n"
	query := map[string]any{"query": queryText}
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
	var result northwindQueryPage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Records) != 2 {
		t.Fatalf("Northwind DTQL result: %d records, err=%v, body=%s", len(result.Records), err, response.Body.String())
	}
	assertRowsMatchAdapterID := func(rows []northwindQueryRow) []string {
		t.Helper()
		ids := make([]string, len(rows))
		for i, row := range rows {
			adapterID, ok := row.Data["id"].(string)
			if !ok || adapterID == "" {
				t.Fatalf("Northwind row %d has no adapter data.id: %#v", i, row)
			}
			want := "Order Details/" + adapterID
			if row.Key != want {
				t.Fatalf("Northwind row key %q, want native collection plus adapter id %q", row.Key, want)
			}
			ids[i] = adapterID
		}
		return ids
	}
	adapterIDs := assertRowsMatchAdapterID(result.Records)
	adapterID := adapterIDs[0]

	pageQuery := "from: {name: 'Order Details'}\norderBy: [{field: OrderID}, {field: ProductID}]\n"
	requestPage := func(doc, token string, closeSnapshot bool) *httptest.ResponseRecorder {
		t.Helper()
		pageRequest := httptest.NewRequest(http.MethodPost, "/v1/databases/northwind/dtql", strings.NewReader(doc))
		pageRequest.Header.Set("OVDB-Page-Size", "2")
		if token != "" {
			pageRequest.Header.Set("OVDB-Page-Token", token)
		}
		if closeSnapshot {
			pageRequest.Header.Set("OVDB-Page-Close", "true")
		}
		pageResponse := httptest.NewRecorder()
		handler.ServeHTTP(pageResponse, pageRequest)
		return pageResponse
	}
	var paged northwindQueryPage
	pagedResponse := requestPage(pageQuery, "", false)
	if pagedResponse.Code != http.StatusOK || json.Unmarshal(pagedResponse.Body.Bytes(), &paged) != nil || len(paged.Records) != 2 {
		t.Fatalf("Northwind paged DTQL: %d %s", pagedResponse.Code, pagedResponse.Body.String())
	}
	assertRowsMatchAdapterID(paged.Records)
	if paged.SnapshotToken == "" || paged.NextPageToken == "" {
		t.Fatalf("Northwind paged response did not expose snapshot continuation: %#v", paged)
	}
	secondPageResponse := requestPage(pageQuery, paged.NextPageToken, false)
	var secondPage northwindQueryPage
	if secondPageResponse.Code != http.StatusOK || json.Unmarshal(secondPageResponse.Body.Bytes(), &secondPage) != nil || len(secondPage.Records) != 2 {
		t.Fatalf("Northwind second DTQL page: %d %s", secondPageResponse.Code, secondPageResponse.Body.String())
	}
	assertRowsMatchAdapterID(secondPage.Records)
	if closeResponse := requestPage(pageQuery, paged.SnapshotToken, true); closeResponse.Code != http.StatusNoContent {
		t.Fatalf("close Northwind query snapshot: %d %s", closeResponse.Code, closeResponse.Body.String())
	}

	projectedQuery := pageQuery + "columns: [{field: OrderID}, {field: ProductID}]\n"
	var projected northwindQueryPage
	projectedResponse := requestPage(projectedQuery, "", false)
	if projectedResponse.Code != http.StatusOK || json.Unmarshal(projectedResponse.Body.Bytes(), &projected) != nil || len(projected.Records) != 2 {
		t.Fatalf("Northwind projected DTQL: %d %s", projectedResponse.Code, projectedResponse.Body.String())
	}
	if projected.SnapshotToken == "" {
		t.Fatalf("Northwind projected page lacks a snapshot token: %#v", projected)
	}
	for i, row := range projected.Records {
		if row.Key != paged.Records[i].Key || row.Key != "Order Details/"+adapterIDs[i] {
			t.Fatalf("projected Northwind key %q does not preserve adapter identity %q", row.Key, adapterIDs[i])
		}
		if len(row.Data) != 2 || row.Data["OrderID"] == nil || row.Data["ProductID"] == nil || row.Data["id"] != nil {
			t.Fatalf("projected Northwind data exposed missing fields or helper identity: %#v", row.Data)
		}
	}
	if closeResponse := requestPage(projectedQuery, projected.SnapshotToken, true); closeResponse.Code != http.StatusNoContent {
		t.Fatalf("close projected Northwind snapshot: %d %s", closeResponse.Code, closeResponse.Body.String())
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
