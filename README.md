# OpenVaultDB Cloud

The Go service in `server/` owns the public gateway for `cloud.openvaultdb.com`
and `ovdb.demodb.dev`. The verified Linux binary embeds the browser assets and
host-specific generated DemoDB catalog; public OVDB traffic enters the existing
read-only handlers directly, without a self-HTTP hop.

This source change is not a deployment receipt. Domain mappings, DNS, edge
header rules, credentials and certificate readiness still require the reviewed
cutover gates in Backstage's `unsigned-shipping-investigation` feature.

## Public boundary

Production startup requires `OVDB_ORIGIN_SECRET` (32–128 ASCII letters, digits,
`_` or `-`). Cloudflare must overwrite `X-OVDB-Origin-Secret` on exactly the two
public hostnames using the matching credential. The Go constant-time validator
rejects missing, wrong or duplicate values before routing and strips the header.
There is no HTTP health exemption; platform startup uses TCP probing.

Both hosts use Google Cloud Run custom domain mappings behind proxied DNS and
Full (strict) TLS. Google documents this mapping facility as preview and not
production ready; the selected design accepts that limitation and potential
certificate issuance delays up to 24 hours. The mapped phase disables the
service's default URL only after mapping/certificate and edge readiness. Direct
mapped-origin requests with branded SNI/Host and spoofed Cloudflare headers must
still be rejected. Natural certificate renewal remains a future observation,
not a claim made by source tests or initial issuance.

Public `/v1`, `/ovdb` and `/.well-known/openvaultdb` routes retain CORS and
provider pin headers. DemoDB's generated database/query allowlist restricts its
API surface and advertises its own hostname. Writes remain forbidden. ECB and
IANA public routes remain unavailable; this port does not activate them.

Abuse limits are independent per Go process/revision: queries 60, reads 300,
preflights 600, device-code creation 10 and device lookups 60 per minute per
IPv4 address or IPv6 /64. The bounded 4096-key map refuses new keys at capacity.
Two replicas have independent buckets; these are not global quotas. Canonical
paths drive both classification and dispatch. Request bodies are bounded to
1 MiB and upstream requests inherit cancellation/deadlines; result streams are
not buffered.

## Existing device and database authority

Only the cloud host exposes OAuth/device/database proxy routes. The Go proxy
uses fixed path rewrites to `SNEAT_API_ORIGIN` and a separate
`OVDB_DEVICE_AUTH_PROXY_SECRET`; it never forwards the edge credential. Sneat's
existing OVDB backend continues to own identity, consent, revocation, Spaces,
grants and storage. This change creates no auth service or account store and
does not move `auth.sneat.co`. Request/response header allowlists and no-store
responses bound that proxy.

Static device pages, scripts, styles and discovery metadata are embedded from
`server/gatewayassets/public/`, with CSP and security headers. DemoDB shares the
static bytes with noindex, but its OAuth/API handlers remain unavailable.

## Build and rollout gates

`npm run check` checks device presentation, retained registry tooling, asset
policy and generated DemoDB drift. Refresh the catalog only from the owning
DemoDB generated manifest:

```sh
node scripts/demodb-ovdb.mjs write ../demo-db/websites/src/data/generated/ovdb.json
node scripts/demodb-ovdb.mjs check ../demo-db/websites/src/data/generated/ovdb.json
```

`Cloud Go CI` tests/builds the Linux binary and archives the complete native
fixture tree including hidden `.ingitdb` files. The manual Go deployment workflow
admits only an exact successful current-main artifact run. Its controller
verifies checksums/provider bytes, creates a no-traffic immutable image revision,
checks image/source identity, then routes exactly that revision. HTTP acceptance
and post-acceptance identity checks are mandatory. A failed candidate stays red.
Rollback requires an independently accepted secure revision and verifies both
restored identity and HTTP behavior; an old unprotected revision is ineligible.

The `protect-origin` stage intentionally allows outage at the old Worker while
origin protection is introduced. The old Worker strips incoming edge credentials
unless its own matching secret binding exists; no rule propagation is assumed.
The `mapped` stage requires both ready Google certificates, final scoped edge
rules/DNS/strict TLS, bypass and public acceptance, then default URL retirement.
The scripts do not create mappings, DNS/rules or secret values automatically.
No provider mutation is implied by a passing offline test.

## Separate registry-search responsibility

`registry-search/` still owns the independent `search.openvaultdb.com` Worker and
its VM-backed registry gateway. Its sources, Wrangler configurations and root
Wrangler development dependency remain. Retiring the two-host OVDB gateway
Worker does not retire that separate service.

See `server/README.md` for native providers, immutable fixture and storage policy.
