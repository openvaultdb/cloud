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
The accepted secure rollback receipt must bind `originIdentity` (project, region,
service and stable defaultUrl) to `OVDB_DEFAULT_ORIGIN_URL`. That retained
non-secret identity is probed even when default-URL disabling removes status.url;
the controller never re-enables the endpoint. TCP startup is pinned to8080;
inherited liveness/readiness probes are refused for owner review, and both
candidate and rollback revisions must satisfy that same health contract.
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

## First-bootstrap controller contract — future provider execution

The reviewed draft bootstrap is separate from ordinary `accepted-rollback`
mode. `first-bootstrap` exposes explicit `capture`, `quiesce`, `protect`,
`open-for-mapping`, `status` and `seal-after-acceptance` actions. This describes
source interfaces, not an executed cutover/runbook. Mapping, DNS, certificates,
edge-rule mutations and the real authenticated journey remain root-owned.

Its owned local `ovdb-bootstrap-state.json` records mode, actual UID/generation/
resourceVersion, config/evidence digests and all observed direct/alias/tag URLs.
Atomic0600 replacement and directory fsync precede each mutation; a local lock
serializes the receipt. The root must durably retain this same state across jobs
and supply it on resume. A missing state never authorizes resume by stage label;
current provider identity/config/traffic must agree. Pending bootstrap blocks
ordinary-mode fallback. Protection-only state never supplies `routesAccepted` or
an accepted functional rollback receipt.

Capture requires `--bootstrap-ingress-inventory`: a fresh root-owned
`ovdb-bootstrap-ingress-inventory/1` document bound to the exact target/fence,
`complete:true`, full coverage of project/region mappings, service direct aliases/
tags and bounded service public ingress, with attributable complete-source result
SHA256 provenance. Its mappingResources/otherPublicIngress must be empty and all
actual Google origins retained. CLI listing failures/incomplete inventory cannot
stand for absence. This is not fleet discovery or an inference that no LB exists.

Bootstrap writes only the existing openvaultdb/europe-west1/chinook-ovdb service.
It sends the captured resourceVersion unchanged in the documented regional v1
replaceService PUT; no gcloud-replace refresh may substitute a newer version.
The same UID/name/namespace and full unrelated spec/metadata are preserved.
Existing Google OAuth credentials are captured only in subprocess memory and the
fixed Google HTTPS Authorization header. They never enter argv/files/receipts or
HTTP error bodies; origin/proxy Secret Manager values are never fetched.409 or
UID/version drift is unresolved and never triggers blind retry/replacement by
name. A lost write response also remains unresolved; only a durably recorded
response generation can authorize reconciliation on resume. Root remediation is
required when safe closure cannot be attributed. An acknowledged exact-generation
mutation with unknown readiness retains its in-progress checkpoint while closure
is observed; only later actual Ready reconciliation permits advancement. Actual
failed readiness, version/config drift and missing response attribution remain
failures.

Quiescence disables default and removes all tags before any protected image
change. Protection keeps default disabled, pins digest/source/credential references
and TCP8080, verifies named ready revision/config, then routes that name100%.
Only verified protection permits explicit default re-enable; real missing/wrong/
duplicate credential probes must reject known data/auth/static paths with valid
TLS. Active default/alias URLs must return403; removed tag URLs may return403
or404. Network/TLS errors, unrelated404 or5xx are not closure evidence. Every later
failure re-closes with the expected-version fence, or records an unresolved
incident; captured naked traffic is never restored. A safely reclosed protected
mapping-stage failure retains its candidate checkpoint. Acceptance can retry
while default remains disabled, but must re-read current mappings/protection and
fresh external evidence and rerun live probes; no mapping recreation or naked
reopening is authorized.

`status` stops pending external mapping-resource/DNS/certificate/real both-host
data/auth gates. `seal-after-acceptance` additionally requires a fresh root-owned
`ovdb-functional-mapped-acceptance/1` proof: exact service UID/candidate identity,
public hosts, retained originIdentity, genuine origin/routes/auth acceptance and
complete ingress inventory, with separate DNS/origin-certificate/edge-rule/
data-pin-streaming/authenticated-device-database evidence SHA256s and provenance.
It re-reads both mapping readiness generations, repeats current public/origin/data
probes with all eight native providers, then disables default and verifies closure
again. The state records only the external proof digest; it never fabricates the
first ordinary-mode functional proof. Natural certificate renewal remains pending
until observed, independently of this source and initial acceptance.
