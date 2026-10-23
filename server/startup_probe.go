package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// startupProbePath is the Cloud Run startup probe route. It answers 200 once the
// native PostgreSQL DemoDB editions have settled, so a fresh instance takes no
// traffic while it would still answer 503 "pending". The path does not end in
// "z" because Cloud Run reserves such paths.
const startupProbePath = "/healthz/startup"

// startupProbeBudgetEnv overrides how long the probe may report "starting",
// counted from process start, as a Go duration between 0s and
// startupProbeMaxBudget (for example "45s"). It is read once at startup; a value
// that is not such a duration is a configuration error and the revision does not
// start, so the one serving keeps serving.
//
// Why the default is short: while the probe reports "starting", Cloud Run holds
// incoming requests for at most max(10 s, 3.5 x average startup time) and then
// answers 429 "no available instance" without CORS headers (Cloudflare gives up
// with 524 after 100 s). That would turn every route into an opaque error,
// including the SQLite editions and the device-auth proxy that are ready at
// once. One edition's first attempt can take the 20 s self-check plus the 90 s
// mount attempt, and six editions two at a time is three waves, 330 s, so waiting
// for the worst case is not an option. With 30 s, a fast start (healthy Neon
// sources settle in seconds) still waits for its mounts, and a slow start
// degrades to the behaviour before this probe: unsettled PostgreSQL editions
// answer 503 database_unavailable with Retry-After, which DataTug waits out.
const startupProbeBudgetEnv = "OVDB_STARTUP_PROBE_BUDGET"

const (
	startupProbeDefaultBudget = 30 * time.Second
	// startupProbeMaxBudget keeps the budget, plus Go start-up and one 5 s probe
	// period, inside the 60 s startup-probe window deploy_gateway.py configures
	// (periodSeconds=5 x failureThreshold=12). Raise both together.
	startupProbeMaxBudget = 50 * time.Second
)

var errStartupProbeBudget = fmt.Errorf("%s must be a duration between 0s and %s", startupProbeBudgetEnv, startupProbeMaxBudget)

// startupProbeBudgetFromEnv is startupProbeDefaultBudget unless
// startupProbeBudgetEnv sets a valid duration.
func startupProbeBudgetFromEnv(getenv func(string) string) (time.Duration, error) {
	value := strings.TrimSpace(getenv(startupProbeBudgetEnv))
	if value == "" {
		return startupProbeDefaultBudget, nil
	}
	budget, err := time.ParseDuration(value)
	if err != nil || budget < 0 || budget > startupProbeMaxBudget {
		return 0, errStartupProbeBudget
	}
	return budget, nil
}

// processStartedAt approximates container start for the probe budget.
var processStartedAt = time.Now()

// startupProbe reports whether the instance may take traffic.
type startupProbe struct {
	settled  <-chan struct{} // closed once every edition finished its first attempt; nil means nothing to wait for
	deadline time.Time
	now      func() time.Time
	budget   time.Duration
	expired  sync.Once
}

func newStartupProbe(settled <-chan struct{}, budget time.Duration) *startupProbe {
	return &startupProbe{settled: settled, deadline: processStartedAt.Add(budget), now: time.Now, budget: budget}
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
		log.Printf("startup probe: native PostgreSQL mounts did not settle within %s; reporting ready, unsettled editions answer 503 until mounted", p.budget)
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
