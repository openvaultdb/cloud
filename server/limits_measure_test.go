package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// residentBytes is the memory the Go runtime holds from the system and has not
// returned: heap in use, garbage the collector has not reclaimed, stacks and
// metadata. It is the figure the arithmetic of limits.go is made of.
func residentBytes() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.Sys - stats.HeapReleased
}

// measureGrowth runs work and returns the most the resident memory rose above
// its level before it, sampled every millisecond after a forced collection.
func measureGrowth(work func()) uint64 {
	runtime.GC()
	debug.FreeOSMemory()
	base := residentBytes()
	var (
		mutex sync.Mutex
		peak  = base
		stop  = make(chan struct{})
		done  = make(chan struct{})
	)
	sample := func() {
		mutex.Lock()
		peak = max(peak, residentBytes())
		mutex.Unlock()
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				sample()
				time.Sleep(time.Millisecond)
			}
		}
	}()
	work()
	close(stop)
	<-done
	sample()
	return peak - base
}

// TestCloudMeasureSingleCollectionReads is the measurement behind
// measuredUngatedBytes. It runs only when OVDB_MEASURE_MEMORY is set (and the
// fixtures are prepared), because the memory a process holds is not something a
// test of the limits should depend on:
//
//	OVDB_MEASURE_MEMORY=1 SAMPLE_DATABASES_INVENTORY=... go test ./... -run TestCloudMeasureSingleCollectionReads -v
//
// It reads every collection of every mounted database in the three ways a read
// reaches the server without passing a gate, one at a time, prints the heaviest
// and fails when one holds more than the figure the arithmetic uses. Run it a few
// times: the collector's timing moves the result by several MiB.
func TestCloudMeasureSingleCollectionReads(t *testing.T) {
	if os.Getenv("OVDB_MEASURE_MEMORY") == "" {
		t.Skip("set OVDB_MEASURE_MEMORY=1 to measure the memory of single-collection reads")
	}
	handler := cloudTestHandler(t)
	type read struct {
		form, database, collection string
		status, answer             int
		growth                     uint64
	}
	var reads []read
	for _, entry := range getJSON(t, handler, "/v1/databases")["databases"].([]any) {
		id := entry.(map[string]any)["id"].(string)
		for _, value := range getJSON(t, handler, "/v1/databases/"+id)["collections"].([]any) {
			collection := value.(string)
			dtql := func() *http.Request {
				body := fmt.Sprintf("from: {name: '%s'}\nlimit: 1000\n", strings.ReplaceAll(collection, "'", "''"))
				request := httptest.NewRequest(http.MethodPost, "/v1/databases/"+id+"/dtql", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/yaml")
				return request
			}
			for _, form := range []struct {
				name    string
				request *http.Request
			}{
				{"query, no limit", wireRequest(t, id, core.Query{Collection: collection})},
				{"query, 1000 rows", wireRequest(t, id, core.Query{Collection: collection, Limit: 1000})},
				{"dtql, 1000 rows", dtql()},
			} {
				response := httptest.NewRecorder()
				growth := measureGrowth(func() { handler.ServeHTTP(response, form.request) })
				reads = append(reads, read{form.name, id, collection, response.Code, response.Body.Len(), growth})
			}
		}
	}
	slices.SortFunc(reads, func(a, b read) int { return int(int64(b.growth) - int64(a.growth)) })
	for _, r := range reads[:min(len(reads), 12)] {
		t.Logf("%-16s %-14s %-48s status %d, answer %5.2f MiB, held %5.1f MiB", r.form, r.database, r.collection, r.status, float64(r.answer)/mebibyte, float64(r.growth)/mebibyte)
	}
	if got := reads[0].growth; got > measuredUngatedBytes {
		t.Errorf("a read held %.1f MiB, above the %d MiB the arithmetic of limits.go uses: re-measure, then update both", float64(got)/mebibyte, measuredUngatedBytes/mebibyte)
	}
	for _, r := range reads {
		if r.form != "query, no limit" && r.growth > 32*mebibyte {
			t.Errorf("%s of %s %s held %.1f MiB: a read with a row limit is expected to hold far less than one without", r.form, r.database, r.collection, float64(r.growth)/mebibyte)
		}
	}
}

// TestCloudMeasureExactMoneyAggregates measures the representative Decimal_TEXT
// requests as a single in-memory slot. It is opt-in because peaks depend on the
// Go collector and machine; run it alongside the single-collection measurements
// when the provider inventory or OpenVaultDB dependency changes.
func TestCloudMeasureExactMoneyAggregates(t *testing.T) {
	if os.Getenv("OVDB_MEASURE_MEMORY") == "" {
		t.Skip("set OVDB_MEASURE_MEMORY=1 to measure exact Money aggregate memory")
	}
	handler := cloudTestHandler(t)
	for _, test := range []struct {
		name, query string
	}{
		{name: "Product 504 rows", query: adventureWorksProductMoneyQuery},
		{name: "PurchaseOrderDetail 8845 rows", query: adventureWorksPurchaseMoneyQuery},
		{name: "SalesOrderHeader 31465-row grouping", query: adventureWorksHeaderMoneyGroupingQuery},
	} {
		t.Run(test.name, func(t *testing.T) {
			var code int
			var answer []byte
			growth := measureGrowth(func() {
				response := postAdventureWorksDTQL(t, handler, test.query)
				code = response.Code
				answer = response.Body.Bytes()
			})
			if code != http.StatusOK {
				t.Fatalf("exact Money request returned %d: %s", code, answer)
			}
			t.Logf("exact Money aggregate held %.1f MiB", float64(growth)/mebibyte)
			if growth > measuredInMemoryBytes {
				t.Errorf("exact Money request held %.1f MiB, above the %d MiB in-memory request allowance", float64(growth)/mebibyte, measuredInMemoryBytes/mebibyte)
			}
		})
	}
}

func wireRequest(t *testing.T, database string, query core.Query) *http.Request {
	t.Helper()
	body, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/databases/"+database+"/query", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	return request
}
