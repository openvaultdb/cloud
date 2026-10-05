package main

import (
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// Limits that fit the instance this service is deployed on.
//
// The instance (deploy-chinook-cloudrun.yml): 512 MiB of memory, 1 vCPU, at most
// 5 requests at once (--concurrency=5), at most 2 instances, 120 s request timeout.
// On Cloud Run the file system is memory, so files the server spools count too.
//
// The worst case of one instance is every class of request at its largest at the
// same moment. The figures are measured peaks of resident Go memory (including
// garbage the collector has not reclaimed yet), taken in process against the
// providers pinned in providers.json and openvaultdb-go v0.13.0; they are repeated
// as constants in limits_test.go, which fails when the arithmetic stops holding.
//
//	server at rest                     80 MiB  14 Go-managed + 52.4 binary (all resident) + 13 slack
//	snapshot spool       2 slots x 64 MiB = 128 MiB
//	in-memory query      1 slot  x 90 MiB =  90 MiB  (measured 79.5 grouping, 34 join; rounded up)
//	database-route query 1 slot  x 64 MiB =  64 MiB  (42.5 for a 5.6 MB answer, scaled to the 8 MiB bound)
//	other requests       3       x 24 MiB =  72 MiB  (5 less the 2 gated slots; 21.2 for the largest answer)
//	total                                   434 MiB of 512: 78 MiB (15.2%) stay free
//
// The library bounds a join to 10,000 rows and 16 MiB and a grouping to 100,000
// groups and 64 MiB, but it counts the JSON size of what it holds. The heap holds
// 1.2 to 4.1 times that, so the sums above use measured memory, not the counted
// bytes. A grouping that could read 100,000 rows held 147 MiB; the row budget below
// keeps it at 79.5 MiB or less.
//
// Chosen against the library defaults (2 in-memory slots, 4 database slots, a
// 1 GiB spool, a platform concurrency of 80), which add up to far more than the
// instance:
//
//   - InMemory 1: the library's own advice for a 512 MiB instance. A second slot
//     adds 90 MiB.
//   - Database 1: a database-route answer can reach the 8 MiB result bound, 64 MiB
//     of heap. A request that finds the slot taken waits one second and is then
//     refused with 503 query_capacity.
//   - MaxSourceRows 40,000: a document that reads more rows than that in memory is
//     refused with 422 query_budget_exceeded (source_rows). Three pinned tables of
//     67,131 to 89,253 rows (Production.WorkOrder, Production.WorkOrderRouting,
//     Production.TransactionHistoryArchive) can no longer be read in memory. A
//     document of one database without a subquery or a null test runs in the
//     database and reads no rows into memory.
//   - MaxSourceBytes, Timeout and QueueWait are the library defaults, written out so
//     that the discovery document and this comment state what is enforced.
//   - JoinEngines sqlite: the only engine this service mounts.
//   - Snapshot slots 2, bytes 64 MiB: the largest pinned collection spools 57.8 MiB.
//     A larger one is refused with 413 snapshot_too_large.
//
// Adding or re-pinning a provider changes the measurements: re-measure, then update
// this comment and limits_test.go.
func cloudQueryLimits() server.QueryLimits {
	return server.QueryLimits{
		Timeout:        10 * time.Second,
		InMemory:       1,
		Database:       1,
		QueueWait:      time.Second,
		MaxSourceRows:  40_000,
		MaxSourceBytes: 64 << 20,
		JoinEngines:    []string{"sqlite"},
	}
}

// cloudSnapshotLimits bounds the spool behind paged queries; see cloudQueryLimits.
func cloudSnapshotLimits() server.SnapshotLimits {
	return server.SnapshotLimits{Slots: 2, Bytes: 64 << 20, Rows: 1_000_000}
}

func cloudServerOptions() []server.Option {
	return []server.Option{
		server.WithQueryLimits(cloudQueryLimits()),
		server.WithSnapshotLimits(cloudSnapshotLimits()),
	}
}
