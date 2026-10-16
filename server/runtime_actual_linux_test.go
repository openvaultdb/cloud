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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	dalrecord "github.com/dal-go/record"
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
	result := map[string]any{"process_namespace_pid": os.Getpid()}
	cgroupIdentity, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	result["process_cgroup_identity"] = strings.TrimSpace(string(cgroupIdentity))
	metrics, err := readCgroupMetrics("/sys/fs/cgroup")
	if err != nil {
		return nil, err
	}
	if err := validateCgroupCapacity(metrics); err != nil {
		return nil, err
	}
	for name, value := range metrics {
		result[name] = value
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

// actualSelectionStart: portable regression compiles this exact harness seam.
type actualWork struct {
	name, path, body string
	want             int
	expectedRows     int
	countField       string
	expectedCount    int64
	noSourceRights   bool
	accept           string
	errorCode, route string
	incompleteBudget string
}

type actualMeasuredWork struct {
	databaseID string
	rows       int64
	work       actualWork
}

type actualWorkSelector struct{ works []actualMeasuredWork }

func (s *actualWorkSelector) add(databaseID string, rows int64, work actualWork) {
	// Each selected table is a distinct workload even when endpoints are shared.
	s.works = append(s.works, actualMeasuredWork{databaseID: databaseID, rows: rows, work: work})
}

func (s *actualWorkSelector) largest() []actualWork {
	result := []actualWork{}
	positions := map[string]int{}
	maxima := map[string]int64{}
	for _, measured := range s.works {
		position, exists := positions[measured.databaseID]
		if !exists {
			positions[measured.databaseID] = len(result)
			maxima[measured.databaseID] = measured.rows
			result = append(result, measured.work)
		} else if measured.rows > maxima[measured.databaseID] {
			maxima[measured.databaseID] = measured.rows
			result[position] = measured.work
		}
	}
	return result
}

// actualSelectionEnd

func actualExecute(handler http.Handler, work actualWork) (map[string]any, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	headers := map[string]string{}
	if work.accept != "" {
		headers["Accept"] = work.accept
	}
	result := actualRequest(handler, http.MethodPost, work.path, work.body, headers, ctx)
	entry := map[string]any{"name": work.name, "status": result.Code, "response_bytes": result.Body.Len(), "seconds": time.Since(started).Seconds()}
	if result.Code != work.want {
		var failure struct {
			Error struct {
				Code   string `json:"code"`
				Budget struct {
					Name string `json:"name"`
				} `json:"budget"`
			} `json:"error"`
		}
		_ = json.Unmarshal(result.Body.Bytes(), &failure)
		entry["error_code"], entry["budget_name"] = failure.Error.Code, failure.Error.Budget.Name
		return entry, fmt.Errorf("%s status %d want %d code=%q budget=%q", work.name, result.Code, work.want, failure.Error.Code, failure.Error.Budget.Name)
	}
	var document struct {
		Records  []json.RawMessage `json:"records"`
		Complete *bool             `json:"complete"`
		Error    struct {
			Code   string `json:"code"`
			Budget struct {
				Name string `json:"name"`
			} `json:"budget"`
		} `json:"error"`
		Execution struct {
			Route string `json:"route"`
		} `json:"execution"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &document); err != nil {
		return entry, fmt.Errorf("%s response envelope is invalid JSON", work.name)
	}
	if work.accept != "" {
		if result.Header().Get("Content-Type") != work.accept || !strings.Contains(strings.Join(result.Header().Values("Vary"), ", "), "Accept") {
			return entry, fmt.Errorf("%s response did not honor negotiated streaming headers", work.name)
		}
	}
	if work.incompleteBudget != "" {
		if result.Code != http.StatusOK || document.Complete == nil || *document.Complete || document.Error.Code != "query_budget_exceeded" || document.Error.Budget.Name != work.incompleteBudget || len(document.Records) == 0 {
			complete := document.Complete != nil && *document.Complete
			return entry, fmt.Errorf("%s incomplete stream status=%d complete=%t error=%q budget=%q rows=%d; want partial rows and a %s budget refusal", work.name, result.Code, complete, document.Error.Code, document.Error.Budget.Name, len(document.Records), work.incompleteBudget)
		}
		if result.Body.Len() > 9*mebibyte {
			return entry, fmt.Errorf("%s incomplete response is %d bytes, above the 9MiB bound", work.name, result.Body.Len())
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(result.Body.Bytes(), &payload); err != nil {
			return entry, err
		}
		for _, successOnly := range []string{"columns", "execution", "providerReads", "sourceRights", "usedSourceIds"} {
			if _, exists := payload[successOnly]; exists {
				return entry, fmt.Errorf("%s incomplete stream included success-only field %q", work.name, successOnly)
			}
		}
	} else if work.want == http.StatusOK && (document.Complete == nil || !*document.Complete) {
		return entry, fmt.Errorf("%s returned without a complete:true streaming footer", work.name)
	}
	if work.expectedRows > 0 && len(document.Records) != work.expectedRows {
		return entry, fmt.Errorf("%s returned %d rows, want %d", work.name, len(document.Records), work.expectedRows)
	}
	if work.countField != "" {
		if len(document.Records) != 1 {
			return entry, fmt.Errorf("%s returned %d count records, want one", work.name, len(document.Records))
		}
		count, ok := actualRecordCount(document.Records[0], work.countField)
		if !ok || count != work.expectedCount {
			return entry, fmt.Errorf("%s exact count %d is invalid or differs from expected %d", work.name, count, work.expectedCount)
		}
		entry["native_count_field"], entry["native_count"] = work.countField, count
	}
	if work.noSourceRights {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(result.Body.Bytes(), &payload); err != nil {
			return entry, err
		}
		if _, exists := payload["sourceRights"]; exists {
			return entry, fmt.Errorf("%s exposed undeclared source rights", work.name)
		}
		if _, exists := payload["usedSourceIds"]; exists {
			return entry, fmt.Errorf("%s exposed undeclared source identifiers", work.name)
		}
	}
	complete := document.Complete != nil && *document.Complete
	entry["rows"], entry["route"], entry["error_code"], entry["complete"] = len(document.Records), document.Execution.Route, document.Error.Code, complete
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

func TestCloudActualExecuteFailureDiagnosticIsSafe(t *testing.T) {
	const privateMarker = "provider-secret-marker-should-not-escape"
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"code":"invalid_dtql","budget":{"name":"query_rows"},"message":%q}}`, privateMarker)
	})
	entry, err := actualExecute(handler, actualWork{
		name: "safe-failure", path: "/v1/databases/chinook-postgresql/dtql",
		body: "from: {schema: chinook, name: Artist}\nlimit: 4000\n", want: http.StatusOK,
	})
	if err == nil {
		t.Fatal("actualExecute accepted a non-success response")
	}
	if strings.Contains(err.Error(), privateMarker) || strings.Contains(fmt.Sprint(entry), privateMarker) {
		t.Fatal("actualExecute exposed raw provider response content")
	}
	if entry["status"] != http.StatusBadRequest || entry["error_code"] != "invalid_dtql" || entry["budget_name"] != "query_rows" {
		t.Fatalf("safe failure receipt = %#v", entry)
	}
}

