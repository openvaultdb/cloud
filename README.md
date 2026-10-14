# OpenVaultDB Cloud

The public Chinook OVDB service is implemented in [`server/`](server/). Its Go
handler serves generic OVDB pages and a read-only SQLite API on Cloud Run;
this Worker proxies the public paths after `CHINOOK_RUN_ORIGIN` is configured.
See the server README for local acceptance and the required Go library release.
The older device and account routes documented below still use Sneat-backed
infrastructure; their independent identity cutover is a separate stage.

This repository contains the public OpenVaultDB Cloud browser surface and API
facade deployed at `https://cloud.openvaultdb.com`.

The first implemented surface is OAuth 2.0 Device Authorization Grant support
for first-party command-line clients:

- `POST /oauth/device/code` starts a short-lived authorization;
- `/device` authenticates the user with the shared Sneat Co. Firebase identity
  and asks for explicit consent;
- `POST /oauth/token` performs the polling exchange exactly once;
- `GET /oauth/userinfo` verifies the stored bearer token and reports its grant;
- `POST /oauth/revoke` revokes a credential immediately.

Pending authorizations and revocable access-token digests live in the existing
Sneat Co. Firestore database and are owned by `github.com/sneat-co/ovdb/backend`.
Raw device codes and access tokens are never stored. HMAC-derived lookup IDs
make the short user code non-enumerable without the backend pepper. Access
tokens default to one year and every API use checks current revocation state.
Cloudflare rate-limit bindings protect public code creation, lookup, and
polling endpoints.

The Worker contains no authorization business state. It serves `/device`,
publishes the stable OAuth paths used by the CLI, and proxies them to the OVDB
backend with a dedicated shared secret. The backend rejects direct calls that
do not carry that secret, preventing callers from bypassing the edge limits.

OpenVaultDB Cloud uses the verified `sneat-eur3-1` Firebase UID directly as the
Sneat Co. `userID`; it does not create a second OpenVaultDB account identifier.
The issued credential is nevertheless an OpenVaultDB token bound to one
registered client and its approved scopes. It is not a Sneat Co. API token.

Sneat Co. Spaces are collaborative grant subjects independently of the product
surface that presents them. A Space may be used through `sneat.work`,
`sneat.app`, or an extension mini-app. Space membership and resource grants are
evaluated by the OpenVaultDB control plane and are not embedded into the
long-lived device token.

## Public OVDB routes: rate limits and the Cloud Run origin

The Worker forwards `/v1/*`, `/ovdb/*` and `/.well-known/openvaultdb` to the
Cloud Run service in [`server/`](server/). Two per-IP rate-limit bindings guard
them, separate from the Northwind/OAuth bucket (`src/ovdb-limits.ts`,
`wrangler.jsonc`):

| Binding | Routes | Limit per IP |
| --- | --- | --- |
| `OVDB_QUERY_LIMITER` (namespace 17033) | `/v1/dtql`, `/v1/databases/{db}/dtql`, `/query`, `/batch`, `/read` | 60 requests / 60 s |
| `OVDB_READ_LIMITER` (namespace 17034) | every other forwarded OVDB route | 300 requests / 60 s |

The service runs at most two instances of two concurrent requests with a 10 s
query timeout, so 60 queries a minute (1/s sustained) from one address is
already a large share; DataTug issues one or two queries per view. Preflight
`OPTIONS` is never counted. A limited request gets
`429 {"error":{"code":"rate_limited",…}}` with `Retry-After: 60`,
`Cache-Control: no-store`, `Vary: Origin`, and `Access-Control-Allow-Origin`
for the DemoDB/DataTug/Chinook origins the service itself allows, so browser
clients can read it. A missing binding fails open (the previous behaviour).
Cloudflare's limiter counts per location and is eventually consistent: these
are abuse ceilings, not quotas.

Cloud Run is deployed `--allow-unauthenticated`, so its `*.run.app` URL would
bypass these limits. To close that, bind the same random value (32-128
characters of `A-Za-z0-9_-`) in two places:

1. Worker secret `OVDB_ORIGIN_SECRET` (`wrangler secret put OVDB_ORIGIN_SECRET`);
   optionally the var `OVDB_ORIGIN_SECRET_HEADER` (default `X-OVDB-Origin-Secret`).
   The Worker then adds that header to every request it forwards to Cloud Run,
   including the ECB/IANA operator routes and the public ECB route.
2. Cloud Run env `OVDB_ORIGIN_SECRET` from Secret Manager (and the same
   `OVDB_ORIGIN_SECRET_HEADER` if changed). The Go server then answers `403` to
   any request without the exact header, and strips it before the library sees it.

