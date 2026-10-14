package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// demoPostgresStatusPath reports each optional native PostgreSQL mount. The
// path does not end in "z" because Cloud Run reserves such paths.
const demoPostgresStatusPath = "/v1/demodb/postgres-mounts"

// Mount states served by demoPostgresStatusPath.
const (
	demoPostgresPending       = "pending"        // first attempt still running
	demoPostgresAvailable     = "available"      // mounted and served
	demoPostgresUnavailable   = "unavailable"    // last attempt failed; retried later
	demoPostgresMisconfigured = "misconfigured"  // refused by configuration; needs a restart with a fixed secret
	demoPostgresNotConfigured = "not_configured" // DSN secret not bound
)

// demoPostgresMountTiming bounds one attempt and spaces retries. The library's own
// catalogue discovery deadline is 30 s; the self-check and connection open add to it.
type demoPostgresMountTiming struct {
	SelfCheck   time.Duration
	Attempt     time.Duration
	FirstRetry  time.Duration
	MaxRetry    time.Duration
	Concurrency int
}

func defaultDemoPostgresMountTiming() demoPostgresMountTiming {
	return demoPostgresMountTiming{
		SelfCheck:   20 * time.Second,
		Attempt:     90 * time.Second,
		FirstRetry:  30 * time.Second,
		MaxRetry:    15 * time.Minute,
		Concurrency: 2,
	}
}

// demoPostgresMountTarget is the part of server.Server the mounter needs.
type demoPostgresMountTarget interface {
	Mount(*core.Database) error
	Unmount(string) error
}

type demoPostgresMountStatus struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	Reason    string     `json:"reason,omitempty"`
	Attempts  int        `json:"attempts"`
	NextRetry *time.Time `json:"nextRetry,omitempty"`
}

// demoPostgresMounter mounts each native PostgreSQL DemoDB source in the
// background, independently of the others and of the SQLite inventory. A failing
// or slow source is logged (never with its DSN), reported as unavailable and
// retried with backoff without a restart.
type demoPostgresMounter struct {
	target  demoPostgresMountTarget
	timing  demoPostgresMountTiming
	mount   func(runtimeDatabase) (*core.Database, error)
	check   func(context.Context, string, string) error
	scope   func(*core.Database, runtimeDatabase) error
	getenv  func(string) string
	ctx     context.Context
	cancel  context.CancelFunc
	slots   chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	status  map[string]*demoPostgresMountStatus
	origins map[string]map[string]bool
	mounted map[string]bool
	settled chan struct{}
	pending int
}

func newDemoPostgresMounter(target demoPostgresMountTarget, providers []runtimeDatabase, timing demoPostgresMountTiming) *demoPostgresMounter {
	ctx, cancel := context.WithCancel(context.Background())
	if timing.Concurrency < 1 {
		timing.Concurrency = 1
	}
	m := &demoPostgresMounter{
		target:  target,
		timing:  timing,
		mount:   mountDemoPostgres,
		check:   checkDemoPostgresSource,
		scope:   checkDemoPostgresMountedScope,
		getenv:  os.Getenv,
		ctx:     ctx,
		cancel:  cancel,
		slots:   make(chan struct{}, timing.Concurrency),
		status:  make(map[string]*demoPostgresMountStatus, len(providers)),
		origins: make(map[string]map[string]bool, len(providers)),
		mounted: make(map[string]bool, len(providers)),
		settled: make(chan struct{}),
		pending: len(providers),
	}
	for _, provider := range providers {
		m.status[provider.ID] = &demoPostgresMountStatus{ID: provider.ID, State: demoPostgresPending}
		m.origins[provider.ID] = make(map[string]bool, len(provider.CORSOrigins))
		for _, origin := range provider.CORSOrigins {
			m.origins[provider.ID][origin] = true
		}
	}
	if m.pending == 0 {
		close(m.settled)
	}
	return m
}

func (m *demoPostgresMounter) start(providers []runtimeDatabase) {
	for _, provider := range providers {
		m.wg.Add(1)
		go m.run(provider)
	}
}

