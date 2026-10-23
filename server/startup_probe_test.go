package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func probeStatus(t *testing.T, h http.Handler, method string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, "http://instance"+startupProbePath, nil))
	return w
}

// A fresh instance reports "starting" while an edition is pending and "ready"
// once every edition has settled, whether it mounted, failed (and is retried in
// the background) or was refused by configuration.
func TestStartupProbeWaitsForPendingThenSettledMounts(t *testing.T) {
	captureLog(t)
	available := testDemoPostgresProvider("chinook")
	failed := testDemoPostgresProvider("northwind")
	misconfigured := testDemoPostgresProvider("pubs")
	t.Setenv(available.nativePostgres.dsnEnv, "postgres://reader:private-marker-1234@ep-x.neon.tech/chinook?sslmode=verify-full&connect_timeout=10")
	t.Setenv(failed.nativePostgres.dsnEnv, "postgres://reader:private-marker-1234@ep-x.neon.tech/northwind?sslmode=verify-full&connect_timeout=10")
	t.Setenv(misconfigured.nativePostgres.dsnEnv, "postgres://reader:private-marker-1234@ep-x.neon.tech/pubs?sslmode=disable&connect_timeout=10")
	providers := []runtimeDatabase{available, failed, misconfigured}
	target := server.New("test", nil)
	m := newDemoPostgresMounter(target, providers, fastDemoPostgresTiming())
	release := make(chan struct{})
	m.check = func(ctx context.Context, _ string, schemaName string) error {
		if schemaName == "northwind" {
			return errDemoPostgresUnreachable
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	manifestPath := sqliteTestManifest(t, available.ID)
	m.mount = func(runtimeDatabase) (*core.Database, error) { return sqliteTestDatabase(manifestPath) }
	m.scope = func(*core.Database, runtimeDatabase) error { return nil } // the fake mount is SQLite
	probe := newStartupProbe(m.settled)
	h := probe.wrap(m.wrap(target.Handler()))
	m.start(providers)
	t.Cleanup(func() { _ = m.stop() })

	waitForDemoPostgresState(t, m, failed.ID, demoPostgresUnavailable)
	waitForDemoPostgresState(t, m, misconfigured.ID, demoPostgresMisconfigured)
	w := probeStatus(t, h, http.MethodGet)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"starting"`) {
		t.Fatalf("pending edition: got %d %q, want 503 starting", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.waitSettled(ctx); err != nil {
		t.Fatal(err)
	}
	waitForDemoPostgresState(t, m, available.ID, demoPostgresAvailable)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w = probeStatus(t, h, method)
		if w.Code != http.StatusOK {
			t.Fatalf("%s settled: got %d, want 200", method, w.Code)
		}
	}
	if body := probeStatus(t, h, http.MethodGet).Body.String(); body != `{"status":"ready"}`+"\n" {
		t.Fatalf("ready body = %q", body)
	}
	// The failed edition still answers 503 database_unavailable after startup.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/databases/"+failed.ID, nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed edition after startup: got %d, want 503", w.Code)
	}
	// Other routes pass through untouched.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, demoPostgresStatusPath, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"mounts"`) {
		t.Fatalf("status route: got %d %q", w.Code, w.Body.String())
	}
}

// A slow edition cannot hold startup past the budget: Cloud Run would otherwise
// kill the instance and start over.
func TestStartupProbeReportsReadyAfterBudget(t *testing.T) {
	logs := captureLog(t)
	now := time.Unix(1000, 0)
	probe := &startupProbe{settled: make(chan struct{}), deadline: now.Add(startupProbeBudget), now: func() time.Time { return now }}
	h := probe.wrap(http.NotFoundHandler())
	if got := probeStatus(t, h, http.MethodGet).Code; got != http.StatusServiceUnavailable {
		t.Fatalf("before budget: got %d, want 503", got)
	}
	now = now.Add(startupProbeBudget)
	for range 2 {
		if got := probeStatus(t, h, http.MethodGet).Code; got != http.StatusOK {
			t.Fatalf("after budget: got %d, want 200", got)
		}
	}
	if got := strings.Count(logs.String(), "did not settle within"); got != 1 {
		t.Fatalf("budget log written %d times, want once: %q", got, logs.String())
	}
}

func TestStartupProbeWithoutNativePostgresIsReadyAndRefusesWrites(t *testing.T) {
	probe := newStartupProbe(nil)
	if want := processStartedAt.Add(startupProbeBudget); !probe.deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v", probe.deadline, want)
	}
	h := probe.wrap(http.NotFoundHandler())
	if got := probeStatus(t, h, http.MethodGet).Code; got != http.StatusOK {
		t.Fatalf("got %d, want 200", got)
	}
	w := probeStatus(t, h, http.MethodPost)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST: got %d Allow=%q", w.Code, w.Header().Get("Allow"))
	}
	// The production handler without native PostgreSQL answers the probe too.
	handler, closeHandler, err := newHandler(sqliteTestManifest(t, "chinook"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeHandler() })
	if got := probeStatus(t, handler, http.MethodGet).Code; got != http.StatusOK {
		t.Fatalf("SQLite-only handler: got %d, want 200", got)
	}
}

// The probe cannot carry the edge credential, so the gateway admits exactly one
// credential-free request shape and strips everything the client sent.
func TestGatewayAdmitsOnlyTheStartupProbeWithoutCredential(t *testing.T) {
	var seen []*http.Request
	inner := newStartupProbe(nil).wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		w.WriteHeader(http.StatusTeapot)
	}))
	h := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		inner.ServeHTTP(w, r)
	}), nil)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r := httptest.NewRequest(method, "https://"+cloudHost+startupProbePath, strings.NewReader("ignored"))
		r.Header.Set("Authorization", "Bearer private")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s probe without credential: got %d, want 200", method, w.Code)
		}
	}
	for _, r := range seen {
		if len(r.Header) != 0 || r.ContentLength != 0 || r.Body != http.NoBody {
			t.Fatalf("probe reached inner with client data: %+v", r.Header)
		}
	}
	seen = nil
	for _, c := range []struct{ method, target string }{
		{http.MethodPost, startupProbePath},
		{http.MethodGet, startupProbePath + "?x=1"},
		{http.MethodGet, startupProbePath + "?"},
		{http.MethodGet, "/healthz/startup/"},
		{http.MethodGet, "/healthz%2Fstartup"},
		{http.MethodGet, "/v1"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, "https://"+cloudHost+c.target, nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s without credential: got %d, want 403", c.method, c.target, w.Code)
		}
	}
	if len(seen) != 0 {
		t.Fatal("non-probe request reached inner without credential")
	}
}

func TestGatewayRejectsInvalidOriginSecret(t *testing.T) {
	if _, err := newGateway(http.NotFoundHandler(), func(name string) string {
		if name == originSecretEnv {
			return "short"
		}
		return ""
	}, nil); err == nil {
		t.Fatal("invalid origin credential accepted")
	}
}
