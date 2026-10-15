package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"github.com/openvaultdb/openvaultdb-go/pkg/server/servertest"
)

func TestAppendConfiguredDemoPostgresIsOptInAndPerMount(t *testing.T) {
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
	// A partial configuration is not a startup error: every source is listed and
	// the background mounter reports the unset ones as not_configured.
	got, err = appendConfiguredDemoPostgres(base)
	if err != nil || len(got) != 1+len(demoPostgresSources) || got[0].ID != "chinook" {
		t.Fatalf("partial secret configuration = %#v, %v", got, err)
	}
	t.Setenv(demoPostgresEnabledEnv, "maybe")
	if _, err := appendConfiguredDemoPostgres(base); err == nil || !strings.Contains(err.Error(), demoPostgresEnabledEnv) {
		t.Fatalf("invalid opt-in value error = %v", err)
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

func TestPostgreSQLRequestSupportsBodylessMethods(t *testing.T) {
	for _, method := range []string{http.MethodOptions, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != method || r.Body != http.NoBody || r.ContentLength != 0 {
					t.Errorf("bodyless request = method %q, body %T, content length %d", r.Method, r.Body, r.ContentLength)
				}
				w.WriteHeader(http.StatusNoContent)
			})
			response := postgreSQLRequest(t, handler, method, "/v1/databases/chinook-postgresql", "", "https://datatug.app")
			if response.Code != http.StatusNoContent {
				t.Fatalf("bodyless %s request status = %d", method, response.Code)
			}
		})
	}
}

// demoPostgresJourneyHandler mounts the six configured sources, waits for the
// background mounter, and requires every source to be available.
func demoPostgresJourneyHandler(t *testing.T) (http.Handler, uint64) {
	t.Helper()
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
	pgHandler, ok := handler.(*demoPostgresHandler)
	if !ok {
		t.Fatalf("handler %T does not expose the PostgreSQL mounter", handler)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := pgHandler.mounter.waitSettled(ctx); err != nil {
		t.Fatal(err)
	}
	for _, status := range pgHandler.mounter.snapshot() {
		if status.State != demoPostgresAvailable {
			t.Fatalf("%s is %s: %s", status.ID, status.State, status.Reason)
		}
	}
	runtime.GC()
	var afterMount runtime.MemStats
	runtime.ReadMemStats(&afterMount)
	var mountedHeapBytes uint64
	if afterMount.HeapAlloc > beforeMount.HeapAlloc {
		mountedHeapBytes = afterMount.HeapAlloc - beforeMount.HeapAlloc
	}
	return handler, mountedHeapBytes
}

// openvaultdbGoVersion reads the pinned library version from go.mod
// (debug.ReadBuildInfo is empty for a library's own tests).
func openvaultdbGoVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "github.com/openvaultdb/openvaultdb-go" {
			return fields[1]
		}
	}
	t.Fatal("go.mod does not pin github.com/openvaultdb/openvaultdb-go")
	return ""
}

// knownLongNameCollectionFailures lists, per pinned openvaultdb-go version, the
// relations whose every read is known to fail. openvaultdb-go v0.24.1 refused 46
// relations whose encoded collection ID is longer than 63 bytes (43
// AdventureWorks relations and three Northwind views); v0.25.2 fixed it, so no
// version is listed and the all-collections journey is strict. If an entry is
// ever added, the journey still fails once a listed relation starts reading.
var knownLongNameCollectionFailures = map[string]map[string]bool{}