// run drives one source until it is mounted, refused for good, or stopped.
func (m *demoPostgresMounter) run(provider runtimeDatabase) {
	defer m.wg.Done()
	delay := m.timing.FirstRetry
	for attempt := 1; ; attempt++ {
		state, err := m.attempt(provider)
		var next *time.Time
		if state == demoPostgresUnavailable {
			at := time.Now().Add(delay)
			next = &at
		}
		m.record(provider.ID, state, err, attempt, next, m.getenv(provider.nativePostgres.dsnEnv))
		if state != demoPostgresUnavailable {
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-m.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay *= 2
		if delay > m.timing.MaxRetry {
			delay = m.timing.MaxRetry
		}
	}
}

// Errors the mounter itself produces; each has a stable public reason code.
var (
	errDemoPostgresNotConfigured = errors.New("DSN secret is not set")
	errDemoPostgresTimeout       = errors.New("mount did not finish in time")
	errDemoPostgresStopping      = errors.New("server is stopping")
	errDemoPostgresMountFailed   = errors.New("mount failed")
	errDemoPostgresMountRejected = errors.New("mounted database was rejected")
)

// demoPostgresLibraryRoleRefusalText is the start of the error openvaultdb-go
// v0.25.2 (pkg/mount refuseWritableRole) returns when RequireReadOnlyNativePostgresRole
// refuses a role. The library exports no sentinel or error type for it (the
// error is a plain fmt.Errorf), so it is matched by this narrow fixed prefix;
// TestDemoPostgresLibraryRoleRefusalTextMatchesLibrary fails if the library
// changes the text.
const demoPostgresLibraryRoleRefusalText = "native read-only PostgreSQL mount refused: the role is not read-only"

// demoPostgresLibraryRoleRefusal reports whether err carries the library's role refusal.
func demoPostgresLibraryRoleRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), demoPostgresLibraryRoleRefusalText)
}

