package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func TestAppendConfiguredDemoPostgresIsOptInAndAllOrNothing(t *testing.T) {
	base := []runtimeDatabase{{ID: "chinook"}}
	got, err := appendConfiguredDemoPostgres(base)
	if err != nil || len(got) != 1 || got[0].ID != "chinook" {
		t.Fatalf("disabled native PostgreSQL config = %#v, %v", got, err)
	}
	t.Setenv(demoPostgresEnabledEnv, " false ")
	got, err = appendConfiguredDemoPostgres(base)
	if err != nil || len(got) != 1 {
		t.Fatalf("explicitly disabled native PostgreSQL config = %#v, %v", got, err)
	}

	t.Setenv(demoPostgresEnabledEnv, "true")
	for _, source := range demoPostgresSources {
		t.Setenv(source.dsnEnv, "")
	}
	t.Setenv(demoPostgresSources[0].dsnEnv, "postgres://reader:private-marker@example.invalid/chinook")
	got, err = appendConfiguredDemoPostgres(base)
	if err == nil || !strings.Contains(err.Error(), demoPostgresSources[1].dsnEnv) || strings.Contains(err.Error(), "private-marker") {
		t.Fatalf("partial secret configuration error = %v", err)
	}
}

func TestConfiguredDemoPostgresDefinesSixDistinctReadOnlyMounts(t *testing.T) {
	t.Setenv(demoPostgresEnabledEnv, "true")
	const secretMarker = "private-marker-do-not-log"
	for _, source := range demoPostgresSources {
		t.Setenv(source.dsnEnv, "postgres://reader:"+secretMarker+"@example.invalid/"+source.databaseID)
	}

	providers, err := appendConfiguredDemoPostgres(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != len(demoPostgresSources) {
		t.Fatalf("configured PostgreSQL provider count = %d, want %d", len(providers), len(demoPostgresSources))
	}
	for i, source := range demoPostgresSources {
		provider := providers[i]
		if provider.ID != source.databaseID+"-postgresql" || provider.Manifest != "" || provider.nativePostgres == nil || provider.nativePostgres.schemaName != source.databaseID || provider.nativePostgres.dsnEnv != source.dsnEnv {
			t.Errorf("provider %d config = %#v", i, provider)
			continue
		}
		manifestBytes, err := demoPostgresManifest(provider)
		if err != nil {
			t.Fatalf("%s manifest: %v", provider.ID, err)
		}
		parsed, err := manifest.Parse(manifestBytes)
		if err != nil {
			t.Fatalf("%s manifest parse: %v", provider.ID, err)
		}
		if parsed.Database.ID != provider.ID || parsed.Storage.Engine != "postgres" || parsed.Storage.Postgres == nil || !parsed.Storage.Postgres.ReadOnly || parsed.Storage.Postgres.DSNEnv != source.dsnEnv {
			t.Errorf("%s manifest does not declare its read-only secret-backed mount: %+v", provider.ID, parsed)
		}
		options, err := demoPostgresMountOptions(provider)
		if err != nil || options.CatalogueDir != "" || len(options.ExcludedNativePostgresRelations) != 1 || options.ExcludedNativePostgresRelations[0].Schema != source.databaseID || options.ExcludedNativePostgresRelations[0].Name != "_import_manifest" {
			t.Errorf("%s exclusions = %+v, %v", provider.ID, options.ExcludedNativePostgresRelations, err)
		}
		if !reflect.DeepEqual(provider.CORSOrigins, demoPostgresCORSOrigins(source.databaseID)) {
			t.Errorf("%s CORS origins = %q", provider.ID, provider.CORSOrigins)
		}
		if !containsString(provider.CORSOrigins, "https://datatug.app") {
			t.Errorf("%s is missing the trusted DataTug origin: %q", provider.ID, provider.CORSOrigins)
		}
	}

	encoded, err := json.Marshal(providers)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secretMarker) || strings.Contains(string(encoded), "nativePostgres") {
		t.Fatalf("runtime provider JSON exposed runtime credentials or mount configuration: %s", encoded)
	}
}

func TestAppendConfiguredDemoPostgresRejectsDuplicateMountIDs(t *testing.T) {
	t.Setenv(demoPostgresEnabledEnv, "true")
	for _, source := range demoPostgresSources {
		t.Setenv(source.dsnEnv, "configured")
	}
	_, err := appendConfiguredDemoPostgres([]runtimeDatabase{{ID: "chinook-postgresql"}})
	if err == nil || !strings.Contains(err.Error(), `database ID "chinook-postgresql" is already configured`) {
		t.Fatalf("duplicate native mount ID error = %v", err)
	}
}

