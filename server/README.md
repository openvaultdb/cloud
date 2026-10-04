# Public DemoDB OVDB service

This Go service embeds the upstream `openvaultdb-go/pkg/server` HTTP handler
and mounts the read-only Chinook and Northwind sample databases on the same
Cloud Run service. It keeps the current `cloud.openvaultdb.com` public origin
and accepts browser origins for `demodb.dev`, both sample subdomains, and the
legacy ChinookDB domains.

`prepare_fixture.py` verifies each provider artifact's pinned SHA-256, copies
it to a serving-only SQLite file, and adds the stable `id` column required by
the DALgo SQLite adapter. The script verifies that native columns, primary
keys, foreign keys, indexes, view definitions, and row counts survive that
adapter preparation. The provider files remain unchanged. Chinook uses the
legacy comma-separated key format to preserve existing record URLs; Northwind
uses natural single-column IDs and typed, collision-safe IDs for composite
keys. BLOB columns are declared as `any` so OVDB can return their native byte
values.

OpenVaultDB v0.11.0 currently emits SQLite identifiers directly while ensuring
strict schemas. The Northwind table `Order Details` therefore uses a quoted
SQL identifier in its generated manifest. After mounting, the service restores
the native logical collection name for discovery and rendering, then maps
GET, HEAD, and query-key reads to the retained quoted driver key. The public
URL, query, and returned row keep the upstream collection name. The SQLite
table, columns, composite primary key, self-reference, and foreign keys keep
their upstream names and definitions. A reusable fix belongs at the
`openvaultdb-go` strict-schema/mount boundary and the DALgo SQLite DDL builder,
where logical names can be separated from safely quoted SQL identifiers; this
service shim does not change those upstream libraries.

## Local acceptance

From `server/`, prepare both fixtures and run the real server journeys:

```sh
python3 prepare_fixture.py fixture/Chinook_Sqlite.sqlite /tmp/ovdb-fixtures chinook 7651ba378ac2fcd0dfc3c66fb101f7a7eed3ba39a612ec642b96e20702061f15 legacy
python3 prepare_fixture.py ../northwind/artifacts/northwind.sqlite /tmp/ovdb-fixtures northwind 279b34136771aee75d802094b3329515a2b01da65d2a20a4a9e3b58c29b4fd20 natural
CHINOOK_MANIFEST=/tmp/ovdb-fixtures/chinook.yaml NORTHWIND_MANIFEST=/tmp/ovdb-fixtures/northwind.yaml go test ./... -run 'TestPublic(Chinook|Northwind)Journey' -count=1
```

The integration journeys cover both database profiles and discovery, the
encoded `Order Details` collection route and DTQL query, Northwind's composite
key fields and self-reference, a BLOB read, both site CORS origins, legacy
Chinook record key `1`, and rejected writes.

## Deployment

The shared Go CI workflow downloads `demo-db/northwind`'s provider artifact at
its pinned repository commit, verifies its SHA-256, prepares both serving
fixtures, runs the integration journeys, and builds the checksummed Linux
binary. The deploy workflow downloads that exact binary and fixture artifact,
checks their source receipts, builds the image without compiling a new binary,
and deploys the existing Cloud Run service. It verifies both database
profiles, Chinook's cacheable DTQL result, and a Northwind query before
publishing the Cloud Run origin receipt consumed by the Cloudflare Worker
workflow.

After deployment, the Worker forwards `/.well-known/openvaultdb`, `/ovdb/*`,
and `/v1/*` to this service. Each website returns its own canonical database
identity while the shared API origin serves both database IDs. The legacy
ChinookDB API path continues to resolve through the same Chinook database.
