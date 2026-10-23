package main

import (
	"log"
	"net/http"
	"sync"
	"time"
)

// startupProbePath is the Cloud Run startup probe route. It answers 200 once the
// native PostgreSQL DemoDB editions have settled, so a fresh instance takes no
// traffic while it would still answer 503 "pending". The path does not end in
// "z" because Cloud Run reserves such paths.
const startupProbePath = "/healthz/startup"

// startupProbeBudget caps how long the probe reports "starting", counted from
// process start. One edition's first attempt can take the 20 s self-check plus
// the 90 s mount attempt; six editions two at a time is three waves, 330 s,
// which exceeds Cloud Run's 240 s startup-probe window (deploy_gateway.py:
// periodSeconds=10 x failureThreshold=24). A probe that never passes would get
// the instance killed and restarted in a loop, so after this budget the instance
// reports ready and any unsettled edition keeps answering 503 with Retry-After,
// as it did before the probe existed. 200 s plus one 10 s probe period leaves
// 30 s of margin inside the window.
const startupProbeBudget = 200 * time.Second

// processStartedAt approximates container start for the probe budget.
var processStartedAt = time.Now()

// startupProbe reports whether the instance may take traffic.
type startupProbe struct {
	settled  <-chan struct{} // closed once every edition finished its first attempt; nil means nothing to wait for
	deadline time.Time
	now      func() time.Time
	expired  sync.Once
}

func newStartupProbe(settled <-chan struct{}) *startupProbe {
	return &startupProbe{settled: settled, deadline: processStartedAt.Add(startupProbeBudget), now: time.Now}
}

// ready is true once the mounts have settled (each edition is available, or its
// first attempt failed or was refused) or the startup budget is spent.
func (p *startupProbe) ready() bool {
	if p.settled == nil {
		return true
	}
	select {
	case <-p.settled:
		return true
	default:
	}
	if p.now().Before(p.deadline) {
		return false
	}
	p.expired.Do(func() {
		log.Printf("startup probe: native PostgreSQL mounts did not settle within %s; reporting ready, unsettled editions answer 503 until mounted", startupProbeBudget)
	})
	return true
}

// wrap serves startupProbePath and passes every other request to next.
func (p *startupProbe) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != startupProbePath {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if !p.ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"starting"}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}` + "\n"))
	})
}

// withStartupProbeRoute admits the platform startup probe, which cannot carry
// the edge credential, past the public boundary. Only an exact GET or HEAD of
// startupProbePath without a query is admitted; it reaches inner with no
// headers and no body, and the route reveals only "starting" or "ready".
// Everything else goes through public.
func withStartupProbeRoute(public, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != startupProbePath || r.URL.RawQuery != "" || r.URL.ForceQuery || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			public.ServeHTTP(w, r)
			return
		}
		probe := r.Clone(r.Context())
		probe.Header = http.Header{}
		probe.Body = http.NoBody
		probe.ContentLength = 0
		inner.ServeHTTP(w, probe)
	})
}
