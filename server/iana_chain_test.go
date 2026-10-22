package main

import (
	"net/http"
	"strings"
	"testing"
)

// The retired Worker bridge is replaced by the owning gateway. Native provider
// admission and redaction tests remain in the provider-specific test suites.
func TestIANASelectedGatewayRefusesDisabledRoute(t *testing.T) {
	t.Setenv("OVDB_IANA_ENABLED", "true")
	calls := 0
	h := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; t.Error("disabled route reached provider") }), nil)
	for _, method := range []string{"GET", "POST", "OPTIONS"} {
		w := gatewayRequest(t, h, method, cloudHost, "/iana/v1/databases/iana-http-status/dtql", strings.NewReader("private-synthetic-marker"))
		if w.Code != 503 || strings.Contains(w.Body.String(), "private-synthetic-marker") {
			t.Fatalf("disabled route=%d %s", w.Code, w.Body.String())
		}
	}
	if calls != 0 {
		t.Fatal("provider executed")
	}
}
