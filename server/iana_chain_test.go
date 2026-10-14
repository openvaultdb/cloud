package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This test owns the loopback listener while the Worker test sends real HTTP
// requests through createWorker().fetch. It is opt-in because Go-only test runs
// should not require the JavaScript toolchain.
func TestIANASelectedCrossRuntimeChain(t *testing.T) {
	if os.Getenv("OVDB_IANA_CROSS_RUNTIME") != "1" {
		t.Skip("set OVDB_IANA_CROSS_RUNTIME=1 to run the local Worker-to-Go chain")
	}
	const marker = "synthetic-private-error-marker"
	var reads atomic.Int32
	transport := probeTransport(func(*http.Request) (*http.Response, error) {
		if reads.Add(1) == 2 {
			panic(marker)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/csv"}},
			Body: io.NopCloser(strings.NewReader(syntheticIANACSV))}, nil
	})
	configureSyntheticIANA(t, syntheticIANAMount(t, transport))
	t.Setenv("OVDB_IANA_ENABLED", "true")
	var goSlog, httpErrors bytes.Buffer
	previousDiagnostic := selectedIANADiagnostic
	selectedIANADiagnostic = &goSlog
	t.Cleanup(func() { selectedIANADiagnostic = previousDiagnostic })
	_, handler, closeHandler, err := configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeHandler() })
	if reads.Load() != 0 {
		t.Fatal("configuredHandler read the synthetic provider at startup")
	}
	listener := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__test/panic" {
			panic(marker)
		}
		handler.ServeHTTP(w, r)
	}))
	listener.Config.ErrorLog = safeHTTPErrorLog(&httpErrors)
	listener.Start()
	t.Cleanup(listener.Close)

	// Positive controls establish that both Go sinks are observed. The panic
	// enters the same net/http server and its configured ErrorLog.
	response, err := listener.Client().Get(listener.URL + "/__test/panic")
	if err == nil {
		_ = response.Body.Close()
	}
	if got := httpErrors.String(); got != "ovdb_http_internal_error\n" {
		t.Fatalf("net/http ErrorLog control: %q", got)
	}
	slog.New(&ianaMarkerHandler{out: &goSlog}).Error(marker)
	if got := goSlog.String(); got != "iana_candidate_internal_error\n" {
		t.Fatalf("Go slog control: %q", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node_modules/.bin/vitest", "run", "test/iana-chain.test.ts")
	command.Dir = ".."
	command.Env = append(os.Environ(), "IANA_CHAIN_BRIDGE_URL="+listener.URL)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Worker-to-Go chain failed: %v\n%s", err, output)
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("expected one successful and one failing synthetic provider read, got %d\n%s", got, output)
	}
	if got := goSlog.String(); got != "iana_candidate_internal_error\n" {
		t.Fatalf("selected Go handler changed the captured slog sink: %q", got)
	}
	if strings.Count(httpErrors.String(), "ovdb_http_internal_error\n") != 2 {
		t.Fatalf("selected provider panic did not pass through the fixed net/http ErrorLog: %q", httpErrors.String())
	}
	for sink, content := range map[string]string{"Go slog": goSlog.String(), "net/http ErrorLog": httpErrors.String()} {
		if strings.Contains(content, marker) {
			t.Fatalf("%s retained the synthetic marker", sink)
		}
	}
}