// demoPostgresReasonCode maps a failure to the stable code the public status
// route serves. Detailed text goes only to the (redacted) server log.
func demoPostgresReasonCode(err error) string {
	for _, c := range []struct {
		err  error
		code string
	}{
		{errDemoPostgresNotConfigured, "not_configured"},
		{errDemoPostgresTLS, "tls_required"},
		{errDemoPostgresInsecureHost, "tls_required"},
		{errDemoPostgresInsecureEnv, "tls_required"},
		{errDemoPostgresPoolerEndpoint, "pooler_endpoint"},
		{errDemoPostgresDSNInvalid, "dsn_rejected"},
		{errDemoPostgresDSNAmbiguous, "dsn_rejected"},
		{errDemoPostgresEnv, "dsn_rejected"},
		{errDemoPostgresOptionsParam, "dsn_rejected"},
		{errDemoPostgresRuntimeParam, "dsn_rejected"},
		{errDemoPostgresConnectTimeout, "dsn_rejected"},
		{errDemoPostgresOwnerRole, "role_unsafe"},
		{errDemoPostgresPrivilegedGrant, "role_unsafe"},
		{errDemoPostgresRoleSwitched, "role_unsafe"},
		{errDemoPostgresWritableRole, "role_unsafe"},
		{errDemoPostgresSchemaScope, "schema_scope"},
		{errDemoPostgresUnreachable, "connection_failed"},
		{errDemoPostgresTimeout, "timeout"},
		{errDemoPostgresStopping, "stopping"},
		{errDemoPostgresMountRejected, "mount_rejected"},
	} {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	if demoPostgresLibraryRoleRefusal(err) {
		return "role_unsafe"
	}
	return "mount_failed"
}

func (m *demoPostgresMounter) attempt(provider runtimeDatabase) (string, error) {
	select {
	case m.slots <- struct{}{}:
	case <-m.ctx.Done():
		return demoPostgresUnavailable, errDemoPostgresStopping
	}
	// The slot is released when the attempt really ends. An abandoned mount keeps
	// it until the late result arrives and is closed, so a hung source can never
	// let more than Concurrency real mounts run at once.
	releaseSlot := true
	defer func() {
		if releaseSlot {
			<-m.slots
		}
	}()

	dsn := m.getenv(provider.nativePostgres.dsnEnv)
	if dsn == "" {
		return demoPostgresNotConfigured, fmt.Errorf("%w: %s", errDemoPostgresNotConfigured, provider.nativePostgres.dsnEnv)
	}
	if err := validateDemoPostgresDSN(dsn, demoPostgresTLSAllowedInsecurely()); err != nil {
		return demoPostgresMisconfigured, err
	}
	checkCtx, cancelCheck := context.WithTimeout(m.ctx, m.timing.SelfCheck)
	err := m.check(checkCtx, dsn, provider.nativePostgres.schemaName)
	cancelCheck()
	if err != nil {
		return demoPostgresUnavailable, err
	}

	done := make(chan demoPostgresMountResult, 1)
	go func() {
		db, err := m.mount(provider)
		done <- demoPostgresMountResult{db, err}
	}()
	var mounted demoPostgresMountResult
	timer := time.NewTimer(m.timing.Attempt)
	defer timer.Stop()
	select {
	case mounted = <-done:
	case <-timer.C:
		releaseSlot = false
		m.closeLate(done)
		return demoPostgresUnavailable, fmt.Errorf("%w (%s)", errDemoPostgresTimeout, m.timing.Attempt)
	case <-m.ctx.Done():
		releaseSlot = false
		m.closeLate(done)
		return demoPostgresUnavailable, errDemoPostgresStopping
	}
	if mounted.err != nil {
		return demoPostgresUnavailable, fmt.Errorf("%w: %w", errDemoPostgresMountFailed, mounted.err)
	}
	db := mounted.db
	if db.Manifest.Database.ID != provider.ID || !runtimeDatabaseIDPattern.MatchString(provider.ID) {
		_ = db.Close()
		return demoPostgresMisconfigured, fmt.Errorf("%w: database ID does not match its provider", errDemoPostgresMountRejected)
	}
	if err := m.scope(db, provider); err != nil {
		_ = db.Close()
		return demoPostgresUnavailable, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		_ = db.Close()
		return demoPostgresUnavailable, errDemoPostgresStopping
	}
	if err := m.target.Mount(db); err != nil {
		_ = db.Close()
		return demoPostgresMisconfigured, fmt.Errorf("%w: %w", errDemoPostgresMountRejected, err)
	}
	m.mounted[provider.ID] = true
	return demoPostgresAvailable, nil
}

type demoPostgresMountResult struct {
	db  *core.Database
	err error
}

// closeLate waits, tracked by stop(), for a mount whose attempt was abandoned,
// closes what it produced, and only then frees the concurrency slot. Every DSN
// sets connect_timeout and the library bounds catalogue discovery, so it ends.
func (m *demoPostgresMounter) closeLate(done <-chan demoPostgresMountResult) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if late := <-done; late.db != nil {
			_ = late.db.Close()
		}
		<-m.slots
	}()
}

// checkDemoPostgresMountedScope asserts that every discovered collection belongs to
// the dataset's own schema. The pre-mount self-check refuses a role that can read
// any other schema; this is the second, independent look at what is served.
func checkDemoPostgresMountedScope(db *core.Database, provider runtimeDatabase) error {
	if db.Manifest.Schemas == nil {
		return nil
	}
	for id := range db.Manifest.Schemas.Collections {
		schemaName, _, err := schema.ParseNativePostgresCollectionID(id)
		if err != nil || schemaName != provider.nativePostgres.schemaName {
			return demoPostgresScopeError(provider.ID, provider.nativePostgres.schemaName)
		}
	}
	return nil
}

