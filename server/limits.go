package main

import (
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// Limits that fit the instance this service is deployed on.
//
// The instance (deploy-chinook-cloudrun.yml): 512 MiB of memory, 1 vCPU, at most
// 2 requests at once (--concurrency=2), at most 2 instances, 120 s request timeout.
// On Cloud Run the file system is memory, so files the server spools count too.
//
// The worst case of one instance is the requests it can run at once, taken from the
// heaviest class down, each at its largest at the same moment. The figures are
// measured peaks of resident Go memory (including garbage the collector has not
// reclaimed yet). The original at-rest, database-route, and generic query
// baselines were measured with openvaultdb-go v0.13.0. Collection reads and the
// exact Money workloads were remeasured with v0.14.2 against the current provider
// pins. The limits are repeated as constants in limits_test.go, which checks that
// the arithmetic still fits the deployment.
//
//	server at rest                     80 MiB  14 Go-managed + 52.8 MiB binary + 13 slack
//	snapshot spool       2 slots x 64 MiB = 128 MiB
//	in-memory query      1 slot  x 90 MiB =  90 MiB  (79.5 generic grouping, 77.6 Money grouping, 34 join; rounded up)
//	read of a collection 1       x 72 MiB =  72 MiB  (concurrency 2 less the in-memory slot; see below)
//	total                                   370 MiB of 512: 142 MiB (27.7%) stay free
//
// A read of a collection by the query endpoint (/v1/databases/{id}/query) is the
// heaviest request that no gate counts: it applies no default row limit, so the
// library reads rows until its 8 MiB buffer is full, and the heap holds 6 to 10
// times the JSON it counts. A measurement over all 128 collections in the six
// pinned fixtures held at most 68.9 MiB in the v0.14.2 run (limits_test.go
// gives the measurement). It is heavier than a database-route query
// (64 MiB), so the second request of an instance is a read, not a join. Nothing in
// this service bounds the number of such reads but the concurrency of the instance:
// at 3 the worst case is 442 MiB, over the 435 MiB (85%) that
// TestCloudLimitsFitTheInstance allows. Reads with a row limit of 1,000, by that
// endpoint or by DTQL, held 22.4 MiB at most; if the library applied a row limit to
// the query endpoint, the same arithmetic would allow 5 requests at once.
//
// The library bounds a join to 10,000 rows and 16 MiB and a grouping to 100,000
// groups and 64 MiB, but it counts the JSON size of what it holds. The heap holds
// 1.2 to 4.1 times that, so the sums above use measured memory, not the counted
// bytes. One grouping that could read 100,000 rows held 147 MiB; a separate
// measured grouping stopped at the 40,000-row budget and held 79.5 MiB. The
// 31,465-row exact Money grouping held 77.6 MiB with v0.14.2.
//
// Chosen against the library defaults (2 in-memory slots, 4 database slots, a
// 1 GiB spool, a platform concurrency of 80), which add up to far more than the
// instance:
//
//   - InMemory 1: the library's own advice for a 512 MiB instance. A second slot
//     would put two joins in one instance at 180 MiB, not 162.
//   - Database 1: a database-route answer can reach the 8 MiB result bound, 64 MiB
//     of heap. A request that finds the slot taken waits one second and is then
//     refused with 503 query_capacity.
//   - MaxSourceRows 40,000: a document that reads more rows than that in memory is
//     refused with 422 query_budget_exceeded (source_rows). Three pinned tables of
//     67,131 to 89,253 rows (Production.WorkOrder, Production.WorkOrderRouting,
//     Production.TransactionHistoryArchive) can no longer be read in memory. A
//     document of one database without a subquery or a null test runs in the
//     database and reads no rows into memory. Discovery states the library's fixed
//     ceiling of 100,000 groups; this row budget keeps it out of reach, because a
//     grouping cannot hold more groups than the rows it reads.
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
