package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeInGitDBRuntimeQueriesAndKeyedReads(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "shipping-logs")
	definition := []byte(`{"record_file":{"name":"records.jsonl","type":"[]map[string]any","format":"jsonl"},"primary_key":["event_id"],"columns":{"event_id":{"type":"string","required":true,"min_length":1},"kind":{"type":"string","required":true}},"columns_order":["event_id","kind"]}`)
	files := map[string][]byte{
		".ingitdb/settings.yaml":         []byte(`{"languages":[{"required":"en"}]}`),
		".ingitdb/root-collections.yaml": []byte(`{"baseline":"collections/baseline","recovery11045example":"collections/recovery11045example","recovery11062example":"collections/recovery11062example"}`),
	}
	for _, id := range []string{"baseline", "recovery11045example", "recovery11062example"} {
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
	provider := runtimeDatabase{StorageEngine: "ingitdb", NativeRoot: "shipping-logs", NativeFiles: pins, ManifestSHA256: digest(manifest), ID: "shipping-logs", Manifest: "shipping-logs.yaml", CORSOrigins: []string{"https://shipping-logs.demodb.dev"}, ProviderRepository: "demo-db/shipping-logs", ProviderRevision: strings.Repeat("b", 40), SourceSHA256: strings.Repeat("a", 64), ServingSHA256: strings.Repeat("c", 64), License: "license.md", LicenseSHA256: digest(license), SmokeRecordset: "baseline", SmokeRecordsets: []string{"baseline", "recovery11045example", "recovery11062example"}}
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

	profile := selectedRequest(handler, "GET", "/v1/databases/shipping-logs", "")
	if profile.Code != 200 {
		t.Fatalf("native profile: %d %s", profile.Code, profile.Body.String())
	}
	for name, want := range loaded[0].verifiedPins {
		if got := profile.Header().Get(name); got != want {
			t.Fatalf("native %s: %q want %q", name, got, want)
		}
	}
	for _, collection := range []string{"baseline", "recovery11045example", "recovery11062example"} {
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