func TestCloudActualExecuteRequiresAnExplicitIncompleteBudgetFooter(t *testing.T) {
	valid := map[string]any{
		"records":  []any{map[string]any{"key": "partial"}},
		"error":    map[string]any{"code": "query_budget_exceeded", "budget": map[string]any{"name": "response_bytes"}},
		"complete": false,
	}
	for _, test := range []struct {
		name    string
		mutate  func(map[string]any)
		wantErr bool
	}{
		{name: "valid partial refusal"},
		{name: "missing complete footer", mutate: func(body map[string]any) { delete(body, "complete") }, wantErr: true},
		{name: "success footer on refusal", mutate: func(body map[string]any) { body["complete"] = true }, wantErr: true},
		{name: "wrong budget", mutate: func(body map[string]any) {
			body["error"].(map[string]any)["budget"].(map[string]any)["name"] = "source_rows"
		}, wantErr: true},
		{name: "no partial rows", mutate: func(body map[string]any) { body["records"] = []any{} }, wantErr: true},
		{name: "success-only metadata", mutate: func(body map[string]any) { body["execution"] = map[string]any{"route": "database"} }, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := map[string]any{}
			for key, value := range valid {
				body[key] = value
			}
			// Copy the nested structures which each mutation may change.
			body["records"] = []any{map[string]any{"key": "partial"}}
			body["error"] = map[string]any{"code": "query_budget_exceeded", "budget": map[string]any{"name": "response_bytes"}}
			if test.mutate != nil {
				test.mutate(body)
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Accept"); got != "application/vnd.openvaultdb.query-stream+json" {
					t.Errorf("Accept = %q", got)
				}
				w.Header().Set("Content-Type", "application/vnd.openvaultdb.query-stream+json")
				w.Header().Set("Vary", "Accept")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(encoded)
			})
			work := actualWork{
				name: "synthetic-late-budget", path: "/v1/databases/adventureworks/query",
				body: `{"collection":"Production.TransactionHistory"}`, want: http.StatusOK,
				accept: "application/vnd.openvaultdb.query-stream+json", incompleteBudget: "response_bytes",
			}
			entry, err := actualExecute(handler, work)
			if (err != nil) != test.wantErr {
				t.Fatalf("actualExecute error = %v, wantErr %t", err, test.wantErr)
			}
			if err == nil && (entry["complete"] != false || entry["rows"] != 1) {
				t.Fatalf("accepted partial stream receipt = %#v", entry)
			}
		})
	}
}

