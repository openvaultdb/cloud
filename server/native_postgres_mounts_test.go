package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// captureLog redirects the standard logger for one test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buffer := &lockedBuffer{}
	previous := log.Writer()
	log.SetOutput(buffer)
	t.Cleanup(func() { log.SetOutput(previous) })
	return buffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// sqliteTestDatabase mounts a copy of the bundled Chinook SQLite fixture under id.
// It runs on mounter goroutines, so it reports errors instead of failing the test.
func sqliteTestDatabase(manifestPath string) (*core.Database, error) {
	return mount.File(manifestPath)
}

func sqliteTestManifest(t *testing.T, id string) string {
	t.Helper()
	dir := t.TempDir()
	source, err := os.ReadFile(filepath.Join("fixture", "Chinook_Sqlite.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chinook.sqlite"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte("database: {id: "+id+", schema_mode: strict}\nstorage: {engine: sqlite, path: chinook.sqlite}\n"+
		"schemas:\n  collections:\n    Album:\n      fields:\n        AlbumId: {type: integer}\n        Title: {type: string}\n        ArtistId: {type: integer}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return manifestPath
}

func testDemoPostgresProvider(databaseID string) runtimeDatabase {
	for _, source := range demoPostgresSources {
		if source.databaseID == databaseID {
			return runtimeDatabase{
				ID:             databaseID + "-postgresql",
				CORSOrigins:    demoPostgresCORSOrigins(databaseID),
				nativePostgres: &nativePostgresMountConfig{schemaName: databaseID, dsnEnv: source.dsnEnv},
			}
		}
	}
	panic("unknown DemoDB source " + databaseID)
}

func fastDemoPostgresTiming() demoPostgresMountTiming {
	return demoPostgresMountTiming{SelfCheck: time.Second, Attempt: 200 * time.Millisecond, FirstRetry: 20 * time.Millisecond, MaxRetry: 40 * time.Millisecond, Concurrency: 2}
}

func waitForDemoPostgresState(t *testing.T, m *demoPostgresMounter, id, want string) demoPostgresMountStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, status := range m.snapshot() {
			if status.ID == id && status.State == want {
				return status
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached %s: %+v", id, want, m.snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func serveGet(handler http.Handler, path string, headers ...string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestDemoPostgresMountRetriesWithoutRestart(t *testing.T) {
	logs := captureLog(t)
	provider := testDemoPostgresProvider("chinook")
	t.Setenv(provider.nativePostgres.dsnEnv, "postgres://reader:private-marker-1234@ep-x.neon.tech/chinook?sslmode=verify-full")
	target := server.New("test", nil)
	m := newDemoPostgresMounter(target, []runtimeDatabase{provider}, fastDemoPostgresTiming())
	var checks atomic.Int32
	m.check = func(context.Context, string, string) error {
		if checks.Add(1) < 3 {
			return errDemoPostgresUnreachable
		}
		return nil
	}
	manifestPath := sqliteTestManifest(t, provider.ID)
	m.mount = func(runtimeDatabase) (*core.Database, error) { return sqliteTestDatabase(manifestPath) }
	m.scope = func(*core.Database, runtimeDatabase) error { return nil } // the fake mount is SQLite
	handler := m.wrap(target.Handler())
	m.start([]runtimeDatabase{provider})
	t.Cleanup(func() { _ = m.stop() })

	if err := m.waitSettled(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForDemoPostgresState(t, m, provider.ID, demoPostgresAvailable)
	if got := checks.Load(); got != 3 {
		t.Fatalf("self-check ran %d times, want 3 (two failures, one success)", got)
	}
	if response := serveGet(handler, "/v1/databases/"+provider.ID); response.Code != http.StatusOK {
		t.Fatalf("mounted after retry: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(logs.String(), "private-marker-1234") {
		t.Fatalf("log disclosed the DSN: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "native PostgreSQL chinook-postgresql: unavailable (attempt 1)") || !strings.Contains(logs.String(), "mounted (attempt 3)") {
		t.Fatalf("log does not record the failed and successful attempts: %s", logs.String())
	}
}

func TestDemoPostgresSlowMountIsAbandonedAndClosed(t *testing.T) {
	captureLog(t)
	provider := testDemoPostgresProvider("pubs")
	t.Setenv(provider.nativePostgres.dsnEnv, "postgres://reader@ep-x.neon.tech/pubs?sslmode=verify-full")
	target := server.New("test", nil)
	m := newDemoPostgresMounter(target, []runtimeDatabase{provider}, fastDemoPostgresTiming())
	m.timing.FirstRetry, m.timing.MaxRetry = time.Hour, time.Hour
	m.check = func(context.Context, string, string) error { return nil }
	release := make(chan struct{})
	closed := make(chan struct{})
	manifestPath := sqliteTestManifest(t, provider.ID)
	m.mount = func(runtimeDatabase) (*core.Database, error) {
		<-release
		db, err := sqliteTestDatabase(manifestPath)
		if err != nil {
			return nil, err
		}
		db.OnClose(func() error { close(closed); return nil })
		return db, nil
	}
	m.start([]runtimeDatabase{provider})
	t.Cleanup(func() { _ = m.stop() })
	status := waitForDemoPostgresState(t, m, provider.ID, demoPostgresUnavailable)
	if !strings.Contains(status.Reason, "did not finish") || status.NextRetry == nil {
		t.Fatalf("slow mount status = %+v", status)
	}
	close(release)
	select {
	case <-closed: // a late mount is closed, never served
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned mount was not closed")
	}
	if response := serveGet(m.wrap(target.Handler()), "/v1/databases/"+provider.ID); response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" {
		t.Fatalf("abandoned mount is served: %d %v", response.Code, response.Header())
	}
}

func TestDemoPostgresConfigurationRefusalsAreNotRetried(t *testing.T) {
	captureLog(t)
	t.Setenv(demoPostgresInsecureTLSEnv, "")
	chinook, pubs := testDemoPostgresProvider("chinook"), testDemoPostgresProvider("pubs")
	t.Setenv(chinook.nativePostgres.dsnEnv, "postgres://reader:pw@ep-x.neon.tech/chinook?sslmode=require")
	t.Setenv(pubs.nativePostgres.dsnEnv, "")
	target := server.New("test", nil)
	m := newDemoPostgresMounter(target, []runtimeDatabase{chinook, pubs}, fastDemoPostgresTiming())
	m.check = func(context.Context, string, string) error {
		t.Error("self-check ran for a refused configuration")
		return nil
	}
	m.start([]runtimeDatabase{chinook, pubs})
	t.Cleanup(func() { _ = m.stop() })
	if err := m.waitSettled(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond) // longer than the retry delay
	statuses := m.snapshot()
	if len(statuses) != 2 ||
		statuses[0].ID != "chinook-postgresql" || statuses[0].State != demoPostgresMisconfigured || statuses[0].Attempts != 1 || statuses[0].NextRetry != nil || statuses[0].Reason != errDemoPostgresTLS.Error() ||
		statuses[1].ID != "pubs-postgresql" || statuses[1].State != demoPostgresNotConfigured || statuses[1].Attempts != 1 {
		t.Fatalf("statuses = %+v", statuses)
	}
}

// TestDemoPostgresBrokenSourceLeavesSQLiteServing is the startup-coupling proof:
// with the PostgreSQL opt-in on, one unreachable DSN and five unset ones, the
// server starts at once, the SQLite database answers queries, the broken source
// is reported and refused with 503, and PostgreSQL is not advertised.
func TestDemoPostgresBrokenSourceLeavesSQLiteServing(t *testing.T) {
	logs := captureLog(t)
	t.Setenv(demoPostgresEnabledEnv, "true")
	t.Setenv(demoPostgresInsecureTLSEnv, "")
	for _, source := range demoPostgresSources {
		t.Setenv(source.dsnEnv, "")
	}
	// Port 1 on loopback refuses the connection at once.
	t.Setenv("OVDB_PG_CHINOOK_DSN", "postgres://reader:private-marker-5678@127.0.0.1:1/chinook?sslmode=verify-full&connect_timeout=2")
	providers, err := appendConfiguredDemoPostgres([]runtimeDatabase{{ID: "chinook", Manifest: sqliteTestManifest(t, "chinook"), CORSOrigins: legacyCORSOrigins("chinook")}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	handler, closeDatabases, err := newHandlerWithProviders(providers)
	if err != nil {
		t.Fatalf("startup failed because of PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = closeDatabases() })
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("startup waited %s for PostgreSQL", elapsed)
	}
	pgHandler, ok := handler.(*demoPostgresHandler)
	if !ok {
		t.Fatalf("handler %T does not expose the PostgreSQL mounter", handler)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := pgHandler.mounter.waitSettled(ctx); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/v1/databases/chinook", "/ovdb/dbs/chinook/collections/Album"} {
		if response := serveGet(handler, path); response.Code != http.StatusOK {
			t.Fatalf("SQLite %s with a broken PostgreSQL source: %d %s", path, response.Code, response.Body.String())
		}
	}
	status := serveGet(handler, demoPostgresStatusPath)
	var report struct {
		Mounts []demoPostgresMountStatus `json:"mounts"`
	}
	if status.Code != http.StatusOK || json.Unmarshal(status.Body.Bytes(), &report) != nil || len(report.Mounts) != len(demoPostgresSources) {
		t.Fatalf("status route: %d %s", status.Code, status.Body.String())
	}
	for _, mount := range report.Mounts {
		want := demoPostgresNotConfigured
		if mount.ID == "chinook-postgresql" {
			want = demoPostgresUnavailable
		}
		if mount.State != want {
			t.Errorf("%s state = %s (%s), want %s", mount.ID, mount.State, mount.Reason, want)
		}
	}
	refused := serveGet(handler, "/v1/databases/chinook-postgresql/collections", "Origin", "https://datatug.app")
	if refused.Code != http.StatusServiceUnavailable || refused.Header().Get("Retry-After") == "" || refused.Header().Get("Access-Control-Allow-Origin") != "https://datatug.app" {
		t.Fatalf("broken source request: %d %v %s", refused.Code, refused.Header(), refused.Body.String())
	}
	assertCloudQueryDiscovery(t, handler)
	for _, text := range []string{logs.String(), status.Body.String(), refused.Body.String()} {
		if strings.Contains(text, "private-marker-5678") || strings.Contains(text, "127.0.0.1:1") {
			t.Fatalf("output disclosed the DSN: %s", text)
		}
	}
	if !strings.Contains(logs.String(), "native PostgreSQL chinook-postgresql: unavailable (attempt 1)") {
		t.Fatalf("broken source was not logged: %s", logs.String())
	}
}

func TestDemoPostgresStatusRouteMethods(t *testing.T) {
	m := newDemoPostgresMounter(server.New("test", nil), nil, fastDemoPostgresTiming())
	handler := m.wrap(http.NotFoundHandler())
	request := httptest.NewRequest(http.MethodPost, demoPostgresStatusPath, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status route = %d", response.Code)
	}
	if err := m.waitSettled(context.Background()); err != nil {
		t.Fatal("a mounter without sources must be settled")
	}
	body, _ := io.ReadAll(serveGet(handler, demoPostgresStatusPath).Body)
	if strings.TrimSpace(string(body)) != `{"mounts":[]}` {
		t.Fatalf("empty status = %s", body)
	}
	if err := m.stop(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(context.Cause(m.ctx), context.Canceled) {
		t.Fatal("stop did not cancel retries")
	}
}
