package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
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
	t.Setenv(provider.nativePostgres.dsnEnv, "postgres://reader:private-marker-1234@ep-x.neon.tech/chinook?sslmode=verify-full&connect_timeout=10")
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
	t.Setenv(provider.nativePostgres.dsnEnv, "postgres://reader@ep-x.neon.tech/pubs?sslmode=verify-full&connect_timeout=10")
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
	if status.Reason != "timeout" || status.NextRetry == nil {
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
	t.Setenv(chinook.nativePostgres.dsnEnv, "postgres://reader:pw@ep-x.neon.tech/chinook?sslmode=require&connect_timeout=10")
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
		statuses[0].ID != "chinook-postgresql" || statuses[0].State != demoPostgresMisconfigured || statuses[0].Attempts != 1 || statuses[0].NextRetry != nil || statuses[0].Reason != "tls_required" ||
		statuses[1].ID != "pubs-postgresql" || statuses[1].State != demoPostgresNotConfigured || statuses[1].Attempts != 1 || statuses[1].Reason != "not_configured" {
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
		if mount.ID == "chinook-postgresql" && mount.Reason != "connection_failed" {
			t.Errorf("public reason = %q, want the stable code connection_failed", mount.Reason)
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

// TestDemoPostgresAbandonedMountKeepsItsSlot: with one slot, a mount abandoned
// after its attempt timeout keeps the slot until it really ends, so the second
// source cannot start a concurrent real mount; stop() waits for the late close.
func TestDemoPostgresAbandonedMountKeepsItsSlot(t *testing.T) {
	captureLog(t)
	first, second := testDemoPostgresProvider("chinook"), testDemoPostgresProvider("pubs")
	t.Setenv(first.nativePostgres.dsnEnv, "postgres://r@ep-x.neon.tech/chinook?sslmode=verify-full&connect_timeout=10")
	t.Setenv(second.nativePostgres.dsnEnv, "postgres://r@ep-x.neon.tech/pubs?sslmode=verify-full&connect_timeout=10")
	timing := fastDemoPostgresTiming()
	timing.Concurrency, timing.FirstRetry, timing.MaxRetry = 1, time.Hour, time.Hour
	m := newDemoPostgresMounter(server.New("test", nil), []runtimeDatabase{first, second}, timing)
	m.check = func(context.Context, string, string) error { return nil }
	m.scope = func(*core.Database, runtimeDatabase) error { return nil }
	release := make(chan struct{})
	var running, maxRunning atomic.Int32
	var closedLate atomic.Bool
	manifests := map[string]string{first.ID: sqliteTestManifest(t, first.ID), second.ID: sqliteTestManifest(t, second.ID)}
	m.mount = func(p runtimeDatabase) (*core.Database, error) {
		now := running.Add(1)
		defer running.Add(-1)
		for {
			old := maxRunning.Load()
			if now <= old || maxRunning.CompareAndSwap(old, now) {
				break
			}
		}
		if p.ID == first.ID {
			<-release // a hung dial
			db, err := sqliteTestDatabase(manifests[p.ID])
			if err == nil {
				db.OnClose(func() error { closedLate.Store(true); return nil })
			}
			return db, err
		}
		return sqliteTestDatabase(manifests[p.ID])
	}
	m.start([]runtimeDatabase{first})
	waitForDemoPostgresState(t, m, first.ID, demoPostgresUnavailable)
	m.start([]runtimeDatabase{second}) // only now, so the abandoned mount surely holds the slot
	time.Sleep(100 * time.Millisecond)
	if state, _ := m.state(second.ID); state != demoPostgresPending {
		t.Fatalf("second source ran while the abandoned mount still held the only slot: %s", state)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- m.stop() }()
	select {
	case <-stopped:
		t.Fatal("stop returned before the abandoned mount ended")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return after the abandoned mount ended")
	}
	if !closedLate.Load() || maxRunning.Load() > 1 {
		t.Fatalf("late mount closed=%v, concurrent real mounts=%d", closedLate.Load(), maxRunning.Load())
	}
}

// TestDemoPostgresHungDialEndsWithConnectTimeout uses the real mount against a
// listener that accepts TCP and never answers: the DSN's required connect_timeout
// ends the abandoned mount, which then frees its slot.
func TestDemoPostgresHungDialEndsWithConnectTimeout(t *testing.T) {
	captureLog(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns []net.Conn
	var connsMu sync.Mutex
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, conn)
			connsMu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		connsMu.Lock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		connsMu.Unlock()
	})
	t.Setenv(demoPostgresInsecureTLSEnv, "true")
	provider := testDemoPostgresProvider("chinook")
	t.Setenv(provider.nativePostgres.dsnEnv, "postgres://reader@"+listener.Addr().String()+"/chinook?sslmode=disable&connect_timeout=1")
	timing := fastDemoPostgresTiming()
	timing.Concurrency, timing.FirstRetry, timing.MaxRetry = 1, time.Hour, time.Hour
	m := newDemoPostgresMounter(server.New("test", nil), []runtimeDatabase{provider}, timing)
	m.check = func(context.Context, string, string) error { return nil } // reach the real mount
	m.start([]runtimeDatabase{provider})
	status := waitForDemoPostgresState(t, m, provider.ID, demoPostgresUnavailable)
	if status.Reason != "timeout" {
		t.Fatalf("hung dial status = %+v", status)
	}
	select {
	case m.slots <- struct{}{}:
		t.Fatal("the slot was free while the hung mount was still dialling")
	default:
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(m.slots) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("connect_timeout did not end the abandoned mount")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := m.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestDemoPostgresAttemptBudgetComesFromTheEnvironment(t *testing.T) {
	env := func(value string) func(string) string {
		return func(key string) string {
			if key != demoPostgresAttemptTimeoutEnv {
				t.Errorf("read %s", key)
			}
			return value
		}
	}
	timing, err := demoPostgresMountTimingFromEnv(env(""))
	if err != nil || timing != defaultDemoPostgresMountTiming() || timing.Attempt != 90*time.Second || timing.Discovery != 60*time.Second {
		t.Fatalf("default timing = %+v, %v", timing, err)
	}
	timing, err = demoPostgresMountTimingFromEnv(env(" 3m "))
	if err != nil || timing.Attempt != 3*time.Minute || timing.Discovery != 150*time.Second || timing.SelfCheck != 20*time.Second || timing.Concurrency != 2 {
		t.Fatalf("configured timing = %+v, %v", timing, err)
	}
	for _, value := range []string{"90", "soon", "59s", "10m1s", "-2m"} {
		if _, err := demoPostgresMountTimingFromEnv(env(value)); !errors.Is(err, errDemoPostgresAttemptTimeout) || !strings.Contains(err.Error(), demoPostgresAttemptTimeoutEnv+" must be a duration between 1m0s and 10m0s") {
			t.Errorf("%q: err = %v", value, err)
		}
	}
	for _, value := range []string{"1m", "10m"} {
		if _, err := demoPostgresMountTimingFromEnv(env(value)); err != nil {
			t.Errorf("%q: err = %v", value, err)
		}
	}
}

func TestDemoPostgresMountOptionsCarryTheDiscoveryDeadline(t *testing.T) {
	provider := runtimeDatabase{ID: "chinook-postgresql", nativePostgres: &nativePostgresMountConfig{schemaName: "chinook", dsnEnv: "OVDB_PG_CHINOOK_DSN"}}
	options, err := demoPostgresMountOptionsWithin(provider, 150*time.Second)
	if err != nil || options.NativePostgresDiscoveryTimeout != 150*time.Second || !options.RequireReadOnlyNativePostgresRole {
		t.Fatalf("options = %+v, %v", options, err)
	}
	if _, err := demoPostgresMountOptionsWithin(runtimeDatabase{ID: "x"}, time.Minute); err == nil {
		t.Fatal("an invalid provider was given mount options")
	}
}

func TestHandlerRefusesAnInvalidAttemptBudget(t *testing.T) {
	t.Setenv(demoPostgresAttemptTimeoutEnv, "forever")
	t.Setenv(demoPostgresEnabledEnv, "true")
	for _, source := range demoPostgresSources {
		t.Setenv(source.dsnEnv, "")
	}
	providers, err := appendConfiguredDemoPostgres([]runtimeDatabase{{ID: "chinook", Manifest: sqliteTestManifest(t, "chinook"), CORSOrigins: legacyCORSOrigins("chinook")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := newHandlerWithProviders(providers); !errors.Is(err, errDemoPostgresAttemptTimeout) {
		t.Fatalf("err = %v", err)
	}
}

func TestDemoPostgresEnvOverrideReachesTheLibraryMount(t *testing.T) {
	provider := testDemoPostgresProvider("chinook")
	t.Setenv(provider.nativePostgres.dsnEnv, "postgres://reader:private-marker-1234@ep-x.neon.tech/chinook?sslmode=verify-full&connect_timeout=10")
	timing, err := demoPostgresMountTimingFromEnv(func(string) string { return "1m" })
	if err != nil || timing.Discovery != 30*time.Second {
		t.Fatalf("timing = %+v, %v", timing, err)
	}
	var got []time.Duration
	previous := mountFileWithOptions
	mountFileWithOptions = func(_ string, options mount.Options) (*core.Database, error) {
		got = append(got, options.NativePostgresDiscoveryTimeout)
		return nil, errors.New("stub")
	}
	t.Cleanup(func() { mountFileWithOptions = previous })
	m := newDemoPostgresMounter(server.New("test", nil), []runtimeDatabase{provider}, timing)
	t.Cleanup(func() { _ = m.stop() })
	if _, err := m.mount(provider); err == nil || err.Error() != "stub" {
		t.Fatalf("mount err = %v", err)
	}
	if len(got) != 1 || got[0] != 30*time.Second {
		t.Fatalf("library discovery deadline = %v, want the overridden 30s (default is 60s)", got)
	}
}
