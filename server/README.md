# Read-only sample database service

The Go service mounts the database providers listed in [`providers.json`](providers.json)
through the real `openvaultdb-go` server. A provider entry pins the GitHub
repository commit, provider manifest, contract, checksum index, public database
manifest, SQLite export, license file, record-key adapter, and official browser
origins. CI fetches files only from `raw.githubusercontent.com` at those
immutable commits and verifies every pin before it prepares a fixture.

`prepare_providers.py` checks that the provider ID, public OVDB identity and
route, exported SQLite path, source digest, license, contract, and checksums
agree. It rejects mutable revisions, unsafe paths, invalid SQLite/FK data, and
individual source files over 25 MiB. A gzip-compressed export may be split into
ordered, individually pinned chunks; the preparer verifies every chunk and the
aggregate encoded hash, streams the full decoded SQLite file to a temporary
fixture, then verifies its decoded size and hash before adapting it. Decoded
fixtures are bounded to 2 GiB. It then creates a runtime inventory
with separate hashes for the original provider SQLite and the derived
serving-only SQLite. At startup, Go verifies those serving receipts and the
license bytes before mounting the inventory. Origins are assembled from the
per-provider configuration, so a new database does not require a change to Go
CORS code.

The provider artifact itself stays byte-for-byte unchanged. The adapter works
on a derived copy, adds a serving ID, and checks source and derived integrity,
foreign keys, row counts, native columns, primary keys, indexes, and view
definitions. It preserves composite primary-key order and supports keyless
tables through SQLite row identity without publishing an invented source key;
`WITHOUT ROWID` tables use their declared primary key. `MONEY` columns map to
numeric query fields, BLOB columns remain byte values, and native names,
including spaces and punctuation, are preserved in manifests; the SQLite
adapter quotes them only when generating SQL.
Chinook keeps its historical comma-separated composite identifiers. The live
OVDB mount exposes physical tables; provider views remain represented in the
published schema metadata and exports.

The adapter currently reserves a native column named `id` (case-insensitively), because the
read-only record adapter uses that name for serving identity. A source table
with that column fails preparation explicitly rather than overwriting it.
The generated `data.id` in OVDB record responses is this derived serving
identity; it is absent from source exports and native provider schema/model
metadata. Native primary keys and adapter record identities remain separate.

## Local checks

From `server/`, run the source/contract validations and real mount journeys:

```sh
python3 -m unittest discover -s . -p 'test_*.py' -v
python3 prepare_providers.py --inventory providers.json --output-dir /tmp/demodb-fixtures --local-root /Users/alex/projects
SAMPLE_DATABASES_INVENTORY=/tmp/demodb-fixtures/inventory.json \
CHINOOK_MANIFEST=/tmp/demodb-fixtures/chinook.yaml \
NORTHWIND_MANIFEST=/tmp/demodb-fixtures/northwind.yaml \
go test ./... -run 'Test(Public(Chinook|Northwind)Journey|InventoryMountAndQueryEveryProvider|LoadRuntimeInventory)' -count=1
```

`TestInventoryMountAndQueryEveryProvider` reads the generated inventory and
for each database mounts the actual read-only server, fetches its profile and
the configured smoke recordset schemas, runs representative queries, checks its
declared CORS origin, validates returned record keys, and confirms writes are
rejected. Entries can name multiple `smokeRecordsets` when one provider needs
specific physical tables checked, including keyless tables. CI performs the
same journey using the provider commits pinned in `providers.json`. An
`emptyRecordsets` entry is checked against the pinned native schema and queried
to ensure genuinely empty physical tables remain discoverable without invented
sample rows. Successful query rows are also fetched back by their serving ID;
the AdventureWorks smoke set covers composite-key history and its empty audit
tables. Providers may declare native binary columns in `blobSmokeFields`; the
generic smoke journey confirms non-empty encoded values without naming a
database or table in service code.

## Limits and the instance memory

The Cloud Run service runs with 512 MiB, one vCPU, five concurrent requests per
instance (`--concurrency=5`) and at most two instances; the deploy workflow states
all of it. A join the database cannot run as one statement (a document that reads
two databases, a subquery, a null test) is computed in the server's memory, and the
library only counts the JSON size of what it holds, so the limits are set in
[`limits.go`](limits.go) from measured memory, not from the counted bytes:

| Limit | Value | Library default |
| --- | --- | --- |
| In-memory query slots | 1 | 2 |
| Database-route query slots | 1 | 4 |
| Rows one request may read from sources | 40,000 | 100,000 |
| Snapshot spool | 2 slots of 64 MiB | 2 slots of 512 MiB |
| Join engines | `sqlite` | `sqlite`, `ingitdb` |

Query timeout (10 s), queue wait (1 s) and source bytes (64 MiB) are the library
defaults. `limits.go` carries the arithmetic: the server at rest, every slot busy and
every other request at its largest add up to 434 MiB of 512. `TestCloudLimitsFitTheInstance`
reads `--memory` and `--concurrency` from the deploy workflow and fails when the sum
exceeds 85% of the memory, so a change to a limit, to the workflow or to the
catalogue of providers (`TestCloudMeasurementsCoverThePinnedProviders`) has to bring
the arithmetic along. A request over a limit gets the library's answer: `422
query_budget_exceeded` with the budget it reached, `503 query_capacity` when no slot
frees within the queue wait, `413 snapshot_too_large` for a larger snapshot.
`/.well-known/openvaultdb` states the limits and the join engines this server
enforces.

Structured queries on PostgreSQL stay off: neither the server code nor the deploy
workflow sets the library's preview switch, and `TestPostgresPreviewStaysOff` fails
if either does.

## Add a database

Publish its immutable source fixture, provider manifest, contract, checksums,
public `ovdb-database.json`, and license in a `demo-db` provider repository.
Then add one entry to `providers.json` with the full repository commit and the
SHA-256/byte-size pins for the six declared files. `prepare_providers.py`
rejects mismatched metadata before mounting. The new provider is automatically
included in the generated runtime inventory, per-entry CORS list, fixture
integrity checks, and generic deploy smoke journey; the server's Go code does
not branch on database IDs. A new or re-pinned provider also changes the memory
the limits were measured against: re-measure as described in
[`limits.go`](limits.go) and update it together with `limits_test.go`.

The CI job builds and tests the Linux binary with all pinned fixtures, then
publishes that exact binary plus the verified fixture directory. The Cloud Run
deploy job checks the binary and every provider receipt, deploys the existing
service, and runs profile, collection, query, CORS, and read-only checks for
each inventory entry before it publishes the service-origin receipt consumed
by the Cloudflare Worker deploy. Deployment keeps the existing Cloud Run
service, region, memory and CPU, Worker origin, and compatibility environment
variables; the per-instance concurrency, which the memory arithmetic depends on, is
written out in the deploy command.
