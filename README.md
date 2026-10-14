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

Run the complete-seed Worker/D1 tests by setting `NORTHWIND_D1_SEED_SQL` to the
`artifacts/hosting-imports/d1.sql` file from pinned Northwind source commit
`64961074fc09801cf489ba0d3632d14c463950e3`. The tests reject any import whose
SHA-256 differs from the pinned artifact. For a real Neon HTTP transport test,
set `NORTHWIND_NEON_CONNECTION_FILE` to a file containing the private readonly
connection URI; the test setup passes it to the test runtime binding without
embedding it in generated test code. Never put the URI in a command argument,
repository file, test output, or log.
