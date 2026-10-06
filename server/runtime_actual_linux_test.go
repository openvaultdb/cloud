//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/openvaultdb/cloud/server/internal/publisherselection"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
)

const actualPeakLimit = uint64(435 << 20)
const actualEnvelope = int64(128 << 20)
const actualSpoolQuery = "from: {name: 'Sales.SalesOrderDetail'}\n"

func actualEmit(t *testing.T, marker string, receipt map[string]any) {
	t.Helper()
	if t.Failed() {
		receipt["outcome"] = "failed"
	} else {
		receipt["outcome"] = "passed"
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Errorf("receipt: %v", err)
		return
	}
	fmt.Println(marker + string(data))
}

func actualCgroup() (map[string]any, error) {
	result := map[string]any{}
	for _, name := range []string{"memory.current", "memory.peak", "memory.max", "cpu.max", "memory.stat", "memory.events"} {
		data, err := os.ReadFile("/sys/fs/cgroup/" + name)
		if err != nil {
			return nil, fmt.Errorf("required cgroup v2 %s: %w", name, err)
		}
		switch name {
		case "memory.current", "memory.peak", "memory.max":
			n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if err != nil {
				return nil, err
			}
			result[name] = n
		case "cpu.max":
			result[name] = strings.TrimSpace(string(data))
		default:
			values := map[string]uint64{}
			fields := strings.Fields(string(data))
			if len(fields)%2 != 0 {
				return nil, fmt.Errorf("invalid cgroup %s", name)
			}
			for i := 0; i < len(fields); i += 2 {
				n, err := strconv.ParseUint(fields[i+1], 10, 64)
				if err != nil {
					return nil, err
				}
				values[fields[i]] = n
			}
			result[name] = values
		}
	}
	if result["memory.max"].(uint64) != 512<<20 {
		return nil, fmt.Errorf("memory limit is not 512MiB")
	}
	fields := strings.Fields(result["cpu.max"].(string))
	if len(fields) != 2 || fields[0] != fields[1] {
		return nil, fmt.Errorf("CPU quota is not exactly one CPU")
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	result["go_heap_alloc"], result["go_sys"] = mem.HeapAlloc, mem.Sys
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			result["process_rss"] = strings.TrimSpace(strings.TrimPrefix(line, "VmRSS:"))
		}
	}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, err
	}
	result["open_fds"] = len(fds)
	return result, nil
}