func TestDemoPostgresManifestRejectsInvalidRuntimeConfig(t *testing.T) {
	for _, provider := range []runtimeDatabase{
		{ID: "chinook-postgresql"},
		{ID: "northwind-postgresql", nativePostgres: &nativePostgresMountConfig{schemaName: "chinook", dsnEnv: "OVDB_PG_CHINOOK_DSN"}},
		{ID: "chinook-postgresql", nativePostgres: &nativePostgresMountConfig{schemaName: "chinook", dsnEnv: "bad-name"}},
	} {
		if _, err := demoPostgresManifest(provider); err == nil {
			t.Errorf("invalid native PostgreSQL config was accepted: %#v", provider)
		}
	}
}

func TestDemoPostgresReadOnlyPublicJourney(t *testing.T) {
	for _, source := range demoPostgresSources {
		if os.Getenv(source.dsnEnv) == "" {
			t.Skipf("set all six secret-backed %s values for the in-process PostgreSQL journey", source.dsnEnv)
		}
	}
	t.Setenv(demoPostgresEnabledEnv, "true")
	providers, err := appendConfiguredDemoPostgres(nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var beforeMount runtime.MemStats
	runtime.ReadMemStats(&beforeMount)
	handler, closeDatabases, err := newHandlerWithProviders(providers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDatabases() })
	runtime.GC()
	var afterMount runtime.MemStats
	runtime.ReadMemStats(&afterMount)
	var mountedHeapBytes uint64
	if afterMount.HeapAlloc > beforeMount.HeapAlloc {
		mountedHeapBytes = afterMount.HeapAlloc - beforeMount.HeapAlloc
	}
	t.Logf("six native PostgreSQL mounts, pools and discovered catalogs retain %d additional heap bytes", mountedHeapBytes)
	if mountedHeapBytes > 64<<20 {
		t.Fatalf("six native PostgreSQL mounts exceed the 64 MiB at-rest heap allowance: %d", mountedHeapBytes)
	}

	for _, source := range demoPostgresSources {
		id := source.databaseID + "-postgresql"
		dtqlPath := "/v1/databases/" + id + "/dtql"
		preflight := postgreSQLRequest(t, handler, http.MethodOptions, dtqlPath, "", "https://datatug.app")
		if preflight.Code != http.StatusNoContent || preflight.Header().Get("Access-Control-Allow-Origin") != "https://datatug.app" || !strings.Contains(preflight.Header().Get("Access-Control-Allow-Methods"), "POST") || !strings.Contains(strings.ToLower(preflight.Header().Get("Access-Control-Allow-Headers")), "content-type") {
			t.Fatalf("%s DataTug DTQL preflight: %d headers=%v body=%s", id, preflight.Code, preflight.Header(), preflight.Body.String())
		}
		descriptorResponse := postgreSQLRequest(t, handler, http.MethodGet, "/v1/databases/"+id, "")
		if descriptorResponse.Code != http.StatusOK {
			t.Fatalf("%s descriptor: %d %s", id, descriptorResponse.Code, descriptorResponse.Body.String())
		}
		var descriptor struct {
			Capabilities map[string]bool `json:"capabilities"`
			Collections  []string        `json:"collections"`
			Schemas      struct {
				Collections map[string]struct {
					Source struct {
						Schema string `json:"schema"`
						Name   string `json:"name"`
					} `json:"source"`
					Fields map[string]struct {
						PrimaryKey bool `json:"primaryKey"`
					} `json:"fields"`
				} `json:"collections"`
			} `json:"schemas"`
		}
		if err := json.Unmarshal(descriptorResponse.Body.Bytes(), &descriptor); err != nil {
			t.Fatalf("%s descriptor JSON: %v", id, err)
		}
		if descriptor.Capabilities["query"] || !descriptor.Capabilities["dtql"] || descriptor.Capabilities["write"] {
			t.Fatalf("%s advertised unsupported native capabilities: %+v", id, descriptor.Capabilities)
		}
		if len(descriptor.Collections) == 0 {
			t.Fatalf("%s has no discovered native relations", id)
		}
		hiddenID, err := schema.NativePostgresCollectionID(source.databaseID, "_import_manifest")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(descriptorResponse.Body.String(), "_import_manifest") || strings.Contains(descriptorResponse.Body.String(), hiddenID) {
			t.Fatalf("%s descriptor exposed internal provenance relation: %s", id, descriptorResponse.Body.String())
		}

		var joinCollectionID, joinField string
		readDone := false
		for _, collectionID := range descriptor.Collections {
			schemaName, tableName, decodeErr := schema.ParseNativePostgresCollectionID(collectionID)
			if decodeErr != nil || schemaName != source.databaseID || tableName == "_import_manifest" {
				t.Fatalf("%s returned collection ID %q resolved to %q.%q (%v)", id, collectionID, schemaName, tableName, decodeErr)
			}
			collection := descriptor.Schemas.Collections[collectionID]
			if collection.Source.Schema != source.databaseID || collection.Source.Name != tableName {
				t.Fatalf("%s collection %q has mismatched source metadata: %+v", id, collectionID, collection.Source)
			}
			fieldNames := make([]string, 0, len(collection.Fields))
			for fieldName := range collection.Fields {
				fieldNames = append(fieldNames, fieldName)
			}
			sort.Strings(fieldNames)
			if !readDone && len(fieldNames) > 0 {
				query := fmt.Sprintf("from: {schema: %s, name: %s}\ncolumns: [{field: %s}]\nlimit: 1\n", strconv.Quote(source.databaseID), strconv.Quote(tableName), strconv.Quote(fieldNames[0]))
				response := postgreSQLRequest(t, handler, http.MethodPost, dtqlPath, query, "https://datatug.app")
				if response.Code != http.StatusOK {
					t.Fatalf("%s read by returned collection ID %q: %d %s", id, collectionID, response.Code, response.Body.String())
				}
				if response.Header().Get("Access-Control-Allow-Origin") != "https://datatug.app" {
					t.Fatalf("%s DTQL response ACAO=%q, want DataTug origin", id, response.Header().Get("Access-Control-Allow-Origin"))
				}
				readDone = true
			}
			if joinCollectionID != "" {
				continue
			}
			for _, fieldName := range fieldNames {
				if collection.Fields[fieldName].PrimaryKey {
					joinCollectionID, joinField = collectionID, fieldName
					break
				}
			}
		}
		if joinCollectionID == "" {
			t.Fatalf("%s has no catalog relation with a primary key for the bounded self-join proof", id)
		}
		_, tableName, err := schema.ParseNativePostgresCollectionID(joinCollectionID)
		if err != nil {
			t.Fatal(err)
		}
		joinQuery := fmt.Sprintf("from:\n  schema: %s\n  name: %s\n  alias: l\n  joins:\n    - type: inner\n      from: {schema: %s, name: %s, alias: r}\n      on:\n        - {left: {field: %s, source: l}, op: '==', right: {field: %s, source: r}}\ncolumns: [{field: %s, source: l}]\nlimit: 1\n", strconv.Quote(source.databaseID), strconv.Quote(tableName), strconv.Quote(source.databaseID), strconv.Quote(tableName), strconv.Quote(joinField), strconv.Quote(joinField), strconv.Quote(joinField))
		joinResponse := postgreSQLRequest(t, handler, http.MethodPost, "/v1/databases/"+id+"/dtql", joinQuery)
		if joinResponse.Code != http.StatusOK {
			t.Fatalf("%s bounded self-join from collection %q: %d %s", id, joinCollectionID, joinResponse.Code, joinResponse.Body.String())
		}
		hiddenQuery := fmt.Sprintf("from: {schema: %s, name: %s}\ncolumns: [{field: source}]\nlimit: 1\n", strconv.Quote(source.databaseID), strconv.Quote("_import_manifest"))
		hiddenResponse := postgreSQLRequest(t, handler, http.MethodPost, "/v1/databases/"+id+"/dtql", hiddenQuery)
		if hiddenResponse.Code != http.StatusNotFound {
			t.Fatalf("%s hidden provenance query = %d %s, want 404", id, hiddenResponse.Code, hiddenResponse.Body.String())
		}
		legacyQuery := postgreSQLRequest(t, handler, http.MethodPost, "/v1/databases/"+id+"/query", `{"collection":"`+descriptor.Collections[0]+`"}`)
		if legacyQuery.Code != http.StatusNotImplemented {
			t.Fatalf("%s legacy query capability = %d %s, want 501", id, legacyQuery.Code, legacyQuery.Body.String())
		}
	}
}

func postgreSQLRequest(t *testing.T, handler http.Handler, method, path, query string, origins ...string) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if method == http.MethodPost {
		encoded, err := json.Marshal(struct {
			Query string `json:"query"`
		}{Query: query})
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(path, "/query") {
			encoded = []byte(query)
		}
		body = strings.NewReader(string(encoded))
	}
	request := httptest.NewRequest(method, path, body)
	if len(origins) > 0 {
		request.Header.Set("Origin", origins[0])
	}
	if method == http.MethodOptions {
		request.Header.Set("Access-Control-Request-Method", http.MethodPost)
		request.Header.Set("Access-Control-Request-Headers", "Content-Type")
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
