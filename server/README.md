# Read-only sample database service

## Query routes

Every query route is `/tugql` (`/v1/databases/{id}/tugql`, `/v1/tugql`, and the
discovery key `endpoints.tugql`). ECB, ECB-public and IANA admission documents
must name the `/tugql` paths in their `path`, `workerPath` and `goPath` fields.

## Operator-selected ECB original HTTP query path

`ecb_candidate.go` compiles the pinned host composition for the original ECB
daily HTTP resource. It remains off by default. The Go service selects it only
when `OVDB_ECB_ENABLED=true`, `OVDB_ECB_HOST_CONFIG` names a separately
reviewed operator JSON document, and `OVDB_ECB_PROXY_SECRET` supplies a
dedicated 32 to 128 character ASCII letter, digit, `_` or `-` secret. A present candidate file alone does nothing.
Selection also requires `OVDB_ECB_ADMISSION_FILE` and its exact raw-byte
SHA-256 in `OVDB_ECB_ADMISSION_SHA256`. This operator-owned decision is
separate from the OVDB preparatory B1 proposal. Its closed JSON format is
`ovdb-ecb-b1-operator-admission/1` with decision
`operator-free-transient-read-only`, nonempty `approvedBy`, RFC 3339
`approvedAt`, and these exact fields: `hostConfigSHA256` of the operator host
JSON, `publisherManifestSHA256` equal to the pinned original publisher SHA in
`ecb_candidate.go`, `rightsDigest` equal to its pinned rights SHA,
`executorId: openvaultdb-cloud`, `resourceId: ecb-daily`, `method: POST`,
`path: /v1/databases/ecb/tugql`, `maxReadsPerExecution: 1`, `maxRows: 50`,
`publicAccess: false`, and `paidAccess: false`. The gate refuses a missing,
altered, duplicate-key, broader or unbound decision before mounting and
rechecks the host-config digest during assembly. The named approver is an
operator assertion, not an identity proof by this service. Keep both JSON
files and the local HTTP manifest under operator control; review their exact
raw-byte hashes before configuring a service.

An operator can prepare the decision from this shape after independently
reviewing the exact pinned host JSON and ECB terms. Replace the placeholder
digest and approver; never deploy this example as an approval:

```json
{
  "format": "ovdb-ecb-b1-operator-admission/1",
  "decision": "operator-free-transient-read-only",
  "approvedBy": "<operator identity>",
  "approvedAt": "<RFC 3339 time>",
  "hostConfigSHA256": "<SHA-256 of exact host JSON bytes>",
  "publisherManifestSHA256": "399ce77bc4513b1a819f61e26a54fe8e6c46569b2b45ea078582d9ad6758697e",
  "rightsDigest": "08669fda7a7d255d1c77d2be733e587a23bb2ff1a916e96c727b82d2540cdc93",
  "executorId": "openvaultdb-cloud",
  "resourceId": "ecb-daily",
  "method": "POST",
  "path": "/v1/databases/ecb/tugql",
  "maxReadsPerExecution": 1,
  "maxRows": 50,
  "publicAccess": false,
  "paidAccess": false
}
```

