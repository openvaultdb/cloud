package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = closeDatabases()
		}
	})
	for _, provider := range providers {
		t.Run(provider.ID, func(t *testing.T) {
			queriedRows := make(map[string]map[string]any, len(provider.SmokeRecordsets))
			profile := httptest.NewRecorder()
			handler.ServeHTTP(profile, httptest.NewRequest(http.MethodGet, "/v1/databases/"+url.PathEscape(provider.ID), nil))
			if profile.Code != http.StatusOK {
				t.Fatalf("database profile returned %d: %s", profile.Code, profile.Body.String())
			}
			var profileData map[string]any
			if err := json.Unmarshal(profile.Body.Bytes(), &profileData); err != nil {
				t.Fatal(err)
			}
			decimalColumns := make(map[string]map[string]struct{})
			if schemas, ok := profileData["schemas"].(map[string]any); ok {
				if collections, ok := schemas["collections"].(map[string]any); ok {
					for collectionName, rawCollection := range collections {
						collection, _ := rawCollection.(map[string]any)
						fields, _ := collection["fields"].(map[string]any)
						for fieldName, rawField := range fields {
							field, _ := rawField.(map[string]any)
							if field["type"] != "decimal" {
								continue
							}
							decimal, _ := field["decimal"].(map[string]any)
							precision, precisionOK := decimal["precision"].(float64)
							scale, scaleOK := decimal["scale"].(float64)
							if !precisionOK || !scaleOK || precision < 1 || scale < 0 || scale > precision || decimal["storage"] != "text" {
								t.Fatalf("invalid exact decimal metadata for %s.%s: %#v", collectionName, fieldName, field)
							}
							if decimalColumns[collectionName] == nil {
								decimalColumns[collectionName] = make(map[string]struct{})
							}
							decimalColumns[collectionName][fieldName] = struct{}{}
						}
					}
				}
			}
			if provider.ID == "adventureworks" && len(decimalColumns) == 0 {
				t.Fatal("AdventureWorks profile did not publish its actual exact-decimal field descriptors")
			}
			if provider.ID == "adventureworks" {
				if got := countDecimalColumns(decimalColumns); got != 48 {
					t.Fatalf("AdventureWorks exact-decimal descriptor count = %d, want 48", got)
				}
				for _, expected := range []struct {
					collection, field string
					precision, scale  float64
				}{
					{"Purchasing.PurchaseOrderDetail", "UnitPrice", 19, 4},
					{"Sales.SalesOrderDetail", "LineTotal", 38, 6},
					{"Purchasing.PurchaseOrderDetail", "StockedQty", 38, 2},
				} {
					if !hasDecimalMetadata(profileData, expected.collection, expected.field, expected.precision, expected.scale) {
						t.Fatalf("AdventureWorks metadata for %s.%s does not match (%g,%g)", expected.collection, expected.field, expected.precision, expected.scale)
					}
				}
			}

			recordsets := provider.SmokeRecordsets
			if len(recordsets) == 0 {
				recordsets = []string{provider.SmokeRecordset}
			}
			verifiedDecimalString := false
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
				var result northwindQueryPage
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Records) == 0 {
					t.Fatalf("query returned no readable rows from %q: records=%d err=%v body=%s", recordset, len(result.Records), err, response.Body.String())
				}
				queriedRows[recordset] = result.Records[0].Data
				for field := range decimalColumns[recordset] {
					value := result.Records[0].Data[field]
					if value != nil {
						if _, ok := value.(string); !ok {
							t.Fatalf("exact decimal %s.%s crossed the query API as %T: %#v", recordset, field, value, value)
						}
						verifiedDecimalString = true
					}
				}
				if result.Records[0].Key == "" || strings.Contains(result.Records[0].Key, "<nil>") {
					t.Fatalf("recordset %q returned an invalid stable record key %q", recordset, result.Records[0].Key)
				}
				if !strings.HasPrefix(result.Records[0].Key, recordset+"/") {
					t.Fatalf("recordset %q returned key outside its native collection: %q", recordset, result.Records[0].Key)
				}
				adapterID, ok := result.Records[0].Data["id"].(string)
				if !ok || adapterID == "" {
					t.Fatalf("recordset %q did not expose its serving identity for lookup: %#v", recordset, result.Records[0].Data)
				}
				recordURL := "/v1/databases/" + url.PathEscape(provider.ID) + "/records/" + url.PathEscape(recordset) + "/" + url.PathEscape(adapterID)
				record := httptest.NewRecorder()
				handler.ServeHTTP(record, httptest.NewRequest(http.MethodGet, recordURL, nil))
				if record.Code != http.StatusOK {
					t.Fatalf("recordset %q returned a row that could not be fetched by its serving identity: %d %s", recordset, record.Code, record.Body.String())
				}
				var fetched struct {
					Key  string         `json:"key"`
					Data map[string]any `json:"data"`
				}
				if err := json.Unmarshal(record.Body.Bytes(), &fetched); err != nil || fetched.Key != result.Records[0].Key || !recordDataMatches(result.Records[0].Data, fetched.Data) {
					t.Fatalf("recordset %q read-back differs from query row: query=%#v fetched=%#v key=%q err=%v", recordset, result.Records[0].Data, fetched.Data, fetched.Key, err)
				}
				for _, field := range provider.BlobSmokeFields[recordset] {
					blobValue, ok := result.Records[0].Data[field].(string)
					if !ok || blobValue == "" {
						t.Fatalf("recordset %q did not preserve binary field %q as encoded data: %#v", recordset, field, result.Records[0].Data)
					}
					if _, err := base64.StdEncoding.DecodeString(blobValue); err != nil {
						t.Fatalf("recordset %q returned invalid base64 for binary field %q: %v value=%q", recordset, field, err, blobValue[:min(len(blobValue), 48)])
					}
				}
				if got := response.Header().Get("Access-Control-Allow-Origin"); got != provider.CORSOrigins[0] {
					t.Fatalf("provider CORS origin %q, want %q", got, provider.CORSOrigins[0])
				}
			}
			if provider.ID == "adventureworks" && !verifiedDecimalString {
				t.Fatal("AdventureWorks smoke recordsets did not prove a non-null exact decimal remains a JSON string")
			}
			for _, relationship := range provider.ForeignKeySmoke {
				sourceRow := queriedRows[relationship.SourceRecordset]
				foreignKeyValue, exists := sourceRow[relationship.SourceField]
				if !exists || foreignKeyValue == nil {
					t.Fatalf("native foreign key %s.%s is absent from queried source row: %#v", relationship.SourceRecordset, relationship.SourceField, sourceRow)
				}
				query := fmt.Sprintf("from: {name: '%s'}\nwhere: {op: '==', left: {field: '%s'}, right: {param: 'foreignKey'}}\nlimit: 1\n", strings.ReplaceAll(relationship.TargetRecordset, "'", "''"), strings.ReplaceAll(relationship.TargetField, "'", "''"))
				body, err := json.Marshal(map[string]any{"query": query, "parameters": map[string]any{"foreignKey": foreignKeyValue}})
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/v1/databases/"+url.PathEscape(provider.ID)+"/dtql", strings.NewReader(string(body)))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("foreign-key target query %s.%s -> %s.%s returned %d: %s", relationship.SourceRecordset, relationship.SourceField, relationship.TargetRecordset, relationship.TargetField, response.Code, response.Body.String())
				}
				var result northwindQueryPage
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Records) == 0 {
					t.Fatalf("foreign-key target query %s.%s -> %s.%s returned no record: err=%v body=%s", relationship.SourceRecordset, relationship.SourceField, relationship.TargetRecordset, relationship.TargetField, err, response.Body.String())
				}
				if result.Records[0].Data[relationship.TargetField] == nil || !reflect.DeepEqual(foreignKeyValue, result.Records[0].Data[relationship.TargetField]) {
					t.Fatalf("foreign-key target did not match source value %v: got %#v", foreignKeyValue, result.Records[0].Data[relationship.TargetField])
				}
			}
			for _, recordset := range provider.EmptyRecordsets {
				collectionPath := "/ovdb/dbs/" + url.PathEscape(provider.ID) + "/collections/" + url.PathEscape(recordset)
				collection := httptest.NewRecorder()
				handler.ServeHTTP(collection, httptest.NewRequest(http.MethodGet, collectionPath, nil))
				if collection.Code != http.StatusOK {
					t.Fatalf("empty recordset profile %q returned %d: %s", recordset, collection.Code, collection.Body.String())
				}
				query := "from: {name: '" + strings.ReplaceAll(recordset, "'", "''") + "'}\nlimit: 1\n"
				body, err := json.Marshal(map[string]string{"query": query})
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/v1/databases/"+url.PathEscape(provider.ID)+"/dtql", strings.NewReader(string(body)))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("empty recordset query %q returned %d: %s", recordset, response.Code, response.Body.String())
				}
				var result northwindQueryPage
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Records) != 0 {
					t.Fatalf("recordset %q was expected to be empty: records=%d err=%v body=%s", recordset, len(result.Records), err, response.Body.String())
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
	if err := closeDatabases(); err != nil {
		t.Fatalf("close mounted inventory providers: %v", err)
	}
	closed = true
	for _, provider := range providers {
		servingPath := filepath.Join(filepath.Dir(provider.Manifest), provider.ID+".sqlite")
		got, err := sha256File(servingPath)
		if err != nil {
			t.Fatalf("hash serving fixture after mount/query: %s: %v", provider.ID, err)
		}
		if got != provider.ServingSHA256 {
			t.Errorf("read-only mount/query mutated %s serving fixture: got SHA-256 %s, inventory pins %s", provider.ID, got, provider.ServingSHA256)
		}
	}
}

