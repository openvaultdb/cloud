# Public Chinook OVDB service

This Go process embeds the reusable `openvaultdb-go/pkg/server` HTTP handler in
an OpenVaultDB-owned Cloud Run service. It mounts a derived copy of the pinned
Chinook SQLite fixture in read-only mode. The image includes the upstream MIT
licence. No Sneat identity, account, or backend is involved in this public
database.

`prepare_fixture.py` verifies the source SHA-256, copies the eleven tables,
adds a stable `id` column based on each table's primary key for DALgo's SQLite
adapter, and generates the strict OVDB manifest with `database.cache_ttl: 24h`.
The canonical upstream source
and downloads on chinookdb.com are unchanged.

## Local acceptance

The cloud service and the updated Go server library live in sibling WB
worktrees while this coordinated change is local. Build a temporary workspace
file outside the repositories, then run:

```sh
python3 prepare_fixture.py fixture/Chinook_Sqlite.sqlite /tmp/chinook-ovdb
mkdir -p /tmp/ovdb-local-work
(cd /tmp/ovdb-local-work && go work init /absolute/path/to/openvaultdb/cloud/server /absolute/path/to/openvaultdb/openvaultdb-go)
GOWORK=/tmp/ovdb-local-work/go.work CHINOOK_MANIFEST=/tmp/chinook-ovdb/chinook.yaml go test . -run TestPublicChinookJourney -count=1
GOWORK=/tmp/ovdb-local-work/go.work CHINOOK_MANIFEST=/tmp/chinook-ovdb/chinook.yaml PORT=8080 go run .
```

Run these commands from `server/`, except the parenthesized workspace command.
The test covers generic pages, discovery, parameterized POST and GET DTQL,
the GET cache header, ChinookDB CORS, and rejected writes.

## Deployment dependency

The Dockerfile deliberately runs the acceptance test. Before building an image
for Cloud Run, publish the accompanying `openvaultdb-go` bound-DTQL, GET DTQL,
and per-database cache-duration change and
update `go.mod` to that release. The current `v0.9.0` pin does not include it;
an image built against it must fail the test instead of silently serving an
incompatible query endpoint.

Once deployed, configure `CHINOOK_RUN_ORIGIN` in the Cloudflare Worker to the
service's HTTPS origin. The Worker forwards public `/.well-known/openvaultdb`,
`/ovdb/*`, and `/v1/*` paths; the Go server uses
`https://cloud.openvaultdb.com` as its canonical public origin. The
ChinookDB site cutover should follow a live API and browser check.
