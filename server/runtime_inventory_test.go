package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestNativeInGitDBRuntimeQueriesAndKeyedReads(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "shipping-logs")
	definition := []byte(`{"record_file":{"name":"records.jsonl","type":"[]map[string]any","format":"jsonl"},"primary_key":["event_id"],"columns":{"event_id":{"type":"string","required":true,"min_length":1},"kind":{"type":"string","required":true}},"columns_order":["event_id","kind"]}`)
	files := map[string][]byte{
		".ingitdb/settings.yaml": []byte(`{"languages":[{"required":"en"}]}`),
	}
	var roots []string
	for _, id := range nativeCollections {
		roots = append(roots, `"`+id+`":"collections/`+id+`"`)
	}
	files[".ingitdb/root-collections.yaml"] = []byte("{" + strings.Join(roots, ",") + "}")
	for _, id := range nativeCollections {
		files["collections/"+id+"/.collection/definition.yaml"] = definition
		files["collections/"+id+"/records.jsonl"] = []byte(`{"event_id":"` + id + `-e1","kind":"observed"}` + "\n")
	}
	var pins []runtimePinnedFile
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		pins = append(pins, runtimePinnedFile{Path: name, SHA256: digest(content), Bytes: int64(len(content))})
	}
	manifest := []byte("database:\n  id: shipping-logs\n  schema_mode: schemaless\nstorage:\n  engine: ingitdb\n  path: ./shipping-logs\n")
	license := []byte("CC0")
	for name, content := range map[string][]byte{"shipping-logs.yaml": manifest, "shipping-logs.source-sha256": []byte(strings.Repeat("a", 64) + "\n"), "license.md": license} {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	provider := runtimeDatabase{StorageEngine: "ingitdb", NativeRoot: "shipping-logs", NativeFiles: pins, ManifestSHA256: digest(manifest), ID: "shipping-logs", Manifest: "shipping-logs.yaml", CORSOrigins: []string{"https://shipping-logs.demodb.dev"}, ProviderRepository: "demo-db/shipping-logs", ProviderRevision: strings.Repeat("b", 40), SourceSHA256: strings.Repeat("a", 64), ServingSHA256: strings.Repeat("c", 64), License: "license.md", LicenseSHA256: digest(license), SmokeRecordset: "baseline", SmokeRecordsets: nativeCollections}
	data, _ := json.Marshal(runtimeInventory{Version: 2, Databases: []runtimeDatabase{provider}})
	inventoryPath := filepath.Join(directory, "inventory.json")
	if err := os.WriteFile(inventoryPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadRuntimeInventory(inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	handler, closeDBs, err := newHandlerWithProviders(loaded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeDBs(); err != nil {
			t.Error(err)
		}
	})

	assertNativeCollectionDiscovery(t, handler)
	profile := selectedRequest(handler, "GET", "/v1/databases/shipping-logs", "")
	if profile.Code != 200 {
		t.Fatalf("native profile: %d %s", profile.Code, profile.Body.String())
	}
	for name, want := range loaded[0].verifiedPins {
		if got := profile.Header().Get(name); got != want {
			t.Fatalf("native %s: %q want %q", name, got, want)
		}
	}
	for _, collection := range nativeCollections {
		query := selectedRequest(handler, "POST", "/v1/databases/shipping-logs/dtql", "from: {name: "+collection+"}\nlimit: 10\n")
		if query.Code != 200 || !strings.Contains(query.Body.String(), collection+"-e1") {
			t.Fatalf("native query %s: %d %s", collection, query.Code, query.Body.String())
		}
		keyed := selectedRequest(handler, "GET", "/v1/databases/shipping-logs/records/"+collection+"/"+collection+"-e1", "")
		if keyed.Code != 200 || !strings.Contains(keyed.Body.String(), "observed") {
			t.Fatalf("native keyed %s: %d %s", collection, keyed.Code, keyed.Body.String())
		}
	}
	query := selectedRequest(handler, "POST", "/v1/databases/shipping-logs/dtql", "from: {name: baseline}\nlimit: 10\n")
	if query.Code != 200 || !strings.Contains(query.Body.String(), "baseline-e1") {
		t.Fatalf("native query: %d %s", query.Code, query.Body.String())
	}
	keyed := selectedRequest(handler, "GET", "/v1/databases/shipping-logs/records/baseline/baseline-e1", "")
	if keyed.Code != 200 || !strings.Contains(keyed.Body.String(), "observed") {
		t.Fatalf("native keyed read: %d %s", keyed.Code, keyed.Body.String())
	}
	canceled := httptest.NewRequest("POST", "/v1/databases/shipping-logs/dtql", strings.NewReader(`{"query":"from: {name: baseline}"}`)).WithContext(func() context.Context { c, cancel := context.WithCancel(context.Background()); cancel(); return c }())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, canceled)
	if response.Code == 200 {
		t.Fatal("canceled native query succeeded")
	}
}

