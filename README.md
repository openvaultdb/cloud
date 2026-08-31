# OpenVaultDB Cloud

This repository contains the public OpenVaultDB Cloud control plane and browser
authorization service deployed at `https://cloud.openvaultdb.com`.

The first implemented surface is OAuth 2.0 Device Authorization Grant support
for first-party command-line clients:

- `POST /oauth/device/code` starts a short-lived authorization;
- `/device` authenticates the user with Firebase and asks for explicit consent;
- `POST /oauth/token` performs the polling exchange exactly once;
- `GET /oauth/userinfo` verifies the stored bearer token and reports its grant;
- `POST /oauth/revoke` revokes a credential immediately.

Pending authorizations and revocable access-token digests live in Cloudflare
D1. Raw device codes and access tokens are never stored. Access tokens default
to one year and every API use checks current revocation state. Cloudflare rate
limit bindings protect public code creation, lookup, and polling endpoints.
An isolated daily job expires pending grants and removes authorization/token
state after a bounded retention window.

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