func countDecimalColumns(collections map[string]map[string]struct{}) int {
	total := 0
	for _, fields := range collections {
		total += len(fields)
	}
	return total
}

func hasDecimalMetadata(profile map[string]any, collectionName, fieldName string, precision, scale float64) bool {
	schemas, _ := profile["schemas"].(map[string]any)
	collections, _ := schemas["collections"].(map[string]any)
	collection, _ := collections[collectionName].(map[string]any)
	fields, _ := collection["fields"].(map[string]any)
	field, _ := fields[fieldName].(map[string]any)
	decimal, _ := field["decimal"].(map[string]any)
	return field["type"] == "decimal" && decimal["precision"] == precision && decimal["scale"] == scale && decimal["storage"] == "text"
}

func recordDataMatches(queryData, fetchedData map[string]any) bool {
	for field, queryValue := range queryData {
		fetchedValue, ok := fetchedData[field]
		if queryValue == nil {
			if ok && fetchedValue != nil {
				return false
			}
			continue
		}
		if !ok || !reflect.DeepEqual(queryValue, fetchedValue) {
			return false
		}
	}
	for field := range fetchedData {
		if _, ok := queryData[field]; !ok {
			return false
		}
	}
	return true
}

func TestRecordDataMatchesNormalizesOnlyMissingNullableValues(t *testing.T) {
	query := map[string]any{"OrderID": float64(10248), "ShipRegion": nil}
	if !recordDataMatches(query, map[string]any{"OrderID": float64(10248)}) {
		t.Fatal("a GET response omitting a nullable nil field should match the DTQL row")
	}
	if !recordDataMatches(query, map[string]any{"OrderID": float64(10248), "ShipRegion": nil}) {
		t.Fatal("a GET response preserving a nullable nil field should match the DTQL row")
	}
	if recordDataMatches(query, map[string]any{"OrderID": float64(10248), "ShipRegion": "wrong"}) {
		t.Fatal("a non-nil value must not match a nullable nil DTQL field")
	}
	if recordDataMatches(query, map[string]any{"OrderID": float64(10248), "Extra": true}) {
		t.Fatal("an unexpected response field must not be accepted")
	}
}
