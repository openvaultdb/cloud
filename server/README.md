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

The default legacy adapter reserves a native column named `id` (case-insensitively), because the
read-only record adapter uses that name for serving identity. A source table
with that column fails legacy preparation explicitly. The offline opt-in adapter
described below preserves that source field and allocates a separate serving column.
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

## Add a database

Publish its immutable source fixture, provider manifest, contract, checksums,
public `ovdb-database.json`, and license in a `demo-db` provider repository.
Then add one entry to `providers.json` with the full repository commit and the
SHA-256/byte-size pins for the six declared files. `prepare_providers.py`
rejects mismatched metadata before mounting. The new provider is automatically
included in the generated runtime inventory, per-entry CORS list, fixture
integrity checks, and generic deploy smoke journey; the server's Go code does
not branch on database IDs.

The CI job builds and tests the Linux binary with all pinned fixtures, then
publishes that exact binary plus the verified fixture directory. The Cloud Run
deploy job checks the binary and every provider receipt, deploys the existing
service, and runs profile, collection, query, CORS, and read-only checks for
each inventory entry before it publishes the service-origin receipt consumed
by the Cloudflare Worker deploy. Deployment keeps the existing Cloud Run
service, region, resource limits, Worker origin, and compatibility environment
variables.

## Offline opt-in preparation

Production `providers.json` remains version 1. Its six providers keep their
existing `id` serving column, key formats, YAML and runtime receipt shape.
The preparer accepts only inventory versions 1 and 2, known entry fields, and
pinned file descriptors. Unknown versions, adapter/profile values or fields
fail before provider fetching.

A separate development inventory can use version 2 and opt each entry into
`servingAdapter: "separate-id/1"` and `readProfile: "bounded-immutable/1"`.
The adapter chooses `__ovdb_record_id`, or the smallest unused positive suffix,
against actual case-insensitive column names. It similarly allocates its unique
index against existing schema objects. Native `id`/`ID`, helper-like names,
indexes, foreign keys, views, and typed native values remain intact. Native row
values are verified with a streaming typed digest in primary-key/rowid order.
Transport IDs use the existing `recordKeyFormat`; their strings do not establish
native identifier semantics. Empty or non-unique transport identities refuse
preparation. Sources remain unchanged unless the caller explicitly consumes a
verified temporary staging source.

Only opt-in YAML adds `storage.sqlite.record_keys` and `busy_timeout: 0s`.
The YAML owns the complete table-to-serving-column map. Version 2 generated
`inventory.json` adds `manifestSha256` and copies supplied adapter/profile
values, without a second key map. This is **offline artifact preparation only**:
the current Go runtime rejects inventory version 2, and its existing manifest
parser rejects the new SQLite fields. `readProfile` is a future runtime policy
input; preparing it does not enforce HTTP limits or authorize publication.
Do not change production provider entries until released OVDB/SQL prerequisites
and the separately reviewed runtime integration have landed.

The follow-on adoption work owns public route/homepage/descriptor/attribution
preparation, descriptor field naming, runtime inventory validation, verified
manifest mounting, four-pin immutable reads, request deadlines/order/CORS,
smoke, and full six-plus-two capacity/publication proof. This offline version 2
subset deliberately refuses those future input fields rather than silently
ignoring them. W1 no-website wrappers therefore await that integration.

All immutable network assets use the exact raw GitHub HTTPS origin and a full
40-hex commit. Every relative path segment is validated before URL encoding;
redirects, including same-origin redirects, are refused before following.
Physical files remain bounded at 25 MiB, encoded gzip streams at 512 MiB and
decoded fixtures at 2 GiB. Ordered pinned stream fragments and gzip members
are verified independently and in aggregate; partial temporary files are
removed after failures. HTTP error bodies are closed without reading or
logging them. Deployment receipt hashes use streaming reads with unchanged
SHA-256 semantics, targets, resources and workflow triggers.

Focused synthetic verification (no provider corpus download or service start):

```sh
wb run -- python3 -m unittest discover -s server -p 'test_*.py' -v
```

Successful Cloud Worker CI on `main` triggers the existing production deployment
workflows. PR validation does not deploy; the landing owner controls when to
merge this offline change.