// redactDemoPostgresText removes the DSN and every connection detail it names
// (password, host, port, database, user) from a message before it is logged.
// Library and driver errors (pgconn.ConnectError) can name host, port, user and
// database.
func redactDemoPostgresText(message, dsn string) string {
	if dsn == "" {
		return message
	}
	message = strings.ReplaceAll(message, dsn, "[dsn]")
	var secrets []string
	if config, err := pgx.ParseConfig(dsn); err == nil {
		secrets = append(secrets, config.Password, config.Host, config.User, config.Database)
		if config.Port != 0 {
			port := strconv.Itoa(int(config.Port))
			message = strings.ReplaceAll(message, ":"+port, ":[port]")
			message = strings.ReplaceAll(message, "port="+port, "port=[port]")
			message = strings.ReplaceAll(message, "port "+port, "port [port]")
		}
	}
	if params, hosts, err := demoPostgresDSNParams(dsn); err == nil {
		secrets = append(secrets, params["password"], params["user"], params["dbname"], params["hostaddr"])
		secrets = append(secrets, hosts...)
	}
	if i := strings.Index(dsn, "://"); i >= 0 {
		if at := strings.Index(dsn[i+3:], "@"); at >= 0 {
			user, password, _ := strings.Cut(dsn[i+3:i+3+at], ":")
			secrets = append(secrets, password, user)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if len(secret) >= 3 {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return message
}

func (m *demoPostgresMounter) record(id, state string, err error, attempts int, next *time.Time, dsn string) {
	detail := ""
	m.mu.Lock()
	status := m.status[id]
	wasPending := status.State == demoPostgresPending
	status.State, status.Attempts, status.NextRetry = state, attempts, next
	status.Reason = ""
	if err != nil {
		status.Reason = demoPostgresReasonCode(err)
		detail = redactDemoPostgresText(err.Error(), dsn)
	}
	if wasPending {
		m.pending--
		if m.pending == 0 {
			close(m.settled)
		}
	}
	m.mu.Unlock()
	switch state {
	case demoPostgresAvailable:
		log.Printf("native PostgreSQL %s: mounted (attempt %d)", id, attempts)
	case demoPostgresNotConfigured:
		log.Printf("native PostgreSQL %s: skipped, %s", id, detail)
	default:
		retry := "not retried until restart"
		if next != nil {
			retry = "retry at " + next.UTC().Format(time.RFC3339)
		}
		log.Printf("native PostgreSQL %s: %s (attempt %d): %s: %s; %s", id, state, attempts, status.Reason, detail, retry)
	}
}

// waitSettled returns once every source has finished its first attempt.
func (m *demoPostgresMounter) waitSettled(ctx context.Context) error {
	select {
	case <-m.settled:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *demoPostgresMounter) snapshot() []demoPostgresMountStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	statuses := make([]demoPostgresMountStatus, 0, len(m.status))
	for _, status := range m.status {
		copied := *status
		statuses = append(statuses, copied)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
	return statuses
}

func (m *demoPostgresMounter) state(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status, ok := m.status[id]
	if !ok {
		return "", false
	}
	return status.State, true
}

// stop cancels retries, waits for running attempts, and unmounts what was mounted.
func (m *demoPostgresMounter) stop() error {
	m.cancel()
	m.wg.Wait()
	m.mu.Lock()
	ids := make([]string, 0, len(m.mounted))
	for id := range m.mounted {
		ids = append(ids, id)
	}
	m.mounted = map[string]bool{}
	m.mu.Unlock()
	var stopErr error
	for _, id := range ids {
		if err := m.target.Unmount(id); err != nil && stopErr == nil {
			stopErr = err
		}
	}
	return stopErr
}

// wrap serves the status route and answers requests for a native PostgreSQL
// database that is not mounted with 503 and Retry-After instead of the library's
// 404, so a client can tell "temporarily unavailable" from "does not exist".
func (m *demoPostgresMounter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == demoPostgresStatusPath {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_ = json.NewEncoder(w).Encode(struct {
				Mounts []demoPostgresMountStatus `json:"mounts"`
			}{m.snapshot()})
			return
		}
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/databases/"); ok && r.Method != http.MethodOptions {
			id, _, _ = strings.Cut(id, "/")
			if state, known := m.state(id); known && state != demoPostgresAvailable {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.Header().Add("Vary", "Origin")
				if origin := r.Header.Get("Origin"); origin != "" && m.origins[id][origin] {
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
				if state == demoPostgresPending || state == demoPostgresUnavailable {
					w.Header().Set("Retry-After", strconv.Itoa(int(m.timing.FirstRetry.Seconds())))
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
					"code":    "database_unavailable",
					"message": "This PostgreSQL edition is not available right now (" + state + ").",
				}})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