func TestDemoPostgresReadOnlyPublicJourney(t *testing.T) {
	handler, mountedHeapBytes := demoPostgresJourneyHandler(t)
	t.Logf("six native PostgreSQL mounts, pools and discovered catalogs retain %d additional heap bytes", mountedHeapBytes)
	if mountedHeapBytes > 64<<20 {
		t.Fatalf("six native PostgreSQL mounts exceed the 64 MiB at-rest heap allowance: %d", mountedHeapBytes)
	}
	libraryVersion := openvaultdbGoVersion(t)
	expectedFailures := knownLongNameCollectionFailures[libraryVersion]
	failed := map[string]string{}
	read := 0

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
		if !descriptor.Capabilities["dtqlStreaming"] || !descriptor.Capabilities["dtqlStreamingErrors"] {
			t.Fatalf("%s did not advertise its negotiated streaming DTQL capabilities", id)
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

		// Every collection is read, not only the first: the first-only read is why
		// relations with long names were never caught.
		var joinCollectionID, joinField string
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
			if len(fieldNames) == 0 {
				failed[schemaName+"."+tableName] = "no fields in the descriptor"
				continue
			}
			read++
			query := fmt.Sprintf("from: {schema: %s, name: %s}\ncolumns: [{field: %s}]\nlimit: 1\n", strconv.Quote(source.databaseID), strconv.Quote(tableName), strconv.Quote(fieldNames[0]))
			response := postgreSQLStreamRequest(t, handler, dtqlPath, query, "https://datatug.app")
			var streamed struct {
				Complete bool `json:"complete"`
			}
			switch {
			case response.Code != http.StatusOK:
				failed[schemaName+"."+tableName] = fmt.Sprintf("%d %s", response.Code, strings.TrimSpace(response.Body.String()))
				continue
			case response.Header().Get("Content-Type") != "application/vnd.openvaultdb.query-stream+json":
				t.Fatalf("%s opted-in DTQL Content-Type = %q", id, response.Header().Get("Content-Type"))
			case !strings.Contains(strings.Join(response.Header().Values("Vary"), ", "), "Accept"):
				t.Fatalf("%s opted-in DTQL Vary = %q, want Accept", id, response.Header().Values("Vary"))
			case response.Header().Get("Access-Control-Allow-Origin") != "https://datatug.app":
				t.Fatalf("%s DTQL response ACAO=%q, want DataTug origin", id, response.Header().Get("Access-Control-Allow-Origin"))
			case json.Unmarshal(response.Body.Bytes(), &streamed) != nil || !streamed.Complete:
				failed[schemaName+"."+tableName] = "stream did not complete: " + strings.TrimSpace(response.Body.String())
				continue
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
		keyedRead := postgreSQLRequest(t, handler, http.MethodGet, "/v1/databases/"+id+"/records/"+descriptor.Collections[0]+"/1", "")
		if keyedRead.Code != http.StatusNotImplemented || !strings.Contains(keyedRead.Body.String(), "operation_unsupported") {
			t.Fatalf("%s keyed record read = %d %s, want 501 operation_unsupported (openvaultdb-go v0.25.5; v0.25.2 answered 500 internal)", id, keyedRead.Code, keyedRead.Body.String())
		}
	}

	t.Logf("read every collection: %d reads, %d failed", read, len(failed))
	names := make([]string, 0, len(failed))
	for name := range failed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if expectedFailures[name] {
			t.Logf("expected failure with openvaultdb-go %s: %s: %s", libraryVersion, name, failed[name])
			continue
		}
		t.Errorf("collection %s: %s", name, failed[name])
	}
	for name := range expectedFailures {
		if _, stillFails := failed[name]; !stillFails {
			t.Errorf("collection %s is listed as a known failure for openvaultdb-go %s but now reads: remove it from knownLongNameCollectionFailures", name, libraryVersion)
		}
	}
}

