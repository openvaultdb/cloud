# OpenVaultDB Cloud

The public Chinook OVDB service is implemented in [`server/`](server/). Its Go
handler serves generic OVDB pages and a read-only SQLite API on Cloud Run;
this Worker proxies the public paths after `CHINOOK_RUN_ORIGIN` is configured.
See the server README for local acceptance and the required Go library release
(openvaultdb-go v0.25.5; its native PostgreSQL mounts require verified TLS for
remote hosts, and the optional DemoDB PostgreSQL editions additionally require
`sslmode=verify-full`, as described under
[Mount hardening and operator requirements](server/README.md#mount-hardening-and-operator-requirements)).
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

The Worker contains no authorization business state. It publishes the stable
OAuth paths used by the CLI and proxies them to the OVDB backend (the Sneat Go
Cloud Run service `sneat-app`, called directly at its `run.app` origin,
`SNEAT_API_ORIGIN`) with a dedicated shared secret. The backend rejects direct
calls that do not carry that secret, preventing callers from bypassing the edge
limits.

## Static assets and `run_worker_first`

Everything under [`public/`](public/) is served as a static asset without
running the Worker: the `/device/` and `/devices/` pages, their scripts and
styles, the `404.html` page and the RFC 8414 discovery metadata at
`/.well-known/oauth-authorization-server` (a static JSON file naming
`https://cloud.openvaultdb.com`). Their headers live in
[`public/_headers`](public/_headers) (CSP, `X-Frame-Options: DENY`,
`Referrer-Policy`, `nosniff`; the metadata's JSON type and `no-store`) and the
`/` → `/device/` 302 in [`public/_redirects`](public/_redirects); `/device` and
`/devices` get the asset layer's trailing-slash 307. `wrangler.jsonc` lists in
`assets.run_worker_first` only the paths that need code: `/v1`, `/ovdb`,
`/.well-known/openvaultdb` (rate limits and Cloud Run forwarding), `/oauth/*`
and `/api/*` (device flow, secret injection), and the ECB/IANA query routes.
When a path is added to the Worker, add it there too
(`scripts/static-routing.test.mjs` checks the list), or it will never run.

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
them, separate from the OAuth/device bucket (`src/ovdb-limits.ts`,
`wrangler.jsonc`).

> **Not live until the Worker deploys.** The limits below are the intended
> behaviour of this code. They apply only once the Worker deploys successfully
> and `OVDB_ORIGIN_SECRET` is bound; until then the public OVDB routes are not
> served through this Worker's limiter and the numbers below are not in force.
> Check the latest Worker deploy run before relying on them.

| Binding | Routes | Limit per client |
| --- | --- | --- |
| `OVDB_QUERY_LIMITER` (namespace 17033) | `/v1/dtql`, `/v1/databases/{db}/dtql`, `/query`, `/batch`, `/read` | 60 requests / 60 s |
| `OVDB_READ_LIMITER` (namespace 17034) | every other forwarded OVDB route | 300 requests / 60 s |
| `OVDB_PREFLIGHT_LIMITER` (namespace 17035) | `OPTIONS` preflights to any of them | 600 requests / 60 s |

The service runs at most two instances of two concurrent requests with a 10 s
query timeout, so 60 queries a minute (1/s sustained) from one address is
already a large share; DataTug issues one or two queries per view. A client is
an IPv4 address, or an IPv6 /64 prefix (one subscriber usually holds a whole
/64). A limited request gets `429 {"error":{"code":"rate_limited",…}}` with
`Retry-After: 60`, `Cache-Control: no-store`, `Vary: Origin`, and
`Access-Control-Allow-Origin` for the DemoDB/DataTug/Chinook origins the service
itself allows, so browser clients can read it. A missing binding fails open
(the previous behaviour) and logs one `console.warn` per isolate. Cloudflare's
limiter counts per location and is eventually consistent: these are abuse
ceilings, not quotas.

The path is canonicalised once and that one spelling is both classified and
forwarded, because Go decodes percent-escapes before routing (`d%74ql` is
`dtql`). Escapes of unreserved characters are decoded and other escapes keep
upper-case hex; a path under `/v1`, `/ovdb` or `/.well-known` with a malformed
escape, an encoded slash or backslash (`%2F`, `%5C`), an empty segment (`//`),
a `.`/`..` segment or a `;` parameter is refused with `400 invalid_path`.

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

## DemoDB OVDB API: `ovdb.demodb.dev`

`https://ovdb.demodb.dev/v1` is a Custom Domain of this Worker
([`src/demodb.ts`](src/demodb.ts)). It replaces `https://demodb.dev/ovdb/v1`,
which used to cost two Worker invocations per call (`demodb-websites` re-fetched
`cloud.openvaultdb.com`); demodb.dev now redirects there with a 308.

- `GET /v1`: the DemoDB server index; `GET /v1/databases/{id}`: the database
  index with `endpoints.dtql` pointing at this host;
  `/v1/databases/{id}/{dtql,query,read,inferred-schema,records/...}`: forwarded
  to Cloud Run through the same per-IP limits and origin secret as
  `cloud.openvaultdb.com`, keyed on the real client address.
- Only the DemoDB databases in [`src/demodb-ovdb.json`](src/demodb-ovdb.json)
  are served; any other id, and every other Worker path, is `404`. None of the
  `cloud.openvaultdb.com` code routes (OAuth, devices, ECB, IANA)
  exist on this host. The static asset bundle is shared, though: `/device/`,
  `/devices/` and their files are served here as on `cloud.openvaultdb.com`
  (with `X-Robots-Tag: noindex`), because `run_worker_first` matches paths, not
  hosts. They cannot do anything on this host: their `/api/*` calls 404.
- Read-only: CORS `*`, `204` preflights allowing `OVDB-Page-Size`,
  `OVDB-Page-Token`, `OVDB-Page-Close`; writes `403` with `Allow`, other methods
  `405`, query bodies over 1 MiB `413`.

`src/demodb-ovdb.json` is a build-time copy of demo-db/websites
`src/data/generated/ovdb.json` (after its `npm run prepare:databases`). When the
DemoDB database set changes, refresh and check it:

```sh
node scripts/demodb-ovdb.mjs write ../demo-db/websites/src/data/generated/ovdb.json
node scripts/demodb-ovdb.mjs check ../demo-db/websites/src/data/generated/ovdb.json
```

`test/demodb.test.ts` replays every path, method and backend outcome through
both the former chain (a verbatim copy of the demodb-websites handlers in
`test/fixtures/`) and this host, and requires identical responses and Cloud Run
requests apart from the advertised host.

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