Deploy the Worker secret first, then the Cloud Run secret; while only the Worker
has it, Cloud Run ignores the header. When neither is set nothing changes. Once
Cloud Run enforces it, the opt-in `OVDB_CLOUD_RUN_SMOKE_ENABLED` smoke step in
`.github/workflows/deploy-chinook-cloudrun.yml`, which calls `*.run.app`
directly, must send the header too (or be pointed at the Worker). An invalid
value refuses to start the Go server and makes the Worker answer 503 rather than
forward without it. Nothing in this repository sets either secret.

## Local development

```sh
npm install
npm test
npm run check
npm run dev
```

The Firebase browser configuration is public by design. Firebase authorization
is verified by the Sneat Co. backend. Put `OVDB_DEVICE_AUTH_PROXY_SECRET` in a
local `.dev.vars` file for manual Worker development; never commit it.

`@dalgo/core` is pinned to immutable source commit
`b863f32d2e92a0784e9633aa0eaa14d2d5528336`. The source archive does not contain
compiled output, so `npm install` builds its `dist/` files with the repository's
locked TypeScript compiler. `npm run check` and deployment also build this pin
before importing the D1 adapter.

## Northwind read-only mirrors

The Worker exposes the pinned Northwind seed through two optional, read-only
routes. The existing Chinook routes remain unchanged.

- `GET /northwind/d1/v1/metadata` and `POST /northwind/d1/v1/query` use the
  optional `NORTHWIND_D1` binding.
- `GET /northwind/neon/v1/metadata` and `POST /northwind/neon/v1/query` use the
  optional `NORTHWIND_NEON_DATABASE_URL` secret and the dedicated
  `northwind_reader` PostgreSQL role.

Both mirrors serve the database identified by canonical ID
`https://demodb.dev/northwind/`, with schema version `1` and seed version
`279b34136771aee75d802094b3329515a2b01da65d2a20a4a9e3b58c29b4fd20`. They
share the versioned query DTO, allowlisted collections and columns, paging,
composite primary keys, and BLOB wire format used by the `@dalgo/d1` client.
The worker allows browser reads from `https://northwind.demodb.dev`,
`https://demodb.dev`, and `https://www.demodb.dev`; requests may send
`X-Dalgo-Schema-Version` and `X-Dalgo-Seed-Version` to require a matching
deployment. Unsupported collections and fields are rejected, and SQL values
are always bound parameters.

Both routes apply one request policy before touching a database: a filter value
that cannot be compared with its column (for example `"abc"` or `1.5` on an
integer column) is `400 invalid_request`, never an empty page; an unknown
collection is `422 unsupported_query`; `offset` is capped at 10000 (`400`
beyond that); a failed query is a generic `500 query_failed`; `503` means the
endpoint is not configured. Every Northwind response carries
`Cache-Control: no-store` and `X-Content-Type-Options: nosniff`, and every
non-preflight request passes the same Cloudflare rate limiter as the other
public lookup routes (`429`). On Neon each query runs in a `READ ONLY`
transaction with a 10 second server-side `statement_timeout`, and text
collation (`COLLATE "C"`) is chosen from the real PostgreSQL column types
(`NORTHWIND_PG_TYPES`), never from the SQLite types.

PostgreSQL `NUMERIC` and `BIGINT` values that the Neon HTTP driver represents as
decimal strings stay strings so their precision is preserved. D1 uses native
SQLite runtime types. Numeric aggregates can differ at floating-point
round-off, so the mirrors do not promise identical JSON scalar types or
bit-identical aggregate results across engines.

The production deployment workflow attaches D1 only when the
`NORTHWIND_D1_DATABASE_ID` Actions variable contains a valid database UUID.
When present, `NORTHWIND_NEON_DATABASE_URL` is transferred directly from the
production Actions secret to Wrangler's secret input; it is never written to
the repository or emitted to logs. The secret must target database `northwind`
on a Neon host as user `northwind_reader`. If either runtime configuration is
absent, that endpoint returns a generic `503` response.

`npm test` also runs `test-node/northwind-postgres.test.ts`, which loads the
pinned `artifacts/hosting-imports/postgres.sql` (set `NORTHWIND_POSTGRES_SEED_SQL`;
required when `CI` is set, SHA-256 checked) into PGlite and runs every one of the
30 collections through the Neon route with real PostgreSQL semantics.

Run the complete-seed Worker/D1 tests by setting `NORTHWIND_D1_SEED_SQL` to the
`artifacts/hosting-imports/d1.sql` file from pinned Northwind source commit
`64961074fc09801cf489ba0d3632d14c463950e3`. The tests reject any import whose
SHA-256 differs from the pinned artifact. For a real Neon HTTP transport test,
set `NORTHWIND_NEON_CONNECTION_FILE` to a file containing the private readonly
connection URI; the test setup passes it to the test runtime binding without
embedding it in generated test code. Never put the URI in a command argument,
repository file, test output, or log.