// actualQueriesStart: parser regression compiles the exact generated workload builders.
func actualOrdinaryQuery(table string, limit int) string {
	query := "from: {name: '" + strings.ReplaceAll(table, "'", "''") + "'}\n"
	if limit > 0 {
		query += fmt.Sprintf("limit: %d\n", limit)
	}
	return query
}
func actualSelectedQuery(table string) string {
	// bounded-immutable admits server-owned serving-key order only.
	return actualOrdinaryQuery(table, 1000)
}
func actualCallerOrderQuery(table, field string) string {
	return actualOrdinaryQuery(table, 0) + "orderBy: [{field: '" + strings.ReplaceAll(field, "'", "''") + "'}]\nlimit: 1000\n"
}
func actualCheckOrderedKeys(body []byte, expected []string) error {
	var page struct {
		Records []struct {
			Key string `json:"key"`
		} `json:"records"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return err
	}
	if len(page.Records) != len(expected) {
		return fmt.Errorf("serving-key page has %d rows, want %d", len(page.Records), len(expected))
	}
	for i, row := range page.Records {
		if row.Key != expected[i] {
			return fmt.Errorf("serving-key order at row %d: %q want %q", i, row.Key, expected[i])
		}
	}
	return nil
}

func actualDiagnosticQueries(table, selected string) []string {
	return []string{actualOrdinaryQuery(table, 1), "from: {name: '" + strings.ReplaceAll(selected, "'", "''") + "'}\nwhere: {op: In, left: {field: id}, right: {query: {from: {name: '" + strings.ReplaceAll(table, "'", "''") + "'}}}}\nlimit: 1\n"}
}
func actualLegacyWorks() []actualWork {
	grouped := "from:\n  database: adventureworks\n  name: Person.Person\n  alias: d\n  joins:\n    - type: left\n      from: {database: chinook, name: Genre, alias: g}\n      on:\n        - {left: {field: BusinessEntityID, source: d}, op: '==', right: {field: GenreId, source: g}}\ngroupBy: [{field: PersonType, source: d}]\ncolumns:\n  - {field: PersonType, source: d}\n  - {aggregate: {function: count, args: [{star: true}]}, as: n}\nlimit: 5\n"
	photo := "from:\n  name: Production.ProductProductPhoto\n  alias: p\n  joins:\n    - type: inner\n      from: {name: Production.ProductPhoto, alias: f}\n      on:\n        - {left: {field: ProductPhotoID, source: p}, op: '==', right: {field: ProductPhotoID, source: f}}\ncolumns:\n  - {field: ProductID, source: p}\n  - {field: LargePhoto, source: f}\n  - {field: ThumbNailPhoto, source: f}\nlimit: 1000\n"
	return []actualWork{
		{name: "heaviest-ungated", path: "/v1/databases/adventureworks/query", body: `{"collection":"Sales.SalesOrderHeaderSalesReason"}`, want: 200, accept: "application/vnd.openvaultdb.query-stream+json"},
		{name: "oversized-ungated", path: "/v1/databases/adventureworks/query", body: `{"collection":"Production.TransactionHistory"}`, want: 200, accept: "application/vnd.openvaultdb.query-stream+json", incompleteBudget: "response_bytes"},
		{name: "database-photo-join", path: "/v1/databases/adventureworks/dtql", body: photo, want: 200, route: "database"},
		{name: "database-ordinary", path: "/v1/databases/adventureworks/dtql", body: "from: {name: Person.Person}\nlimit: 1000\n", want: 200},
		{name: "in-memory-grouping", path: "/v1/dtql", body: grouped, want: 200, route: "in-memory"},
		{name: "money-grouping", path: "/v1/databases/adventureworks/dtql", body: adventureWorksHeaderMoneyGroupingQuery, want: 200},
		{name: "money-budget-refusal", path: "/v1/databases/adventureworks/dtql", body: adventureWorksSalesBudgetMoneyQuery, want: 422, errorCode: "query_budget_exceeded"},
	}
}

// actualQueriesEnd

func actualMatrix(t *testing.T, handler http.Handler, w1 []actualWork) []map[string]any {
	t.Helper()
	legacy := actualLegacyWorks()
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

// TestActualDemoPostgresCapacity runs inside a 512 MiB, one-CPU Linux shipping-
// image job whose six read-only DSNs are bound directly from Secret Manager.
// It intentionally emits only identifiers and measurements, never environment
// values or provider errors that could contain a connection string.
func TestActualDemoPostgresCapacity(t *testing.T) {
	if os.Getenv("OVDB_NATIVE_PG_CAPACITY") != "1" {
		t.Skip("requires the secret-bound Linux native PostgreSQL capacity job")
	}
	const shaName = "OVDB_NATIVE_PG_CAPACITY_SOURCE_SHA"
	sha := os.Getenv(shaName)
	if len(sha) != 40 {
		t.Fatal("native PostgreSQL capacity source SHA is missing or invalid")
	}
	for _, char := range sha {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			t.Fatal("native PostgreSQL capacity source SHA is missing or invalid")
		}
	}

	receipt := map[string]any{
		"source_sha":                 sha,
		"memory_limit_bytes":         uint64(512 << 20),
		"peak_limit_bytes":           actualPeakLimit,
		"configured_cpu_vcpu":        1,
		"cpu_quota_note":             "effective cgroup quota may be below the configured one vCPU due to platform overhead",
		"credential_values_recorded": false,
		"provider_errors_recorded":   false,
	}
	defer actualEmit(t, "NATIVE_POSTGRES_CAPACITY_JSON=", receipt)
	initial, err := actualCgroup()
	if err != nil {
		t.Fatal("native PostgreSQL capacity job lacks the required 512 MiB Linux cgroup")
	}
	receipt["initial_cgroup"] = initial
	monitor := actualStartMonitor()
	defer func() {
		peaks, monitorErr := monitor.finish()
		receipt["phase_memory_current_peaks"] = peaks
		if monitorErr != "" {
			t.Error("native PostgreSQL cgroup monitoring failed")
		}
		final, cgroupErr := actualCgroup()
		if cgroupErr != nil {
			t.Error("native PostgreSQL final cgroup metrics unavailable")
			return
		}
		receipt["final_cgroup"] = final
		if final["memory.peak"].(uint64) > actualPeakLimit {
			t.Error("native PostgreSQL workload exceeded the 435 MiB memory gate")
		}
		events := final["memory.events"].(map[string]uint64)
		if events["oom"] != 0 || events["oom_kill"] != 0 || events["max"] != 0 {
			t.Error("native PostgreSQL capacity job recorded cgroup memory pressure")
		}
	}()

	if os.Getenv(demoPostgresEnabledEnv) != "true" {
		t.Fatal("native PostgreSQL capacity job is not configured for all six sources")
	}
	started := time.Now()
	providers, handler, closeHandler, err := configuredHandler()
	if err != nil {
		t.Fatal("shipping image could not mount its configured read-only databases")
	}
	defer func() {
		if closeHandler() != nil {
			t.Error("native PostgreSQL capacity handler cleanup failed")
		}
	}()
	receipt["startup_seconds"] = time.Since(started).Seconds()
	if len(providers) != 12 {
		t.Fatalf("shipping image mounted %d providers, want six SQLite plus six PostgreSQL", len(providers))
	}
	receipt["provider_count"] = len(providers)
	var mountedIDs []string
	for _, provider := range providers {
		mountedIDs = append(mountedIDs, provider.ID)
	}
	sort.Strings(mountedIDs)
	var wantMountedIDs []string
	for _, source := range demoPostgresSources {
		wantMountedIDs = append(wantMountedIDs, source.databaseID, source.databaseID+"-postgresql")
	}
	sort.Strings(wantMountedIDs)
	if strings.Join(mountedIDs, "\n") != strings.Join(wantMountedIDs, "\n") {
		t.Fatal("shipping image did not mount the expected six SQLite and six PostgreSQL sources")
	}
	receipt["provider_ids"] = mountedIDs

	var beforeQueries runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&beforeQueries)
	var workloads []map[string]any
	expectedCatalogCounts := map[string]int{
		"chinook": 11, "northwind": 30, "pubs": 12,
		"sakila": 23, "adventureworks": 82, "employees": 8,
	}
	for _, source := range demoPostgresSources {
		id := source.databaseID + "-postgresql"
		descriptorResponse := actualRequest(handler, http.MethodGet, "/v1/databases/"+id, "", nil, context.Background())
		if descriptorResponse.Code != http.StatusOK {
			t.Fatalf("native PostgreSQL descriptor failed for %s", source.databaseID)
		}
		var descriptor struct {
			Capabilities map[string]bool `json:"capabilities"`
			Collections  []string        `json:"collections"`
			Schemas      struct {
				Collections map[string]struct {
					Source struct {
						Schema string `json:"schema"`
						Name   string `json:"name"`
					} `json:"source"`
					Fields map[string]struct {
						PrimaryKey bool `json:"primaryKey"`
					} `json:"fields"`
				} `json:"collections"`
			} `json:"schemas"`
		}
		if json.Unmarshal(descriptorResponse.Body.Bytes(), &descriptor) != nil ||
			descriptor.Capabilities["query"] || !descriptor.Capabilities["dtql"] || descriptor.Capabilities["write"] ||
			!descriptor.Capabilities["dtqlStreaming"] || !descriptor.Capabilities["dtqlStreamingErrors"] ||
			len(descriptor.Collections) == 0 || strings.Contains(descriptorResponse.Body.String(), "_import_manifest") {
			t.Fatalf("native PostgreSQL descriptor was incomplete or advertised unsupported behavior for %s", source.databaseID)
		}
		if want := expectedCatalogCounts[source.databaseID]; len(descriptor.Collections) != want {
			t.Fatalf("native PostgreSQL catalog for %s has %d collections, want %d", source.databaseID, len(descriptor.Collections), want)
		}
		receipt["catalog_count_"+source.databaseID] = len(descriptor.Collections)
		collectionIDs := append([]string(nil), descriptor.Collections...)
		sort.Strings(collectionIDs)
		var selectedCollection string
		var selectedField string
		var selectedKey string
		for _, collectionID := range collectionIDs {
			collection, ok := descriptor.Schemas.Collections[collectionID]
			if !ok || collection.Source.Schema != source.databaseID || collection.Source.Name == "_import_manifest" {
				continue
			}
			fields := make([]string, 0, len(collection.Fields))
			for field := range collection.Fields {
				fields = append(fields, field)
			}
			sort.Strings(fields)
			if len(fields) == 0 {
				continue
			}
			selectedCollection = collectionID
			for _, field := range fields {
				if collection.Fields[field].PrimaryKey {
					selectedKey = field
					selectedField = field
					break
				}
			}
			if selectedKey != "" {
				break
			}
		}
		if selectedCollection == "" || selectedField == "" || selectedKey == "" {
			t.Fatalf("native PostgreSQL catalog lacks a queryable keyed relation for %s", source.databaseID)
		}
		collection := descriptor.Schemas.Collections[selectedCollection]
		base := fmt.Sprintf("from: {schema: %s, name: %s}\n", strconv.Quote(collection.Source.Schema), strconv.Quote(collection.Source.Name))
		read := actualWork{
			name: source.databaseID + "-native-read",
			path: "/v1/databases/" + id + "/dtql",
			body: base + fmt.Sprintf("columns: [{field: %s}]\nlimit: 1\n", strconv.Quote(selectedField)),
			want: http.StatusOK, expectedRows: 1, noSourceRights: true,
		}
		entry, queryErr := actualExecute(handler, read)
		if queryErr != nil {
			t.Fatalf("bounded native PostgreSQL read failed for %s", source.databaseID)
		}
		workloads = append(workloads, entry)
		join := actualWork{
			name: source.databaseID + "-native-self-join",
			path: "/v1/databases/" + id + "/dtql",
			body: fmt.Sprintf("from:\n  schema: %s\n  name: %s\n  alias: l\n  joins:\n    - type: inner\n      from: {schema: %s, name: %s, alias: r}\n      on:\n        - {left: {field: %s, source: l}, op: '==', right: {field: %s, source: r}}\ncolumns: [{field: %s, source: l}]\nlimit: 1\n",
				strconv.Quote(collection.Source.Schema), strconv.Quote(collection.Source.Name),
				strconv.Quote(collection.Source.Schema), strconv.Quote(collection.Source.Name),
				strconv.Quote(selectedKey), strconv.Quote(selectedKey), strconv.Quote(selectedKey)),
			want: http.StatusOK, expectedRows: 1, noSourceRights: true,
		}
		entry, queryErr = actualExecute(handler, join)
		if queryErr != nil {
			t.Fatalf("bounded native PostgreSQL self-join failed for %s", source.databaseID)
		}
		workloads = append(workloads, entry)
	}
	for _, workload := range nativePostgresCapacityWorkloads() {
		work := actualWork{
			name:           "chinook-postgresql-" + workload.name,
			path:           "/v1/databases/chinook-postgresql/dtql",
			body:           workload.query,
			want:           http.StatusOK,
			expectedRows:   workload.expectedRows,
			countField:     workload.countField,
			expectedCount:  workload.expectedCount,
			noSourceRights: true,
			route:          workload.route,
		}
		entry, queryErr := actualExecute(handler, work)
		workloads = append(workloads, entry)
		receipt["native_workloads"] = workloads
		if queryErr != nil {
			receipt["failed_workload"] = map[string]any{
				"name": entry["name"], "status": entry["status"],
				"error_code": entry["error_code"], "budget_name": entry["budget_name"],
			}
			t.Fatalf("bounded PostgreSQL capacity workload failed: %v", queryErr)
		}
	}
	concurrentWorks := []actualWork{
		{
			name: "chinook-postgresql-concurrent-track-read", path: "/v1/databases/chinook-postgresql/dtql",
			body: "from: {schema: chinook, name: Track}\nlimit: 1000\n", want: http.StatusOK, expectedRows: 1000, noSourceRights: true,
		},
		{
			name: "chinook-sqlite-concurrent-track-read", path: "/v1/databases/chinook/dtql",
			body: "from: {name: Track}\nlimit: 1000\n", want: http.StatusOK, expectedRows: 1000,
		},
	}
	startConcurrent := make(chan struct{})
	type concurrentResult struct {
		entry map[string]any
		err   error
	}
	concurrentDone := make(chan concurrentResult, len(concurrentWorks))
	for _, work := range concurrentWorks {
		go func(work actualWork) {
			<-startConcurrent
			entry, queryErr := actualExecute(handler, work)
			concurrentDone <- concurrentResult{entry: entry, err: queryErr}
		}(work)
	}
	close(startConcurrent)
	for range concurrentWorks {
		result := <-concurrentDone
		if result.err != nil {
			t.Fatal("concurrent SQLite and PostgreSQL reads failed")
		}
		result.entry["concurrency"] = len(concurrentWorks)
		workloads = append(workloads, result.entry)
	}
	var afterQueries runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&afterQueries)
	heapGrowth := uint64(0)
	if afterQueries.HeapAlloc > beforeQueries.HeapAlloc {
		heapGrowth = afterQueries.HeapAlloc - beforeQueries.HeapAlloc
	}
	receipt["native_query_count"] = len(workloads)
	receipt["native_workloads"] = workloads
	receipt["query_heap_growth_bytes"] = heapGrowth
	if heapGrowth > 64<<20 {
		t.Fatal("native PostgreSQL query heap growth exceeded 64 MiB")
	}
	spoolCount, spoolBytes, spoolErr := actualSpools()
	if spoolErr != nil || spoolCount != 0 || spoolBytes != 0 {
		t.Fatal("native PostgreSQL bounded workload retained unexpected query snapshots")
	}
	receipt["temporary_query_spools"] = 0
}

// actualManifestReadStart: portable regression exercises this exact read with the real inventory loader.
func actualReadResolvedManifest(provider runtimeDatabase) ([]byte, error) {
	// loadRuntimeInventory already resolves Manifest and License against its base.
	return os.ReadFile(provider.Manifest)
}

// actualManifestReadEnd

func actualSelectedReads(t *testing.T, handler http.Handler, providers []runtimeDatabase) ([]actualWork, map[string]any) {
	t.Helper()
	selection := actualWorkSelector{}
	proof := map[string]any{}
	nativeCount, selectedCount := 0, 0
	var keys int64
	for _, provider := range providers {
		// Six legacy fixtures also get actual schema and ordinary-read checks.
		if provider.ReadProfile == "" {
			result := actualRequest(handler, http.MethodPost, "/v1/databases/"+provider.ID+"/dtql", actualOrdinaryQuery(provider.SmokeRecordset, 1), nil, context.Background())
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
		bytes, err = actualReadResolvedManifest(provider)
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
			work := actualWork{name: provider.ID + "/" + table, path: "/v1/databases/" + provider.ID + "/dtql", body: actualSelectedQuery(table), want: 200}
			response := actualRequest(handler, http.MethodPost, work.path, work.body, nil, context.Background())
			if response.Code != 200 {
				t.Fatalf("%s: %d %s", work.name, response.Code, response.Body.String())
			}
			ordered, err := raw.Query("SELECT " + quote(helper) + " FROM " + quote(table) + " ORDER BY " + quote(helper) + " ASC LIMIT 1000")
			if err != nil {
				t.Fatal(err)
			}
			expected := []string{}
			for ordered.Next() {
				var servingKey string
				if err := ordered.Scan(&servingKey); err != nil {
					t.Fatal(err)
				}
				expected = append(expected, dalrecord.NewKeyWithID(table, servingKey).String())
			}
			if err := ordered.Err(); err != nil {
				t.Fatal(err)
			}
			if err := ordered.Close(); err != nil {
				t.Fatal(err)
			}
			if err := actualCheckOrderedKeys(response.Body.Bytes(), expected); err != nil {
				t.Fatalf("%s: %v", work.name, err)
			}
			if table == publisher.Recordsets[0] {
				negative := actualWork{name: provider.ID + "/caller-order-refusal", path: work.path, body: actualCallerOrderQuery(table, orderField), want: 400, errorCode: "ordering_unsupported"}
				refusal, err := actualExecute(handler, negative)
				if err != nil {
					t.Fatal(err)
				}
				proof[provider.ID+"_caller_order_refusal"] = refusal
			}

			// Record every selected table before deriving the largest paired workload.
			tables = append(tables, map[string]any{"table": table, "rows": count, "last_key_read": true, "serving_key_order_verified": true})
			selection.add(provider.ID, count, work)
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
			for _, query := range actualDiagnosticQueries(table, publisher.Recordsets[0]) {
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
	proof["ordering_contract"] = "server-owned ascending serving key; caller orderBy rejected for both W1 providers; same tables, limit 1000 and largest-table pairing"
	proof["caller_order_refusal_cases"] = 2
	proof["key_method"] = "production checked mount validates every helper key; Python preparation separately scans all typed native values and all keys; HTTP final-key reads are additional samples"
	// Derive largest selected table per provider from all actual measured works.
	largest := selection.largest()
	if len(largest) != 2 {
		t.Fatal("missing two actual W1 workloads")
	}
	return largest, proof
}

func TestActualCapacity(t *testing.T) {
	if os.Getenv("OVDB_ACTUAL_CAPACITY") != "1" {
		t.Skip("requires full-corpus manual Linux experiment; host tests are not capacity proof")
	}
	receipt := map[string]any{
		"cold_cloud_capacity_accepted": false,
		"pool_metrics":                 "library exposes no pool counters; actual FD counts, cancellation and slot reuse recorded",
		"memory_gate_bytes":            actualPeakLimit,
		"configured_cpu_vcpu":          1,
		"cpu_quota_note":               "effective cgroup quota may be below the configured one vCPU due to platform overhead",
	}
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
	naturalMetrics, err := actualCgroup()
	if err != nil {
		t.Fatal(err)
	}
	receipt["natural_spools_memory"] = naturalMetrics
	receipt["load_natural_spools"] = actualMatrix(t, handler, w1)
	observedCount, observedBytes, err := actualSpools()
	if err != nil || observedCount != 2 || observedBytes != retained {
		t.Fatal("natural retained envelope expired or changed during workload")
	}
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
	info, err := os.Stat(ballastPath)
	if err != nil {
		t.Fatal(err)
	}
	allocation, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("ballast lacks actual Linux block allocation evidence")
	}
	allocatedBytes := allocation.Blocks * 512
	if written != ballast || info.Size() != ballast || allocatedBytes < ballast {
		t.Fatal("ballast was not fully written and allocated; sparse files are not envelope proof")
	}
	envelopeMetrics, err := actualCgroup()
	if err != nil {
		t.Fatal(err)
	}
	receipt["capacity_envelope"] = map[string]any{"actual_natural_spool_bytes": retained, "requested_ballast_bytes": ballast, "realized_ballast_bytes": info.Size(), "allocated_ballast_block_bytes": allocatedBytes, "separately_labelled_ballast_bytes": written, "total_tmpfs_bytes": retained + info.Size(), "ballast_is_snapshot_data": false, "memory": envelopeMetrics}
	receipt["load_ballast_envelope"] = actualMatrix(t, handler, w1)
	observedCount, observedBytes, err = actualSpools()
	if err != nil || observedCount != 2 || observedBytes != retained {
		t.Fatal("natural spools expired during ballast workload; full retained-envelope proof incomplete")
	}
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
	smallQuery := actualOrdinaryQuery("Order Details", 0)
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