// A real executable listener is probed only in its private container namespace.
// The observer shares that cgroup; its overhead is conservative and explicit.
func TestActualProductionProbe(t *testing.T) {
	if os.Getenv("OVDB_ACTUAL_PROBE") != "1" {
		t.Skip("requires actual-runtime manual Linux container")
	}
	receipt := map[string]any{"probe_overhead": "static Go test observer shares production cgroup; no subtraction", "host_ports": false}
	defer actualEmit(t, "ACTUAL_PROBE_JSON=", receipt)
	started := time.Now()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	for time.Since(started) < 225*time.Second {
		response, err := client.Get("http://127.0.0.1:8080/v1/databases")
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("probe body: %v %v", readErr, closeErr)
			}
			if response.StatusCode != 200 {
				t.Fatalf("actual executable probe status %d", response.StatusCode)
			}
			var document struct {
				Databases []struct {
					ID string `json:"id"`
				} `json:"databases"`
			}
			if err := json.Unmarshal(body, &document); err != nil {
				t.Fatal(err)
			}
			if len(document.Databases) != 8 {
				t.Fatalf("actual executable mounted %d databases", len(document.Databases))
			}
			receipt["databases"] = document.Databases
			receipt["observer_ready_seconds"] = time.Since(started).Seconds()
			metrics, err := actualCgroup()
			if err != nil {
				t.Fatal(err)
			}
			receipt["memory"] = metrics
			if metrics["memory.peak"].(uint64) > actualPeakLimit {
				t.Fatal("production+observer cgroup peak exceeds 435MiB")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("actual production executable did not listen within bounded probe")
}

type actualMonitor struct {
	mu    sync.Mutex
	phase string
	peaks map[string]uint64
	error string
	stop  chan struct{}
	done  chan struct{}
}

func actualStartMonitor() *actualMonitor {
	m := &actualMonitor{phase: "startup", peaks: map[string]uint64{}, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			metrics, err := actualCgroup()
			m.mu.Lock()
			if err != nil {
				m.error = err.Error()
			} else {
				m.peaks[m.phase] = max(m.peaks[m.phase], metrics["memory.current"].(uint64))
			}
			m.mu.Unlock()
			select {
			case <-m.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return m
}
func (m *actualMonitor) setPhase(phase string) { m.mu.Lock(); defer m.mu.Unlock(); m.phase = phase }
func (m *actualMonitor) finish() (map[string]uint64, string) {
	close(m.stop)
	<-m.done
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peaks, m.error
}

func actualRequest(handler http.Handler, method, path, body string, headers map[string]string, ctx context.Context) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/yaml")
		if strings.HasPrefix(body, "{") {
			request.Header.Set("Content-Type", "application/json")
		}
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	return result
}
func actualSpools() (int, int64, error) {
	paths, err := filepath.Glob(filepath.Join(os.TempDir(), "ovdb-query-snapshots-*", "snapshot-*"))
	if err != nil {
		return 0, 0, err
	}
	var bytes int64
	for _, path := range paths {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, 0, err
		}
		bytes += info.Size()
	}
	return len(paths), bytes, nil
}

type actualSnapshot struct {
	Token   string `json:"snapshotToken"`
	Next    string `json:"nextPageToken"`
	Expires string `json:"snapshotExpiresAt"`
}

func actualCapture(t *testing.T, handler http.Handler, database, query string) actualSnapshot {
	t.Helper()
	started := time.Now()
	response := actualRequest(handler, http.MethodPost, "/v1/databases/"+database+"/dtql", query, map[string]string{"OVDB-Page-Size": "2"}, context.Background())
	var snapshot actualSnapshot
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || snapshot.Token == "" || snapshot.Next == "" {
		t.Fatalf("natural capture failed: %d %s", response.Code, response.Body.String())
	}
	expiry, err := time.Parse(time.RFC3339, snapshot.Expires)
	if err != nil || expiry.Sub(started) < 299*time.Second || time.Until(expiry) > 301*time.Second {
		t.Fatalf("actual five-minute expiry: %s %v", snapshot.Expires, err)
	}
	return snapshot
}
func actualClose(t *testing.T, handler http.Handler, database, query string, snapshot actualSnapshot) {
	t.Helper()
	response := actualRequest(handler, http.MethodPost, "/v1/databases/"+database+"/dtql", query,
		map[string]string{"OVDB-Page-Size": "2", "OVDB-Page-Token": snapshot.Token, "OVDB-Page-Close": "true"}, context.Background())
	if response.Code != 204 {
		t.Fatalf("snapshot close %d %s", response.Code, response.Body.String())
	}
}

func actualCancel(t *testing.T, handler http.Handler) map[string]any {
	t.Helper()
	before, _, err := actualSpools()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- actualRequest(handler, http.MethodPost, "/v1/databases/adventureworks/dtql", actualSpoolQuery,
			map[string]string{"OVDB-Page-Size": "2"}, ctx)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		count, bytes, err := actualSpools()
		if err != nil {
			t.Fatal(err)
		}
		if count > before && bytes > 0 {
			break
		}
		select {
		case result := <-done:
			t.Fatalf("capture finished before observed construction cancellation: %d %s", result.Code, result.Body.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no bounded observable natural snapshot construction")
		}
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	cancel()
	var result *httptest.ResponseRecorder
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("capture cancellation exceeded ten seconds")
	}
	if result.Code == 200 {
		t.Fatal("cancelled capture returned a successful snapshot")
	}
	count, bytes, err := actualSpools()
	if err != nil || count != before || bytes != 0 {
		t.Fatalf("cancel left partial spool: %d/%d %v", count, bytes, err)
	}
	return map[string]any{"seconds": time.Since(started).Seconds(), "response_status": result.Code, "partial_construction_observed": true, "remaining_spools": count}
}

type actualWork struct {
	name, path, body string
	want             int
	errorCode, route string
}

func actualExecute(handler http.Handler, work actualWork) (map[string]any, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result := actualRequest(handler, http.MethodPost, work.path, work.body, nil, ctx)
	entry := map[string]any{"name": work.name, "status": result.Code, "response_bytes": result.Body.Len(), "seconds": time.Since(started).Seconds()}
	if result.Code != work.want {
		return entry, fmt.Errorf("%s status %d want %d: %.1000s", work.name, result.Code, work.want, result.Body.String())
	}
	var document struct {
		Records []json.RawMessage `json:"records"`
		Error   struct {
			Code string `json:"code"`
		} `json:"error"`
		Execution struct {
			Route string `json:"route"`
		} `json:"execution"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &document); err != nil {
		return entry, err
	}
	entry["rows"], entry["route"], entry["error_code"] = len(document.Records), document.Execution.Route, document.Error.Code
	if work.want == 200 && len(document.Records) == 0 {
		return entry, fmt.Errorf("%s read no rows", work.name)
	}
	if work.errorCode != "" && document.Error.Code != work.errorCode {
		return entry, fmt.Errorf("%s wrong error %s", work.name, document.Error.Code)
	}
	if work.route != "" && document.Execution.Route != work.route {
		return entry, fmt.Errorf("%s wrong route %s", work.name, document.Execution.Route)
	}
	return entry, nil
}
func actualMatrix(t *testing.T, handler http.Handler, w1 []actualWork) []map[string]any {
	t.Helper()
	grouped := "from:\n  database: adventureworks\n  name: Person.Person\n  alias: d\n  joins:\n    - type: left\n      from: {database: chinook, name: Genre, alias: g}\n      on:\n        - {left: {field: BusinessEntityID, source: d}, op: '==', right: {field: GenreId, source: g}}\ngroupBy: [{field: PersonType, source: d}]\ncolumns:\n  - {field: PersonType, source: d}\n  - {aggregate: {function: count, args: [{star: true}]}, as: n}\nlimit: 5\n"
	photo := "from:\n  name: Production.ProductProductPhoto\n  alias: p\n  joins:\n    - type: inner\n      from: {name: Production.ProductPhoto, alias: f}\n      on:\n        - {left: {field: ProductPhotoID, source: p}, op: '==', right: {field: ProductPhotoID, source: f}}\ncolumns:\n  - {field: ProductID, source: p}\n  - {field: LargePhoto, source: f}\n  - {field: ThumbNailPhoto, source: f}\nlimit: 1000\n"
	legacy := []actualWork{
		{name: "heaviest-ungated", path: "/v1/databases/adventureworks/query", body: `{"collection":"Sales.SalesOrderHeaderSalesReason"}`, want: 200},
		{name: "database-photo-join", path: "/v1/databases/adventureworks/dtql", body: photo, want: 200, route: "database"},
		{name: "database-ordinary", path: "/v1/databases/adventureworks/dtql", body: "from: {name: Person.Person}\nlimit: 1000\n", want: 200},
		{name: "in-memory-grouping", path: "/v1/dtql", body: grouped, want: 200, route: "in-memory"},
		{name: "money-grouping", path: "/v1/databases/adventureworks/dtql", body: adventureWorksHeaderMoneyGroupingQuery, want: 200},
		{name: "money-budget-refusal", path: "/v1/databases/adventureworks/dtql", body: adventureWorksSalesBudgetMoneyQuery, want: 422, errorCode: "query_budget_exceeded"},
	}
	results := []map[string]any{}
	for _, work := range append(append([]actualWork{}, w1...), legacy...) {
		entry, err := actualExecute(handler, work)
		if err != nil {
			t.Fatal(err)
		}
		entry["concurrency"] = 1
		results = append(results, entry)
	}
	pairs := [][2]actualWork{}
	for i := range w1 {
		pairs = append(pairs, [2]actualWork{w1[i], w1[(i+1)%len(w1)]})
	}
	for _, selected := range w1 {
		for _, work := range legacy {
			pairs = append(pairs, [2]actualWork{selected, work})
		}
	}
	for _, pair := range pairs {
		start := make(chan struct{})
		type answer struct {
			entry map[string]any
			err   error
		}
		done := make(chan answer, 2)
		for _, work := range pair {
			go func(work actualWork) { <-start; entry, err := actualExecute(handler, work); done <- answer{entry, err} }(work)
		}
		close(start)
		for range 2 {
			result := <-done
			if result.err != nil {
				t.Fatal(result.err)
			}
			result.entry["concurrency"] = 2
			results = append(results, result.entry)
		}
	}
	return results
}

func actualSelectedReads(t *testing.T, handler http.Handler, providers []runtimeDatabase) ([]actualWork, map[string]any) {
	t.Helper()
	works := []actualWork{}
	proof := map[string]any{}
	nativeCount, selectedCount := 0, 0
	var keys int64
	for _, provider := range providers {
		// Six legacy fixtures also get actual schema and ordinary-read checks.
		if provider.ReadProfile == "" {
			result := actualRequest(handler, http.MethodPost, "/v1/databases/"+provider.ID+"/dtql", "from: {name: '"+provider.SmokeRecordset+"'}\nlimit: 1\n", nil, context.Background())
			if result.Code != 200 {
				t.Fatalf("legacy %s: %d %s", provider.ID, result.Code, result.Body.String())
			}
			continue
		}
		bytes, err := os.ReadFile(filepath.Join(protectedFixtureRoot, provider.ID+".publisher.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		publisher, err := publisherselection.Parse(bytes)
		if err != nil {
			t.Fatal(err)
		}
		bytes, err = os.ReadFile(filepath.Join(protectedFixtureRoot, provider.Manifest))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := manifest.Parse(bytes)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := sql.Open("sqlite", "file:"+filepath.Join(protectedFixtureRoot, provider.ID+".sqlite")+"?mode=ro&immutable=1")
		if err != nil {
			t.Fatal(err)
		}
		selected := map[string]bool{}
		tables := []map[string]any{}
		for _, table := range publisher.Recordsets {
			selected[table] = true
			selectedCount++
			helper := parsed.Storage.SQLite.RecordKeys[table]
			if helper == "" {
				t.Fatal("publisher selection has no helper")
			}
			quote := func(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
			var count int64
			if err := raw.QueryRow("SELECT count(*) FROM " + quote(table)).Scan(&count); err != nil {
				t.Fatal(err)
			}
			keys += count
			var key string
			if err := raw.QueryRow("SELECT " + quote(helper) + " FROM " + quote(table) + " ORDER BY " + quote(helper) + " DESC LIMIT 1").Scan(&key); err != nil {
				t.Fatal(err)
			}
			profile := actualRequest(handler, http.MethodGet, "/ovdb/dbs/"+provider.ID+"/collections/"+url.PathEscape(table), "", nil, context.Background())
			if profile.Code != 200 {
				t.Fatalf("selected schema %s: %d", table, profile.Code)
			}
			record := actualRequest(handler, http.MethodGet, "/v1/databases/"+provider.ID+"/records/"+url.PathEscape(table)+"/"+url.PathEscape(key), "", nil, context.Background())
			if record.Code != 200 {
				t.Fatalf("selected final key %s: %d %s", table, record.Code, record.Body.String())
			}
			var descriptor struct {
				Recordsets []struct {
					Name    string `json:"name"`
					Columns []struct {
						Name string `json:"name"`
					} `json:"columns"`
				} `json:"recordsets"`
			}
			descriptorBytes, err := os.ReadFile(filepath.Join(protectedFixtureRoot, provider.ID+".descriptor.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(descriptorBytes, &descriptor); err != nil {
				t.Fatal(err)
			}
			orderField := ""
			for _, recordset := range descriptor.Recordsets {
				if recordset.Name == table && len(recordset.Columns) > 0 {
					orderField = recordset.Columns[0].Name
				}
			}
			if orderField == "" {
				t.Fatal("selected native schema has no ordering field")
			}
			work := actualWork{name: provider.ID + "/" + table, path: "/v1/databases/" + provider.ID + "/dtql", body: "from: {name: '" + table + "'}\norderBy: ['" + strings.ReplaceAll(orderField, "'", "''") + "']\nlimit: 1000\n", want: 200}
			if _, err := actualExecute(handler, work); err != nil {
				t.Fatal(err)
			}
			// Record every selected table before deriving the largest paired workload.
			tables = append(tables, map[string]any{"table": table, "rows": count, "last_key_read": true})
			if len(works) == 0 || works[len(works)-1].path != work.path {
				works = append(works, work)
			}
		}
		rows, err := raw.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		nativeCount += len(names)
		for _, table := range names {
			if selected[table] {
				continue
			}
			for _, path := range []string{"/ovdb/dbs/" + provider.ID + "/collections/" + url.PathEscape(table), "/v1/databases/" + provider.ID + "/records/" + url.PathEscape(table) + "/any"} {
				if response := actualRequest(handler, http.MethodGet, path, "", nil, context.Background()); response.Code < 400 || response.Code >= 500 {
					t.Fatal("diagnostic metadata/key did not fail closed")
				}
			}
			for _, query := range []string{"from: {name: '" + table + "'}\nlimit: 1\n", "from: {name: '" + publisher.Recordsets[0] + "'}\nwhere: {op: in, left: {field: id}, right: {query: {from: {name: '" + table + "'}}}}\nlimit: 1\n"} {
				if response := actualRequest(handler, http.MethodPost, "/v1/databases/"+provider.ID+"/dtql", query, nil, context.Background()); response.Code < 400 || response.Code >= 500 {
					t.Fatal("diagnostic ordinary/nested query did not fail closed")
				}
			}
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		proof[provider.ID] = tables
	}
	if nativeCount != 16 || selectedCount != 11 || keys != 1280314 {
		t.Fatalf("incomplete actual W1 corpus %d/%d/%d", nativeCount, selectedCount, keys)
	}
	proof["native_tables"], proof["selected_tables"], proof["keys_validated_at_startup"] = nativeCount, selectedCount, keys
	proof["key_method"] = "production checked mount validates every helper key; Python preparation separately scans all typed native values and all keys; HTTP final-key reads are additional samples"
	// Derive largest selected table per provider from the measured counts above.
	largest := []actualWork{}
	for _, provider := range providers {
		if provider.ReadProfile == "" {
			continue
		}
		tables := proof[provider.ID].([]map[string]any)
		maxRows := int64(-1)
		name := ""
		for _, table := range tables {
			if table["rows"].(int64) > maxRows {
				maxRows = table["rows"].(int64)
				name = table["table"].(string)
			}
		}
		for _, work := range works {
			if work.name == provider.ID+"/"+name {
				largest = append(largest, work)
			}
		}
	}
	if len(largest) != 2 {
		t.Fatal("missing two actual W1 workloads")
	}
	return largest, proof
}

func TestActualCapacity(t *testing.T) {
	if os.Getenv("OVDB_ACTUAL_CAPACITY") != "1" {
		t.Skip("requires full-corpus manual Linux experiment; host tests are not capacity proof")
	}
	receipt := map[string]any{"cold_cloud_capacity_accepted": false, "pool_metrics": "library exposes no pool counters; actual FD counts, cancellation and slot reuse recorded", "memory_gate_bytes": actualPeakLimit}
	defer actualEmit(t, "ACTUAL_CAPACITY_JSON=", receipt)
	baseline, err := actualCgroup()
	if err != nil {
		t.Fatal(err)
	}
	receipt["initial"] = baseline
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(os.TempDir(), &filesystem); err != nil {
		t.Fatal(err)
	}
	if filesystem.Type != 0x01021994 {
		t.Fatal("writable experiment /tmp is not memory-backed tmpfs")
	}
	receipt["temporary_filesystem"] = "verified tmpfs; natural snapshots and labelled ballast charged to container memory"
	monitor := actualStartMonitor()
	defer func() {
		peaks, monitorError := monitor.finish()
		receipt["phase_sampled_current_peaks"] = peaks
		if monitorError != "" {
			t.Error(monitorError)
		}
		metrics, err := actualCgroup()
		if err != nil {
			t.Error(err)
			return
		}
		receipt["final"] = metrics
		if metrics["memory.peak"].(uint64) > actualPeakLimit {
			t.Error("whole-cgroup kernel memory peak exceeded435MiB")
		}
		events := metrics["memory.events"].(map[string]uint64)
		if events["oom"] != 0 || events["oom_kill"] != 0 || events["max"] != 0 {
			t.Error("cgroup memory limit/OOM events observed")
		}
	}()
	started := time.Now()
	providers, handler, closeHandler, err := configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closeHandler(); err != nil {
			t.Error(err)
		}
	}()
	receipt["checked_startup_seconds"] = time.Since(started).Seconds()
	if len(providers) != 8 || time.Since(started) >= 240*time.Second {
		t.Fatal("full eight-provider checked startup failed probe ceiling")
	}
	mounted, err := actualCgroup()
	if err != nil {
		t.Fatal(err)
	}
	receipt["mounted"] = mounted
	monitor.setPhase("ordinary-no-spools")
	w1, proof := actualSelectedReads(t, handler, providers)
	receipt["corpus"] = proof
	receipt["load_without_spools"] = actualMatrix(t, handler, w1)
	monitor.setPhase("construction-cancel")
	receipt["cancelled_capture"] = actualCancel(t, handler)
	monitor.setPhase("natural-spools")
	first := actualCapture(t, handler, "adventureworks", actualSpoolQuery)
	second := actualCapture(t, handler, "adventureworks", actualSpoolQuery)
	full := actualRequest(handler, http.MethodPost, "/v1/databases/adventureworks/dtql", actualSpoolQuery, map[string]string{"OVDB-Page-Size": "2"}, context.Background())
	if full.Code != 503 || !strings.Contains(full.Body.String(), "snapshot_capacity") {
		t.Fatalf("full natural slots: %d %s", full.Code, full.Body.String())
	}
	headers := map[string]string{"OVDB-Page-Size": "2", "OVDB-Page-Token": second.Next}
	retry1 := actualRequest(handler, http.MethodPost, "/v1/databases/adventureworks/dtql", actualSpoolQuery, headers, context.Background())
	retry2 := actualRequest(handler, http.MethodPost, "/v1/databases/adventureworks/dtql", actualSpoolQuery, headers, context.Background())
	if retry1.Code != 200 || retry2.Code != 200 || retry1.Body.String() != retry2.Body.String() {
		t.Fatal("natural continuation is not retryable")
	}
	actualClose(t, handler, "adventureworks", actualSpoolQuery, first)
	reused := actualCapture(t, handler, "adventureworks", actualSpoolQuery)
	count, retained, err := actualSpools()
	if err != nil || count != 2 || retained <= 0 || retained > actualEnvelope {
		t.Fatalf("natural occupancy %d/%d: %v", count, retained, err)
	}
	receipt["natural_spools"] = map[string]any{"files": count, "bytes": retained, "slot_close_reuse": true, "stable_page_retry": true, "snapshots": []actualSnapshot{second, reused}}
	receipt["load_natural_spools"] = actualMatrix(t, handler, w1)
	monitor.setPhase("ballast-envelope")
	ballast := actualEnvelope - retained
	ballastPath := filepath.Join(os.TempDir(), "ovdb-capacity-envelope-ballast")
	f, err := os.OpenFile(ballastPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Remove(ballastPath); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	}()
	block := make([]byte, 1<<20)
	var written int64
	for written < ballast {
		n, err := f.Write(block[:min(int64(len(block)), ballast-written)])
		if err != nil {
			t.Fatal(err)
		}
		written += int64(n)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	receipt["capacity_envelope"] = map[string]any{"actual_natural_spool_bytes": retained, "separately_labelled_ballast_bytes": written, "total_tmpfs_bytes": retained + written, "ballast_is_snapshot_data": false}
	receipt["load_ballast_envelope"] = actualMatrix(t, handler, w1)
	monitor.setPhase("idle-five-minute-expiry")
	expiry, err := time.Parse(time.RFC3339, reused.Expires)
	if err != nil {
		t.Fatal(err)
	}
	idle := time.Now()
	for time.Now().Before(expiry.Add(1500 * time.Millisecond)) {
		time.Sleep(time.Second)
	}
	count, bytes, err := actualSpools()
	if err != nil || count != 0 || bytes != 0 {
		t.Fatalf("real idle expiry left spools %d/%d: %v", count, bytes, err)
	}
	expired := actualRequest(handler, http.MethodPost, "/v1/databases/adventureworks/dtql", actualSpoolQuery, map[string]string{"OVDB-Page-Size": "2", "OVDB-Page-Token": reused.Next}, context.Background())
	if expired.Code != 410 || !strings.Contains(expired.Body.String(), "snapshot_expired") {
		t.Fatalf("expired token %d %s", expired.Code, expired.Body.String())
	}
	receipt["idle_expiry"] = map[string]any{"wait_seconds": time.Since(idle).Seconds(), "configured_lifetime_seconds": 300, "remaining_spools": count, "expired_status": expired.Code}
	if err := os.Remove(ballastPath); err != nil {
		t.Fatal(err)
	}
	monitor.setPhase("post-expiry-recovery")
	smallQuery := "from: {name: 'Order Details'}\n"
	small := actualCapture(t, handler, "northwind", smallQuery)
	actualClose(t, handler, "northwind", smallQuery, small)
	receipt["post_expiry_cancel"] = actualCancel(t, handler)
	receipt["post_expiry_reads"] = actualMatrix(t, handler, w1)
	count, bytes, err = actualSpools()
	if err != nil || count != 0 || bytes != 0 {
		t.Fatal("post-recovery spool leak")
	}
	recovered, err := actualCgroup()
	if err != nil {
		t.Fatal(err)
	}
	receipt["recovered"] = recovered
	if recovered["open_fds"].(int) > mounted["open_fds"].(int)+32 {
		t.Fatal("unbounded FD growth after lifecycle workload")
	}
	receipt["fd_limit_note"] = "allows up to32 persistent connections across full eight-provider exercised pools; no private pool counter API"
	receipt["seconds"] = time.Since(started).Seconds()
}
