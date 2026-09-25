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

From `server/`, run:

```sh
python3 prepare_fixture.py fixture/Chinook_Sqlite.sqlite /tmp/chinook-ovdb
CHINOOK_MANIFEST=/tmp/chinook-ovdb/chinook.yaml go test . -run TestPublicChinookJourney -count=1
CHINOOK_MANIFEST=/tmp/chinook-ovdb/chinook.yaml PORT=8080 go run .
```

The test covers generic pages, discovery, parameterized POST and GET DTQL,
the GET cache header, ChinookDB CORS, and rejected writes.

## Deployment dependency

The shared Go CI workflow tests against the published `openvaultdb-go v0.10.0`
module and produces a checksummed Linux binary from the same commit. After CI
passes on `main`, GitHub Actions downloads that exact binary, builds the image
on its runner, pushes it to Artifact Registry, and deploys it to Cloud Run.
The Dockerfile packages the validated binary and generated fixture; no cloud
build service compiles the application.

Once deployed, configure `CHINOOK_RUN_ORIGIN` in the Cloudflare Worker to the
service's HTTPS origin. The Worker forwards public `/.well-known/openvaultdb`,
`/ovdb/*`, and `/v1/*` paths; the Go server uses
`https://cloud.openvaultdb.com` as its canonical public origin. The
ChinookDB site cutover should follow a live API and browser check.
