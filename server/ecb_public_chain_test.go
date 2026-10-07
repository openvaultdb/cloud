package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Owns only a loopback listener and authored XML transport, never ECB network.
func TestECBPublicCrossRuntimeChain(t *testing.T) {
	if os.Getenv("OVDB_ECB_PUBLIC_CROSS_RUNTIME") != "1" {
		t.Skip("set OVDB_ECB_PUBLIC_CROSS_RUNTIME=1 for synthetic Worker-to-Go chain")
	}
	reads := 0
	a, c, digest := configureSyntheticECBPublic(t, &reads)
	_, handler, closeH, err := configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeH() })
	if reads != 0 {
		t.Fatal("startup provider read")
	}
	badGate := newECBPublicGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"records":[{"key":"marker","data":{"rate":"synthetic-private-marker"}}]`))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(`,"complete":false}`))
	}), a, c, digest, syntheticPublicSecret)
	listener := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("X-Synthetic-Bad-Footer"), "true") {
			badGate.ServeHTTP(w, r)
		} else {
			handler.ServeHTTP(w, r)
		}
	}))
	listener.Config.ErrorLog = safeHTTPErrorLog(os.Stderr)
	listener.Start()
	t.Cleanup(listener.Close)
	encoded, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node_modules/.bin/vitest", "run", "test/ecb-public-chain.test.ts")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "ECB_CHAIN_BRIDGE_URL="+listener.URL, "ECB_PUBLIC_CHAIN_ADMISSION="+string(encoded), "ECB_PUBLIC_CHAIN_DIGEST="+digest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic chain: %v\n%s", err, out)
	}
	if reads != 2 {
		t.Fatalf("expected positive/zero exactly two reads; got%d\n%s", reads, out)
	}
}
