package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
