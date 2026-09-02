# OpenVaultDB Cloud

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

## Disabled Listus demo relay

The Worker includes a disabled-by-default relay for the bounded Listus local
demo. It is enabled only when `OVDB_DEMO_ENABLED=true` and all of these private
bindings are present: KV binding `OVDB_DEMO_SESSIONS`, secrets
`OVDB_DEMO_CONTROL_SECRET` and `OVDB_DEMO_ENCRYPTION_KEY`, and variables
`OVDB_DEMO_FIREBASE_PROJECT_ID`, `OVDB_DEMO_CORS_ORIGIN`, and
`OVDB_DEMO_ORIGIN_HOST_SUFFIX`. The suffix restricts session origins to the
account-owned relay domain and prevents the trusted control plane from turning
the Worker into an arbitrary fetch proxy. Missing
configuration fails closed. `OVDB_DEMO_CONTROL_SECRET` is only for backend to
edge `/internal/demo/sessions/{sessionId}` calls; it is never forwarded to a
tunnel origin. `OVDB_DEMO_ENCRYPTION_KEY` is a base64url-encoded 32-byte AES-GCM
key used to encrypt the per-session database credential before writing KV.

When disabling the demo, keep the KV binding and control secret until cleanup
finishes. Authenticated internal `DELETE` calls on `cloud.openvaultdb.com`
remain available while admission and data access are disabled; they need no
encryption key or Firebase configuration and never contact a tunnel origin.

Before enabling a session's exact origin hostname, configure that hostname as
an exact Worker route (not a wildcard) whose normal origin is the managed
tunnel. Its only accepted form is
`https://ovdb-demo-{sessionId}.openvaultdb.com/`, with a DNS-safe lowercase
session ID. Verify the account's real routing semantics with an unauthenticated
direct-host request: it must be denied by this Worker without an origin request.
Also test the bare tunnel target separately. Do not enable the feature until
both checks are recorded; the local Worker test intentionally does not claim a
production tunnel receipt.
