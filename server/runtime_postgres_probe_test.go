package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dtql"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
)

type nativePostgresCapacityWorkload struct {
	name          string
	query         string
	expectedRows  int
	route         string
	countField    string
	expectedCount int64
}

func nativePostgresCapacityWorkloads() []nativePostgresCapacityWorkload {
	return []nativePostgresCapacityWorkload{
		{name: "artist", query: nativePostgresTableQuery("Artist", 275), expectedRows: 275},
		{name: "album", query: nativePostgresTableQuery("Album", 347), expectedRows: 347},
		{name: "track", query: nativePostgresTableQuery("Track", joinexec.MaxResultRows), expectedRows: joinexec.MaxResultRows},
		{
			name:          "track-count",
			query:         nativePostgresCountQuery("Track"),
			expectedRows:  1,
			route:         "database",
			countField:    "row_count",
			expectedCount: 3503,
		},
	}
}

func nativePostgresTableQuery(table string, limit int) string {
	return fmt.Sprintf("from: {schema: %s, name: %s}\nlimit: %d\n", strconv.Quote("chinook"), strconv.Quote(table), limit)
}

func nativePostgresCountQuery(table string) string {
	return fmt.Sprintf("from: {schema: %s, name: %s}\ncolumns: [{aggregate: {function: count, args: [{star: true}]}, as: row_count}]\nlimit: 1\n", strconv.Quote("chinook"), strconv.Quote(table))
}

func actualExactJSONInteger(raw json.RawMessage) (int64, bool) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return 0, false
	}
	if value[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, false
		}
		value = text
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || strconv.FormatInt(parsed, 10) != value {
		return 0, false
	}
	return parsed, true
}

func actualRecordCount(record json.RawMessage, field string) (int64, bool) {
	var item struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(record, &item); err != nil || item.Data == nil {
		return 0, false
	}
	raw, exists := item.Data[field]
	if !exists {
		return 0, false
	}
	return actualExactJSONInteger(raw)
}

func TestNativePostgresCapacityQueriesStayWithinResultLimit(t *testing.T) {
	workloads := nativePostgresCapacityWorkloads()
	wantLimits := map[string]int{"artist": 275, "album": 347, "track": joinexec.MaxResultRows, "track-count": 1}
	if len(workloads) != len(wantLimits) {
		t.Fatalf("native PostgreSQL workload count = %d, want %d", len(workloads), len(wantLimits))
	}
	seen := make(map[string]bool, len(workloads))
	for _, workload := range workloads {
		t.Run(workload.name, func(t *testing.T) {
			if seen[workload.name] {
				t.Fatalf("duplicate workload %q", workload.name)
			}
			seen[workload.name] = true
			query, err := dtql.Deserialize([]byte(workload.query))
			if err != nil {
				t.Fatalf("generated query does not parse: %v", err)
			}
			if got, want := query.Limit(), wantLimits[workload.name]; got != want {
				t.Fatalf("generated query limit = %d, want %d", got, want)
			}
			if query.Limit() <= 0 || query.Limit() > joinexec.MaxResultRows {
				t.Fatalf("generated query requests %d rows, outside public maximum %d", query.Limit(), joinexec.MaxResultRows)
			}
			if workload.name == "track-count" && (workload.countField != "row_count" || workload.expectedCount != 3503 || workload.route != "database") {
				t.Fatalf("Track native count contract = route %q, field %q, count %d; want database/row_count=3503", workload.route, workload.countField, workload.expectedCount)
			}
			if workload.name != "track-count" && (workload.countField != "" || workload.route != "") {
				t.Fatalf("row workload %q unexpectedly declares a count field or route", workload.name)
			}
		})
	}
	for name := range wantLimits {
		if !seen[name] {
			t.Errorf("native PostgreSQL capacity workload %q is missing", name)
		}
	}
}

func TestActualExactJSONIntegerRejectsRoundedOrNoncanonicalCounts(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want int64
		ok   bool
	}{
		{name: "number", raw: `3503`, want: 3503, ok: true},
		{name: "numeric string", raw: `"3503"`, want: 3503, ok: true},
		{name: "fraction", raw: `3503.0`},
		{name: "exponent", raw: `3.503e3`},
		{name: "leading zero string", raw: `"03503"`},
		{name: "null", raw: `null`},
		{name: "overflow", raw: `9223372036854775808`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := actualExactJSONInteger(json.RawMessage(test.raw))
			if got != test.want || ok != test.ok {
				t.Fatalf("actualExactJSONInteger(%s) = %d, %t; want %d, %t", test.raw, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestActualRecordCountReadsExactNestedDataField(t *testing.T) {
	for _, test := range []struct {
		name   string
		record string
		want   int64
		ok     bool
	}{
		{name: "number", record: `{"id":"count","data":{"row_count":3503}}`, want: 3503, ok: true},
		{name: "numeric string", record: `{"id":"count","data":{"row_count":"3503"}}`, want: 3503, ok: true},
		{name: "field missing", record: `{"id":"count","data":{"other":3503}}`},
		{name: "wrong nesting", record: `{"row_count":3503}`},
		{name: "rounded float", record: `{"id":"count","data":{"row_count":3503.0}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := actualRecordCount(json.RawMessage(test.record), "row_count")
			if got != test.want || ok != test.ok {
				t.Fatalf("actualRecordCount = %d, %t; want %d, %t", got, ok, test.want, test.ok)
			}
		})
	}
}