func TestLoadRuntimeInventoryVerifiesFixtureAndLicensePins(t *testing.T) {
	directory := t.TempDir()
	fixture := []byte("derived sqlite fixture bytes")
	fixtureHash := sha256.Sum256(fixture)
	fixtureSHA := hex.EncodeToString(fixtureHash[:])
	license := []byte("sample license")
	licenseHash := sha256.Sum256(license)
	licenseSHA := hex.EncodeToString(licenseHash[:])
	for path, content := range map[string][]byte{
		"sample-db.yaml":          []byte("database: {id: sample-db}\n"),
		"sample-db.sqlite":        fixture,
		"sample-db.source-sha256": []byte(fixtureSHA + "\n"),
		"sample-db-license.md":    license,
	} {
		if err := os.WriteFile(filepath.Join(directory, path), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeInventory := func(path, artifactHash, licenseHash string, origins []string) {
		t.Helper()
		inventory := runtimeInventory{Version: 1, Databases: []runtimeDatabase{{
			ID: "sample-db", Manifest: "sample-db.yaml", License: "sample-db-license.md",
			ProviderRepository: "demo-db/sample", ProviderRevision: strings.Repeat("a", 40), SourceSHA256: artifactHash, ServingSHA256: artifactHash, LicenseSHA256: licenseHash,
			CORSOrigins: origins, SmokeRecordset: "records",
			EmptyRecordsets: []string{"empty"},
		}}}
		data, err := json.Marshal(inventory)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inventoryPath := filepath.Join(directory, "inventory.json")
	writeInventory(inventoryPath, fixtureSHA, licenseSHA, []string{"https://sample.demodb.dev", "https://demodb.dev"})
	databases, err := loadRuntimeInventory(inventoryPath)
	if err != nil {
		t.Fatalf("load pinned runtime inventory: %v", err)
	}
	if len(databases) != 1 || databases[0].ID != "sample-db" || databases[0].Manifest != filepath.Join(directory, "sample-db.yaml") {
		t.Fatalf("runtime inventory did not preserve arbitrary provider identity and resolve its manifest: %#v", databases)
	}

	writeInventory(inventoryPath, strings.Repeat("b", 64), licenseSHA, []string{"https://sample.demodb.dev"})
	if _, err := loadRuntimeInventory(inventoryPath); err == nil || !strings.Contains(err.Error(), "source hash receipt") {
		t.Fatalf("mismatched source marker was accepted: %v", err)
	}
	writeInventory(inventoryPath, fixtureSHA, strings.Repeat("c", 64), []string{"https://sample.demodb.dev"})
	if _, err := loadRuntimeInventory(inventoryPath); err == nil || !strings.Contains(err.Error(), "license hash") {
		t.Fatalf("mismatched license was accepted: %v", err)
	}
	writeInventory(inventoryPath, fixtureSHA, licenseSHA, []string{"http://sample.demodb.dev"})
	if _, err := loadRuntimeInventory(inventoryPath); err == nil || !strings.Contains(err.Error(), "invalid CORS origin") {
		t.Fatalf("non-HTTPS CORS origin was accepted: %v", err)
	}
}

func TestLoadRuntimeInventoryRejectsDuplicateSmokeGroups(t *testing.T) {
	directory := t.TempDir()
	fixture := []byte("fixture")
	fixtureHash := sha256.Sum256(fixture)
	fixtureSHA := hex.EncodeToString(fixtureHash[:])
	license := []byte("license")
	licenseHash := sha256.Sum256(license)
	licenseSHA := hex.EncodeToString(licenseHash[:])
	for name, content := range map[string][]byte{
		"sample.yaml": []byte("database: {id: sample}\n"), "sample.sqlite": fixture,
		"sample.source-sha256": []byte(fixtureSHA + "\n"), "license.txt": license,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	database := runtimeDatabase{
		ID: "sample", Manifest: "sample.yaml", License: "license.txt", ProviderRepository: "demo-db/sample",
		ProviderRevision: strings.Repeat("a", 40), SourceSHA256: fixtureSHA, ServingSHA256: fixtureSHA,
		LicenseSHA256: licenseSHA, CORSOrigins: []string{"https://sample.demodb.dev"},
		SmokeRecordset: "records", SmokeRecordsets: []string{"records"}, EmptyRecordsets: []string{"records"},
	}
	data, err := json.Marshal(runtimeInventory{Version: 1, Databases: []runtimeDatabase{database}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "inventory.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRuntimeInventory(path); err == nil || !strings.Contains(err.Error(), "repeats recordset") {
		t.Fatalf("recordset listed as both populated and empty was accepted: %v", err)
	}
}

func TestLoadRuntimeInventoryRejectsUnsafeAndDuplicateProviderIDs(t *testing.T) {
	directory := t.TempDir()
	write := func(databases []runtimeDatabase) string {
		t.Helper()
		path := filepath.Join(directory, "inventory.json")
		data, err := json.Marshal(runtimeInventory{Version: 1, Databases: databases})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	unsafe := runtimeDatabase{ID: "../bad", Manifest: "sample.yaml", License: "license.md", ProviderRepository: "demo-db/sample", ProviderRevision: strings.Repeat("a", 40), SourceSHA256: strings.Repeat("b", 64), ServingSHA256: strings.Repeat("b", 64), LicenseSHA256: strings.Repeat("c", 64), SmokeRecordset: "records", CORSOrigins: []string{"https://sample.demodb.dev"}}
	if _, err := loadRuntimeInventory(write([]runtimeDatabase{unsafe})); err == nil || !strings.Contains(err.Error(), "invalid database ID") {
		t.Fatalf("unsafe provider ID was accepted: %v", err)
	}
	duplicate := unsafe
	duplicate.ID = "sample"
	unsafe.ID = "sample"
	if _, err := loadRuntimeInventory(write([]runtimeDatabase{unsafe, duplicate})); err == nil || !strings.Contains(err.Error(), "repeats database ID") {
		t.Fatalf("duplicate provider ID was accepted: %v", err)
	}
}

// The native root map is the public identity boundary, not its physical directories.
func assertNativeCollectionDiscovery(t *testing.T, handler http.Handler) {
	t.Helper()
	expected := slices.Clone(nativeCollections)
	sort.Strings(expected)
	for _, prefix := range []string{"/v1/databases/", "/ovdb/dbs/"} {
		profile := selectedRequest(handler, "GET", prefix+"shipping-logs", "")
		if profile.Code != http.StatusOK {
			t.Fatalf("native profile %s: %d %s", prefix, profile.Code, profile.Body.String())
		}
		if prefix == "/v1/databases/" {
			var data struct {
				Collections []string `json:"collections"`
			}
			if err := json.Unmarshal(profile.Body.Bytes(), &data); err != nil {
				t.Fatal(err)
			}
			sort.Strings(data.Collections)
			if !reflect.DeepEqual(data.Collections, expected) {
				t.Fatalf("native public collection IDs: %v want %v", data.Collections, expected)
			}
		}

	}
}

func TestActualNativeCollectionDiscoveryAndReads(t *testing.T) {
	inventory := os.Getenv("SAMPLE_DATABASES_INVENTORY")
	if inventory == "" {
		t.Skip("set SAMPLE_DATABASES_INVENTORY to checksum-verified original native fixtures")
	}
	providers, err := loadRuntimeInventory(inventory)
	if err != nil {
		t.Fatal(err)
	}
	var native []runtimeDatabase
	for _, provider := range providers {
		if provider.ID == "shipping-logs" {
			native = append(native, provider)
		}
	}
	if len(native) != 1 {
		t.Fatal("actual inventory must include exactly one shipping-logs provider")
	}
	handler, closeDBs, err := newHandlerWithProviders(native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := closeDBs(); closeErr != nil {
			t.Errorf("close native databases: %v", closeErr)
		}
	})
	assertNativeCollectionDiscovery(t, handler)
	for collection, key := range map[string]string{"baseline": "attempt-11062-1-e1", "recovery11045example": "attempt-11045-2-e1", "recovery11062example": "attempt-11062-2-e1", "carrierGateway": "attempt-11038-1-c1", "carrierGatewayRecovery11045example": "attempt-11045-2-c1", "carrierGatewayRecovery11062example": "attempt-11062-2-c1"} {
		query := selectedRequest(handler, "POST", "/v1/databases/shipping-logs/dtql", "from: {name: "+collection+"}\nlimit: 100\n")
		if query.Code != http.StatusOK {
			t.Fatalf("native query %s: %d %s", collection, query.Code, query.Body.String())
		}
		// Native mounts have no immutable-query cache policy; real ServeHTTP must refuse storage.
		getQuery := selectedRequest(handler, "GET", "/v1/databases/shipping-logs/dtql?q="+url.QueryEscape("from: {name: "+collection+"}\nlimit: 100\n"), "")
		if getQuery.Code != http.StatusOK || getQuery.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("native GET cache contract %s: %d %q", collection, getQuery.Code, getQuery.Header().Get("Cache-Control"))
		}
		t.Logf("native %s actual GET Cache-Control: %s", collection, getQuery.Header().Get("Cache-Control"))
		var rows struct {
			Records []struct {
				Key  string         `json:"key"`
				Data map[string]any `json:"data"`
			} `json:"records"`
		}
		if err := json.Unmarshal(query.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		var expected map[string]any
		for _, record := range rows.Records {
			if !strings.HasPrefix(record.Key, collection+"/") {
				t.Fatalf("cross-collection key %q", record.Key)
			}
			if record.Data["event_id"] == key {
				expected = record.Data
			}
		}
		if expected == nil {
			t.Fatalf("original event %s absent from %s", key, collection)
		}
		keyed := selectedRequest(handler, "GET", "/v1/databases/shipping-logs/records/"+collection+"/"+key, "")
		var record struct {
			Data map[string]any `json:"data"`
		}
		if keyed.Code != http.StatusOK {
			t.Fatalf("native keyed %s: %d %s", key, keyed.Code, keyed.Body.String())
		}
		if err := json.Unmarshal(keyed.Body.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(record.Data, expected) {
			t.Fatalf("native keyed event %s differs from query", key)
		}
	}
}