The [ECB reuse conditions](https://www.ecb.europa.eu/services/using-our-site/disclaimer/html/index.en.html)
require accurate reproduction and ECB source citation, an explicit notice of
modification, and specific free-original notices when information is put in
sold documents. The selected `sourceRight` carries attribution, the free ECB
resource link and the XML-to-row transformation notice. Whether a paid SaaS
presentation falls under the sold-document condition, and whether its UI meets
the before-payment and each-access notice duties, is a separate rights decision.
This operator-only admission does not infer it.

The selected Go handler admits only `POST /v1/databases/ecb/tugql`; other ECB
paths, passive reads and query strings do not reach the ECB provider. Existing
sample routes retain their current handler.

The candidate assembly requires an operator-owned JSON document with format
`ovdb-ecb-host-candidate/1`, paths to the original publisher manifest and a
separate local HTTP storage manifest, the reviewed publisher commit and Git
blob, decoder version and module version, and the exact detached `sourceRight`
and `binding` from the reviewed preparatory B1 proposal. It verifies the
original publisher bytes, SHA-256 and Git blob, and the complete rights digest
before mounting. The local storage manifest must match `ecbHTTPManifest`
byte for byte:
strict `ecb/daily`, fixed `ecb-daily/1` HTTP profile, native string fields and
no retention. The server also checks the mounted manifest after opening it.
It uses the stable `openvaultdb-cloud` rights identity and the library's
`ecb-public-free/1` request profile. Request Host, query data and command-line
flags cannot select this composition.

The public Go gateway refuses ECB and IANA routes with 503 regardless of
operator environment flags. Existing native admission, integrity and redaction
suites remain available for the separate default-off components. No public
activation or operator admission is granted by the gateway port.

The separate [IANA operator query component](IANA.md) is also default off and
synthetic-only. It requires its own reviewed admission and credentials; this ECB
configuration does not enable it.

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
numeric query fields, `DECIMAL_TEXT(p,s)` fields retain their provider-declared
precision and scale with string storage, BLOB columns remain byte values, and
native names,
including spaces and punctuation, are preserved in manifests; the SQLite
adapter quotes them only when generating SQL.
Chinook keeps its historical comma-separated composite identifiers. The live
OVDB mount exposes physical tables; provider views remain represented in the
published schema metadata and exports.

## Origin secret

Production gateway startup requires `OVDB_ORIGIN_SECRET` (32–128 characters of
`A-Za-z0-9_-`). Every request must carry exactly one matching header named by
`OVDB_ORIGIN_SECRET_HEADER` (default `X-OVDB-Origin-Secret`), otherwise 403.
The edge must overwrite that header for both admitted hosts; the constant-time
validator removes it before inner handlers/proxies. No HTTP health bypass exists.
See the root README for the source-only mapped-domain cutover and future gates.

## Optional DemoDB PostgreSQL editions

The runtime can additionally mount the six native DemoDB editions as
`chinook-postgresql`, `northwind-postgresql`, `pubs-postgresql`,
`sakila-postgresql`, `adventureworks-postgresql`, and `employees-postgresql`.
Set `OVDB_DEMODB_POSTGRES_ENABLED=true` and bind each corresponding
`OVDB_PG_CHINOOK_DSN`, `OVDB_PG_NORTHWIND_DSN`, `OVDB_PG_PUBS_DSN`,
`OVDB_PG_SAKILA_DSN`, `OVDB_PG_ADVENTUREWORKS_DSN`, and
`OVDB_PG_EMPLOYEES_DSN` from Secret Manager. An unset or false feature flag
leaves the SQLite-only inventory unchanged. DSNs are only read by the server and
are not written into the image, inventory, discovery responses, logs, or
temporary manifests.

### What enabling it exposes

Turning `OVDB_DEMODB_POSTGRES_ENABLED` on publishes **full public TugQL** on every
mounted edition, to anyone who can reach the service. The library's
`OVDB_PREVIEW_POSTGRES_QUERIES` switch is not involved: a native read-only mount
answers `core.CanQuery()` itself, and discovery (`/.well-known/openvaultdb`)
then lists `postgres` among the join engines. Callers get filters, ordering,
offsets, equality joins across up to 8 sources, grouping with aggregates and
`HAVING`, and `EXISTS`/`IN` subqueries, under the limits in
[Limits and the instance memory](#limits-and-the-instance-memory). A document over
one database without a subquery (single-source reads, joins, grouping) is
compiled to SQL and runs inside PostgreSQL; a document with a subquery runs in the
server's in-memory engine. Writes are refused, and the legacy `/query` route
answers 501 for these mounts, and so do keyed record reads and writes
(`GET /v1/databases/<id>/records/<collectionId>/<id>` answers
`501 operation_unsupported`, not `500 internal`; fixed in v0.25.3).
`TestNativePostgresMountExposesFullPublicTugQL`
checks exactly this against a real database.

### Each mount is optional

Startup never waits for or fails on PostgreSQL. The SQLite databases are mounted
synchronously; each PostgreSQL edition is mounted in the background, at most two
at a time, and independently of the others:

- a DSN that is not bound is reported as `not_configured`;
- a DSN refused by configuration (TLS mode, repeated keys, `options`, missing
  `connect_timeout`, `PG*` environment variables,
  `OVDB_NATIVE_POSTGRES_INSECURE_TRANSPORT`) is `misconfigured` and is not
  retried until a restart with a fixed secret;
- a failed connection, self-check or catalogue discovery, or an attempt that
  takes longer than its budget (90 s; `OVDB_DEMO_POSTGRES_ATTEMPT_TIMEOUT`, a Go
  duration from `1m` to `10m`, overrides it, and a value outside that range stops
  the revision from starting), is `unavailable` and retried after 30 s, doubling
  to at most 15 minutes, without a restart. The library's catalogue deadline is
  the budget less 30 s (the longest `connect_timeout` a DSN may set): 60 s by
  default. Since v0.25.5 the library discovers a PostgreSQL schema in two batched
  queries instead of one per relation, so the default 60 s is ample (the
  AdventureWorks mount went from about 12 s to about 0.7 s at 100 ms round-trip
  time); an override of `1m` leaves discovery 30 s. The override is validated
  only when native PostgreSQL is enabled and at least one source is configured.
  A catalogue that does not finish in time is reported as `timeout`, not
  `mount_failed`. An abandoned attempt keeps its concurrency slot until the
  underlying mount really ends (bounded by the DSN's `connect_timeout` and the
  library's discovery deadline) and is closed; shutdown waits for it.

Every outcome is logged as one line naming the database and the reason, with
the DSN and its host, port, database, user and password redacted.
`GET /v1/demodb/postgres-mounts` returns, per edition, the state, a stable
reason code (`not_configured`, `tls_required`, `dsn_rejected`, `role_unsafe`
(the service's own role self-check or the library's refusal of a writable role),
`schema_scope`, `pooler_endpoint`, `connection_failed`, `timeout`,
`mount_failed`, `mount_rejected`, `stopping`), the attempt count and the next
retry; it never serves error text (the path avoids Cloud Run's reserved `…z`
paths such as `/healthz`). While an edition is not mounted, requests to
`/v1/databases/<id>…` get `503 database_unavailable` with `Retry-After`, and
`postgres` is not advertised as a join engine until at least one edition is
mounted.

### Mount hardening and operator requirements

The service pins openvaultdb-go **v0.25.5**; v0.25.2 was the first release whose native
read-only PostgreSQL mount hardens itself. Two independent layers now guard
every edition, and an edition is mounted only when both accept it.

**The library (openvaultdb-go v0.25.5), set in `applyLibraryMountHardening`
in `native_postgres_guard.go`:**

- **BREAKING in v0.25.2:** a mount to a remote host requires verified TLS
  (`sslmode=verify-full` or `verify-ca`, no plaintext fallback); loopback and
  Unix-socket hosts are exempt. The library's opt-outs
  (`mount.Options.AllowInsecureNativePostgresTransport`, the manifest field
  `storage.postgres.insecure_transport`, the environment variable
  `OVDB_NATIVE_POSTGRES_INSECURE_TRANSPORT`) are never used here: the option is
  forced off, the generated manifest does not set the field, and the server
  refuses every edition (`tls_required`) while the variable is set.
- every session starts with `default_transaction_read_only=on` and a
  `statement_timeout` of **15 s** (`NativePostgresStatementTimeout`), written
  after anything the DSN sets, so it wins over the DSN and over a role default;
  every read also runs in its own `BEGIN READ ONLY` transaction;
- `RequireReadOnlyNativePostgresRole`: the mount fails when the role, or any
  role it is a member of, is a superuser or has CREATEROLE/CREATEDB/REPLICATION/
  BYPASSRLS, is a member of `pg_write_all_data` or a server-file/program role,
  owns any schema, relation or function, may write an exposed relation, may
  create in an exposed schema or `public`, may use or update a sequence there, or
  may execute a SECURITY DEFINER function there;
- catalogue discovery is bounded by `NativePostgresDiscoveryTimeout` (the
  attempt budget less 30 s, see above).

**This service, before it lets the library connect (`checkDemoPostgresSource`):**

- the DSN names exactly one host, sets every key once (parsers disagree on
  which occurrence of a repeated key wins), uses no service file, sets
  `sslmode=verify-full` (stricter than the library: `verify-ca` and `require`
  are refused) and a `connect_timeout` of 1-30 s, and sends no session
  parameter other than `statement_timeout`, `lock_timeout`,
  `idle_in_transaction_session_timeout` or `application_name` (in particular no
  `options`, `search_path` or `default_transaction_read_only=off`). The
  configuration pgx actually derives is then checked too: TLS on, hostname
  verification on, no plaintext fallback;
- no `PG*` connection environment variable (`PGSSLMODE`, `PGHOST`, `PGOPTIONS`,
  `PGSERVICE`, ...) and no `OVDB_NATIVE_POSTGRES_INSECURE_TRANSPORT` is set in
  the process;
- a loopback host is **not** exempt here, unlike in the library: a loopback DSN
  without `verify-full` is accepted only with `OVDB_PG_INSECURE_TLS_FOR_TESTS=true`
  (the local real-PostgreSQL harness). It has no effect on a remote host and is
  never set by the deploy. The two policies agree on every remote host; on
  loopback the service is the stricter one;
- its role passes a live self-check: not `postgres`, not `neondb_owner` or any
  `*_owner` role, no superuser/CREATEROLE/CREATEDB/REPLICATION/BYPASSRLS;
  `session_user` equals `current_user`; not a member, with or without INHERIT
  (a member can still `SET ROLE`), of any role that has those attributes, owns a
  relation, schema or the database, or is one of `pg_write_all_data`,
  `pg_read_all_data`, `pg_execute_server_program`, `pg_read_server_files`,
  `pg_write_server_files`, `pg_database_owner`, `pg_signal_backend`,
  `pg_create_subscription`, `pg_checkpoint`, `pg_maintain`; does not own and
  cannot INSERT/UPDATE/DELETE/TRUNCATE any relation (not only the exposed ones)
  or update any sequence; cannot CREATE in the database or any schema;
- the only schema in which the role can read any relation is the edition's own
  (`chinook` for `chinook-postgresql`, and so on). After mounting, every
  discovered collection is checked again to belong to that schema.

The role checks overlap on purpose and are both kept: the service's covers the
whole database, the owner-name convention, `pg_read_all_data` and the other
built-in roles, database ownership and the schema scope; the library's covers
function ownership and SECURITY DEFINER functions, which the service's does not
look at (`TestDemoPostgresLibraryHardeningAgainstRealPostgres` mounts such a
role past the self-check and proves the library refuses it). The only check
removed with the bump is the service's former requirement that the role or DSN
carry a `statement_timeout` (reason code `statement_timeout_required`): the
library now sets one on every session, at least as strictly.
`mount.All`/`Spec.Optional` is not used: `demoPostgresMounter` already isolates
each edition and, unlike `mount.All`, retries a failed one with backoff.

**Operator checklist for the six Neon secrets:**

1. Use six separate Neon databases, or six roles each granted `USAGE` and
   `SELECT` on one schema only. A role that can read two editions' schemas is
   refused for both.
2. Use a dedicated reader role created with SQL (`CREATE ROLE chinook_reader
   LOGIN PASSWORD …`, then `GRANT USAGE ON SCHEMA chinook` and `GRANT SELECT ON
   ALL TABLES IN SCHEMA chinook`); never the owner role (`neondb_owner`) and
   never a role created in the Neon Console or API: those are made members of
   `neon_superuser` (CREATEDB, CREATEROLE), so the self-check refuses them.
   Grant nothing else: no sequence `USAGE`, no `EXECUTE` on SECURITY DEFINER
   functions.
3. Before activation, list the SECURITY DEFINER functions the reader can
   execute. The library refuses a role that may execute one in an exposed
   schema, **including `public`**, and PostgreSQL grants `EXECUTE` to `PUBLIC`
   by default, so a stray function there fails the mount (reason `role_unsafe`).
   Run [`docs/list_security_definer_functions.sql`](docs/list_security_definer_functions.sql)
   as an admin role on each database (no secrets in the file); it is this query:

   ```sql
   SELECT n.nspname, p.proname, pg_get_function_identity_arguments(p.oid) AS args
   FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
   WHERE p.prosecdef
     AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_%'
     AND has_function_privilege('chinook_reader', p.oid, 'EXECUTE');
   ```

   For every row, `REVOKE EXECUTE ON FUNCTION <schema>.<function>(<args>) FROM
   PUBLIC;` (the script prints the statement), then re-run it until it returns
   nothing. Also consider `ALTER DEFAULT PRIVILEGES … REVOKE EXECUTE ON
   FUNCTIONS FROM PUBLIC` for the owner that creates functions.
4. Use Neon's **direct** endpoint (`ep-….neon.tech`), never the `-pooler`
   endpoint. PgBouncer rejects unknown startup parameters unless
   `ignore_startup_parameters` is set, and the mount sends `statement_timeout`
   and `default_transaction_read_only`; whether they survive the pooler is
   untested, so the service **fails closed**: a host whose first label ends in
   `-pooler`, or that contains `-pooler.` or `pgbouncer`, is refused with status
   reason `pooler_endpoint` before any connection is made.
5. As an extra backstop, `ALTER ROLE chinook_reader SET statement_timeout =
   '15s'` (and likewise for each reader). It duplicates the library's startup
   parameter harmlessly and also bounds any session that bypasses the mount.
6. Write `sslmode=verify-full&connect_timeout=10` in each DSN. A
   `statement_timeout` in the DSN is no longer needed (the library's 15 s
   overrides it).
7. Never set `OVDB_NATIVE_POSTGRES_INSECURE_TRANSPORT`,
   `OVDB_PG_INSECURE_TLS_FOR_TESTS` or any `PG*` variable on the service.

### Collection IDs and the all-collections journey

The runtime manifest for each mount is created temporarily and uses the
library's native PostgreSQL collection IDs. The consumer excludes only the
selected schema's `_import_manifest` relation. The SQLite-only memory
measurements below do not cover the additional pools and schemas. Keep this
opt-in disabled in production until the shipping-image and all-six live
memory/query proof pass.

`TestDemoPostgresReadOnlyPublicJourney` reads **every** collection of every
edition and is strict: any failed read fails it. (openvaultdb-go v0.24.1 refused
46 relations whose encoded collection ID is longer than 63 bytes, 43
AdventureWorks relations and three Northwind views; v0.25.2 fixed it, and
`knownLongNameCollectionFailures` is empty. An entry added there still fails the
journey once the listed relation starts reading.)
`TestDemoPostgresEveryCollectionBattery` additionally runs the library's
`servertest.RequireEveryCollection` battery (bounded and streamed reads, an
order by each field, equality filters on sampled values, offset pages,
out-of-range integer refusals) on all 166 collections. To run them against a
real PostgreSQL 17 loaded from the DemoDB SQL files, set the six DSNs (and, for
a loopback server without TLS, `OVDB_PG_INSECURE_TLS_FOR_TESTS=true`):

```sh
export OVDB_PG_INSECURE_TLS_FOR_TESTS=true
for db in chinook northwind pubs sakila adventureworks employees; do
  export "OVDB_PG_$(echo $db | tr a-z A-Z)_DSN=postgres://demo_reader@127.0.0.1:55440/$db?sslmode=disable&connect_timeout=10"
done
# optional: a superuser on the chinook database, for the privileged-membership
# and library-hardening scenarios (they create and drop throwaway roles, a view
# and a function in the chinook schema)
export OVDB_PG_TEST_PRIVILEGED_DSN="postgres://postgres@127.0.0.1:55440/chinook?sslmode=disable&connect_timeout=10"
go test -count=1 -run 'DemoPostgres|NativePostgres' -v .
```

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

## Limits and the instance memory

AdventureWorks publishes 48 provider-declared exact-decimal field descriptors in its database metadata. These values remain JSON strings through record reads and snapshots. TugQL exact calculations are an explicit `money` opt-in on supported aggregate queries, use bounded 38-digit decimal arithmetic with half-even rounding, and remain subject to the existing source-row and memory limits. Default numeric behavior is unchanged; source computed columns remain snapshots rather than being recomputed.

The Cloud Run service runs with 512 MiB, one vCPU, two concurrent requests per
instance (`--concurrency=2`) and at most two instances; the deploy workflow states
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
| Join engines | `sqlite` (PostgreSQL is opt-in) | `sqlite`, `ingitdb` |

Query timeout (10 s), queue wait (1 s) and source bytes (64 MiB) are the library
defaults. `limits.go` carries the arithmetic: the server at rest, the snapshot spool
and the two heaviest requests one instance can hold at once add up to 370 MiB of 512.
Those two are an in-memory request (90 MiB) and a read of a whole collection by the
query endpoint (72 MiB), which no gate counts and which applies no row limit of its
own: the library reads until an 8 MiB buffer is full. Only the instance's
concurrency bounds how many such reads run together, which is why it is 2: at 3 the
worst case is 442 MiB, over the 85% (435 MiB) that `TestCloudLimitsFitTheInstance`
allows. The exact Money grouping regression reads and groups 31,465 SalesOrderHeader rows, returns the first five ordered groups, and held 77.6 MiB with OpenVaultDB v0.14.2; Product and PurchaseOrderDetail aggregates held 1.7 MiB and 4.1 MiB. Four Money accumulators at that grouping cardinality return 422 at the existing 64 MiB aggregation cap, with no partial rows. The Linux amd64 deployment binary is 55,397,616 bytes (52.8 MiB). That test reads `--memory` and `--concurrency` from the deploy workflow,
fails when the sum exceeds 85% of the memory or when one more request per instance
would also fit, so a change to a limit, to the workflow or to the catalogue of
providers (`TestCloudMeasurementsCoverThePinnedProviders`) has to bring the
arithmetic along. `TestCloudSingleCollectionReadsStayInsideTheLibraryBuffer` reads
every collection through the query endpoint and fails when an answer outgrows the
buffer the figure is measured on; `TestCloudMeasureSingleCollectionReads` (set
`OVDB_MEASURE_MEMORY=1`) repeats the memory measurement itself. A request over a
limit gets the library's answer: `422 query_budget_exceeded` with the budget it
reached, `503 query_capacity` when no slot frees within the queue wait, `413
snapshot_too_large` for a larger snapshot. A whole-collection read that outgrows the
8 MiB response buffer is not a successful result: the default `application/json`
stream is aborted after rows have been written, while a caller that negotiates
`application/vnd.openvaultdb.query-stream+json` receives a bounded terminal
`complete:false` with a `query_budget_exceeded` / `response_bytes` error. The
fixture-backed capacity test checks the negotiated terminal and counts it as a
refusal, never as an answered read. `/.well-known/openvaultdb` states the limits
and the join engines this server enforces. It also states the library's fixed
ceiling of 100,000 groups; with a budget of 40,000 source rows a grouping cannot
reach more groups than the rows it reads, so 40,000 is the effective ceiling.

Neither the server code nor the deploy workflow sets the library's
`OVDB_PREVIEW_POSTGRES_QUERIES` preview switch, and
`TestServiceNeverSetsLibraryPostgresPreviewSwitch` fails if either does. That
switch only governs ordinary PostgreSQL manifests; it does **not** keep
PostgreSQL queries off. The native read-only DemoDB editions serve full public
TugQL as soon as `OVDB_DEMODB_POSTGRES_ENABLED` is on (see
[What enabling it exposes](#what-enabling-it-exposes)).

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

The CI job builds and tests the Linux binary with pinned fixtures and embeds
its gateway assets. Deployment checks the binary and complete native archive,
creates a no-traffic revision, verifies its exact image/source identity and routes
that immutable name. Live acceptance is mandatory; rollback requires a separately
accepted secure revision and HTTP rechecks. Resource limits remain 512 MiB,
1 CPU and concurrency 2. See the root README for the manual two-stage controller.

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

For `readProfile: "bounded-immutable/1"`, a closed `publisherManifest`
`{path, sha256, bytes}` is mandatory. It is fetched from the same repository and
full revision as other metadata, verified before the SQLite artifact is fetched,
and parsed by `cmd/publisher-selection` using the existing pinned Go YAML library.
Opt-in preparation needs the Go toolchain and module cache and invokes the helper
once per provider. Legacy preparation remains standalone Python. The YAML parser
rejects aliases, anchors, duplicate keys, unknown root fields, multiple documents
and files over 2 MiB.

The pinned publisher's `recordsets` is the sole selection authority. Only those
physical tables receive serving helpers, indexes, schema collections and the
complete `storage.sqlite.record_keys` map with `busy_timeout: 0s`. All native
tables, including unselected diagnostics, retain their metadata and typed values
in the serving copy. The public descriptor's selected native schema, homepage,
Cloud routes and query availability must agree with provider/publisher metadata.
There is no provider-name selection branch or second authored selection list.

Runtime inventory version 2 adds `manifestSha256`, adapter/profile and publisher
pin, a closed `publicDescriptor` pin and explicit boolean `requirePublishedQuery`.
Preparation copies checked bytes to `<id>.publisher.yaml` and `<id>.descriptor.json`;
original pin paths identify the files in the same pinned provider repository.
Before mounting, runtime checks these pins, selection, generated schema/key
allocation and native field types, exact routes and descriptor capability flags.
Mixed version 2 inventories include legacy entries with manifest hashes. Version 1
retains its receipt shape and refuses opt-in profile fields.

Bounded startup streams SQLite through a held handle into a new private directory,
hashes exactly the copied bytes, writes exactly the hashed/parsed manifest bytes
there, then seals the directory (0500) and files (0400) before mounting. The driver
opens private paths; replacing original files cannot replace mounted bytes. The
service exposes only the handler, with no remount API. This trusts the process
owner and OS credentials; another process with the same credentials deliberately
modifying private files is outside this boundary. Closing a database removes its
snapshot; failed startup removes staging copies. Metadata is bounded to 2 MiB per
file and SQLite to 2 GiB per provider. The additional complete disk copy and startup
hashing cost require six-plus-two capacity measurement; tiny fixtures establish no
production memory/disk or latency headroom. Using the current native snapshot
sizes only as an estimate, ROR's 238,563,328 bytes add 227.51 MiB (44.44% of
512 MiB); Geo's 75,304,960 bytes add 71.82 MiB. Together these copies alone are
299.33 MiB (58.46%), before serving helpers, the seven SQLite providers and
native Shipping Logs provider, heap,
SQLite connections/cache and snapshot spooling. These are source-size arithmetic,
not measured serving sizes or admitted memory use. Cloud Run's [writable
filesystem uses instance memory](https://docs.cloud.google.com/run/docs/container-contract#filesystem).
If this copy approach fails the later unchanged 512 MiB capacity gate, an image
immutable binding must receive its own reviewed identity proof; do not increase
resources or truncate native diagnostics to conceal the cost.

`server.NewChecked` derives immutable per-database profiles from checked inventory.
Candidate query advertisement stays false; final advertisement requires matching
descriptor flags. The library mount verifies every selected serving ID. Legacy
providers retain their read/snapshot behavior and existing limits, including the
48 AdventureWorks decimal fields.

The production `providers.json` and protected image serve the six established
SQLite providers, the immutable Shipping SQLite provider, the Countries
reference SQLite provider (`demo-db/countries`: ISO codes, currency and dated
World Bank population, CC BY 4.0), and the original fourteen-file Shipping Logs
inGitDB store (six collections: baseline and carrier-gateway streams, each with
their separately stored 11045/11062 recovery examples). The publication smoke reads Countries rows for the demo's ship
countries, its `USA`/`UK` aliases and the World Bank attribution row. The central
`https://demodb.dev/.well-known/openvaultdb` catalogue (demo-db/websites) lists only
datasets with a demodb.dev host, so it must list the other eight and may omit Countries.
After a failed deploy, the rollback smoke (`--rollback-acceptance`) fully checks only the
providers the older rollback image serves at this inventory's four pins, and records
absent or differently pinned ones in its receipt. Shipping Logs is read directly through
the published inGitDB DAL; preparation and runtime do not create a parsed log
database, cache, or snapshot. Native PostgreSQL is a separate runtime opt-in and
does not change that inventory. Four-pin HTTP guards, deadline/cache middleware, gateway
propagation, public descriptor routes, attribution downloads, deploy verification
and smoke, actual Geo/ROR preparation and all-key proof, six-plus-two capacity and
live journeys, final wrapper repins, Directory and App admission remain downstream
gates. The six PostgreSQL sources must pass their all-source runtime proof before
the production opt-in or public edition admission is enabled.

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

Cloud Go CI builds an artifact; explicit manual deployment selects its exact
successful current-main run. PR validation does not deploy.

### Explicit selected storage at startup

The production scratch image fixes `OVDB_SELECTED_STORAGE=protected-image` and
uses only `/srv/fixture/inventory.json`. Before inventory access, Linux startup
requires actual UID/GID 65532 and checks `/`, `/srv`, the fixture directory and
its complete consumed tree: root ownership, ordinary directories without
non-owner writes, regular singly-linked nonwritable files, no symlinks/special
files, bounded metadata/SQLite sizes and no SQLite journal/WAL/SHM companions.
Preparation rejects nested, hidden, linked and special output entries. The flat
file set permits numeric Dockerfile frontend 1.2 `COPY --chmod=0444`; this avoids
requiring newer symbolic-mode support. The build sets modes and owners using
`COPY`; startup never chmods or copies
image assets. `/tmp` is separately created with sticky mode 1777 for existing
bounded legacy spools. Changing paths, using a legacy manifest override,
observing relevant permission-bypass capabilities or an additional mount over
`/srv`/the fixture tree, or failing any pin/profile check aborts before a handler
or listener. There is no automatic fallback or partial provider omission.

The trusted build/deploy owner must inspect the final merged image filesystem
and record the exact image digest and revision configuration: no covering
volume, mutable backing, external writer or application authority that defeats
these permissions. The process owner/platform administrator remains trusted.
Available Linux UID/group/capability/NoNewPrivs/mount observations are recorded;
missing `/proc` observations alone rely on that admitted Cloud deployment
profile rather than pretending to provide universal platform attestation.
Observed unsafe evidence is never waived. NoNewPrivs is diagnostic and does not
revoke existing authority ([Linux documentation](https://docs.kernel.org/userspace-api/no_new_privs.html)).

Each SQLite file is streamed and pinned once at inventory admission, with
checkpointed rollback-mode header and opened-file identity checks. Selected
mounts reopen the exact protected manifest and `./<id>.sqlite` through the
existing library API; the same fixed paths apply to driver, schema, sandbox and
later connection opens. Direct-mode close/failure never removes image assets.
All native diagnostics remain physically present and outside public routes.
The production inventory contains the six established providers plus Shipping
and native Shipping Logs. The deployment gate preserves the established six,
checks both additions through the existing public plural routes, and records a
rollback receipt that proves the additions are absent after restoring the prior
revision.

For selected development/tests explicitly set `OVDB_SELECTED_STORAGE=sealed-copy`
and `SAMPLE_DATABASES_INVENTORY` to the prepared inventory. This retains the
captured private copy and cleanup/replacement tests. Unset storage retains the
historical legacy-only environment contract; it refuses selected providers.
Protected-image mode refuses non-Linux hosts.

`python3 test_image_runtime.py` requires an already-running Linux Docker engine;
it never installs/starts one. The disjoint `image-runtime.yml` CI job runs the
finite synthetic experiment without listeners, mapped ports, secrets or deploys.
It builds the production Dockerfile, records the final image config and entry
modes, runs the real guard and checked handler as UID 65532, proves 16 native /
11 selected tables, schema/ordinary/key reads, real SQLite reconnection, full
handler reopen, denied SQL/filesystem writes, no selected writable copy, and
hostile-image refusal. Its JSON receipt is a CI artifact. Cross-compilation and
host tests do not substitute for this Linux result.

This component does not admit actual Geo/ROR capacity. Complete six-plus-two
fidelity/key proof, measured instance peak <=435 MiB under unchanged 512 MiB /
1 CPU / concurrency 2, retained legacy spools, idle recovery and the existing
10-second cancellation contract remain gates. Full startup must finish before
listening within the actual configured startup-probe budget and Cloud Run's
four-minute startup limit; record the observed margin. Tiny fixtures do not
prove these conditions. The deploy owner must separately verify the actual
Cloud revision and total memory accounting ([Cloud Run startup contract](https://docs.cloud.google.com/run/docs/container-contract#startup),
[memory limits](https://docs.cloud.google.com/run/docs/configuring/services/memory-limits)).
Transport pins/CORS, descriptor routes, wrappers and Directory/App public
admission remain separate downstream gates.

The separate default-off [public-free ECB completion candidate](ECB_PUBLIC.md)
uses synthetic admission and a bounded authenticated completion gate.

### Smoke request pacing

`smoke_inventory.py` spaces gateway query-class requests by 1.1 seconds by
default (`--query-interval-seconds`, finite 0–60). OPTIONS and keyed record
reads use their separate gateway classes. This avoids spending more than 60
query requests per minute from this controller; another run can still consume
that actor's quota. Limits are unchanged.

Only an actual HTTP 429 retries the same smoke intent, up to two additional
attempts (`--rate-limit-retries`, 0–5). The owning gateway's integer
`Retry-After` delta-seconds must be present and within
`--max-retry-after-seconds` (default 120, finite 0–300); invalid or excessive
values fail. Other statuses are never retried. Every response, including a
429 before a successful retry, retains status/body/hash/headers in the receipt;
the receipt also records the selected policy. All existing final status,
data, CORS, cache, provenance and serving-identity assertions still apply.
