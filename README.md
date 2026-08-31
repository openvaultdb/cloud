# OpenVaultDB Cloud

This repository contains the public OpenVaultDB Cloud control plane and browser
authorization service deployed at `https://cloud.openvaultdb.com`.

The first implemented surface is OAuth 2.0 Device Authorization Grant support
for first-party command-line clients:

- `POST /oauth/device/code` starts a short-lived authorization;
- `/device` authenticates the user with the shared Sneat Co. Firebase identity
  and asks for explicit consent;
- `POST /oauth/token` performs the polling exchange exactly once;
- `GET /oauth/userinfo` verifies the stored bearer token and reports its grant;
- `POST /oauth/revoke` revokes a credential immediately.

Pending authorizations and revocable access-token digests live in Cloudflare
D1. Raw device codes and access tokens are never stored. Access tokens default
to one year and every API use checks current revocation state. Cloudflare rate
limit bindings protect public code creation, lookup, and polling endpoints.
An isolated daily job expires pending grants and removes authorization/token
state after a bounded retention window.

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

Apply D1 migrations locally before manual browser testing:

```sh
npx wrangler d1 migrations apply openvaultdb-cloud --local
```

The Firebase browser configuration is public by design. Firebase authorization
and D1 data access are enforced by the Worker, not by hiding browser config.