// TestDemoPostgresEveryCollectionBattery runs the library's client-side battery
// (servertest.RequireEveryCollection: bounded and streamed reads, ordering by
// each field, equality filters on sampled values, offset pages, out-of-range
// integer refusals) on every collection of all six editions, through this
// service's own handler.
func TestDemoPostgresEveryCollectionBattery(t *testing.T) {
	handler, _ := demoPostgresJourneyHandler(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for _, source := range demoPostgresSources {
		t.Run(source.databaseID, func(t *testing.T) {
			report := servertest.RequireEveryCollection(t, ctx, server.URL, source.databaseID+"-postgresql", servertest.Options{Client: server.Client()})
			if report.Collections == 0 {
				t.Fatalf("%s: the battery found no collections", source.databaseID)
			}
		})
	}
}

func postgreSQLRequest(t *testing.T, handler http.Handler, method, path, query string, origins ...string) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
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

func postgreSQLStreamRequest(t *testing.T, handler http.Handler, path, query string, origins ...string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Query string `json:"query"`
	}{Query: query})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/vnd.openvaultdb.query-stream+json")
	if len(origins) > 0 {
		request.Header.Set("Origin", origins[0])
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

// TestNativePostgresMountExposesFullPublicDTQL states, by behaviour, what turning
// on OVDB_DEMODB_POSTGRES_ENABLED publishes. The library's preview switch is not
// involved: a native read-only mount answers core.CanQuery() itself, so discovery
// adds postgres to the join engines, and the public gets full DTQL (filters,
// ordering, equality joins, grouping with HAVING, subqueries). Single-source reads
// and joins run inside PostgreSQL; a document with a subquery runs in the
// server's in-memory engine. Writes and the legacy /query route stay refused.
func TestNativePostgresMountExposesFullPublicDTQL(t *testing.T) {
	if os.Getenv("OVDB_PG_CHINOOK_DSN") == "" {
		t.Skip("set OVDB_PG_CHINOOK_DSN to a read-only role on a loaded chinook schema")
	}
	t.Setenv(demoPostgresEnabledEnv, "true")
	for _, source := range demoPostgresSources[1:] {
		t.Setenv(source.dsnEnv, "")
	}
	providers, err := appendConfiguredDemoPostgres(nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, closeDatabases, err := newHandlerWithProviders(providers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDatabases() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	mounter := handler.(*demoPostgresHandler).mounter
	if err := mounter.waitSettled(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _ := mounter.state("chinook-postgresql"); state != demoPostgresAvailable {
		t.Fatalf("chinook-postgresql is %s: %+v", state, mounter.snapshot())
	}
	assertCloudQueryDiscovery(t, handler, true)

	const db = "chinook-postgresql"
	for _, c := range []struct {
		name, doc, route string
		minRows          int
	}{
		{"filter and order", "from: {schema: chinook, name: Album}\nwhere: {op: '==', left: {field: ArtistId}, right: {value: 90}}\norderBy: [{field: Title, desc: true}]\ncolumns: [{field: Title}]\n", "database", 2},
		{"equality join", "from:\n  schema: chinook\n  name: Album\n  alias: a\n  joins:\n    - type: inner\n      from: {schema: chinook, name: Artist, alias: r}\n      on: [{left: {field: ArtistId, source: a}, op: '==', right: {field: ArtistId, source: r}}]\ncolumns: [{field: Title, source: a}, {field: Name, source: r}]\nlimit: 5\n", "database", 5},
		{"grouping with HAVING", "from: {schema: chinook, name: Album}\ngroupBy: [{field: ArtistId}]\nhaving: {op: '>=', left: {aggregate: {function: count, args: [{star: true}]}}, right: {value: 5}}\ncolumns: [{field: ArtistId}, {aggregate: {function: count, args: [{star: true}]}, as: albums}]\n", "database", 1},
		{"IN subquery", "from: {schema: chinook, name: Artist, alias: r}\nwhere:\n  op: In\n  left: {field: ArtistId, source: r}\n  right:\n    query:\n      from: {schema: chinook, name: Album, alias: a}\n      where: {op: '==', left: {field: AlbumId, source: a}, right: {value: 1}}\n      columns: [{field: ArtistId, source: a}]\ncolumns: [{field: Name, source: r}]\n", "in-memory", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			response := postgreSQLRequest(t, handler, http.MethodPost, "/v1/databases/"+db+"/dtql", c.doc)
			if response.Code != http.StatusOK {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
			var answer struct {
				Records   []json.RawMessage `json:"records"`
				Execution struct {
					Route string `json:"route"`
				} `json:"execution"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
				t.Fatal(err)
			}
			if len(answer.Records) < c.minRows || answer.Execution.Route != c.route {
				t.Fatalf("rows=%d route=%q, want >=%d rows on route %q: %s", len(answer.Records), answer.Execution.Route, c.minRows, c.route, response.Body.String())
			}
		})
	}
	albumID, err := schema.NativePostgresCollectionID("chinook", "Album")
	if err != nil {
		t.Fatal(err)
	}
	keyedRead := postgreSQLRequest(t, handler, http.MethodGet, "/v1/databases/"+db+"/records/"+albumID+"/1", "")
	if keyedRead.Code != http.StatusNotImplemented || !strings.Contains(keyedRead.Body.String(), "operation_unsupported") {
		t.Errorf("GET keyed record = %d %s, want 501 operation_unsupported, not 500", keyedRead.Code, keyedRead.Body.String())
	}
	for _, write := range []struct{ method, path, body string }{
		{http.MethodPut, "/v1/databases/" + db + "/records/" + albumID + "/1", `{"Title":"x"}`},
		{http.MethodPost, "/v1/databases/" + db + "/batch", `{"ops":[{"op":"delete","key":"` + albumID + `/1"}]}`},
	} {
		request := httptest.NewRequest(write.method, write.path, strings.NewReader(write.body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 400 || response.Code >= 500 {
			t.Errorf("%s %s = %d %s, want a 4xx refusal", write.method, write.path, response.Code, response.Body.String())
		}
	}
}
