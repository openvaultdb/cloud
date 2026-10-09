# Public registry metadata search service

This package is independent of the existing Cloud Worker. Its base gateway configuration is disabled by default. The reviewed live configuration uses the private Typesense VM pilot through `https://vm1.sneat.dev/`; the Cloud deployment path remains available with its exact-host check. The Git registries and their trusted site exporters remain authoritative.

## Private VM development pilot

On the designated Linux VM, Node.js 20+ and Docker are required. From an `openvaultdb/cloud` checkout, run `node registry-search/vm-pilot.mjs install` as root. The installer pulls the pinned Typesense 30.2 image, creates `/opt/datatug/registry-search` with root-only ownership and a durable `owner.json` receipt, and copies itself there before starting the container. The receipt names operator `alex`, this task, and the exact stop and teardown commands. An admin key is generated into a root-only `typesense.env` file; it is never passed as a command argument. The data mount persists under `data/`.

The container uses Docker bridge networking and binds `127.0.0.1:8108`, has a 2 CPU and 3 GiB memory limit with swap disabled, and uses bounded Docker logs. Run `node /opt/datatug/registry-search/vm-pilot.mjs status` to check its Docker configuration and readiness. Run `node /opt/datatug/registry-search/vm-pilot.mjs stop` to stop it, or `node /opt/datatug/registry-search/vm-pilot.mjs teardown` to remove the container. Teardown retains data and credentials for deliberate recovery or disposal. Re-running install accepts only an exact matching container and owner receipt; configuration drift requires investigation instead of silent adoption.

The install, status, stop, teardown and reinstall sequence was executed on the Hetzner `vmai` host on 2026-10-07. The persistent index and search-only credential survived recreation. The current 1,274-document corpus passed exact queries and 60-second loads at 10 and 50 requests/second with zero errors and p95 below 5 ms. These measurements used an in-process gateway and test limiter on the VM; they do not qualify WAN latency, a deployed gateway, growth capacity or concurrent indexing.

For the executed snapshot-and-restore proof, run as root:

```sh
node registry-search/vm-recovery.mjs prove /opt/datatug/registry-search/exports/manifest.json
```

The helper installs itself as `/opt/datatug/registry-search/recover.mjs`, records ownership before starting resources, requests a supported Typesense snapshot, encrypts the archive, and restores it into an isolated loopback container on port 8109. It checks the live alias, count and manifest smoke queries, then removes its owned temporary container and directories. Retained encrypted archives and private passfiles are under `backups/`; copy both off-host into private storage without logging their contents. The 2026-10-07 archive was copied into the operator's private `.wb/private/registry-search-vm-pilot/` directory and its digest verified. If interrupted, run `node /opt/datatug/registry-search/recover.mjs cleanup`; cleanup requires matching ownership receipts. The primary data and credentials are retained.

The VM pilot alone does not configure DNS, TLS, a firewall rule, a public gateway route, automatic publication or scheduled backup. The reviewed live route and publisher are described below. Full staging and Typesense Cloud capacity qualification remain DataTug launch gates; the live site search has a separate 10,000 expanded-document VM ceiling. Detailed inventory and measured receipts live in the backstage `registry-metadata-search` plan.

`registry-search-export/v1` inputs are pinned in a manifest. Each `sources[]` entry has `domain`, `repository`, `revision` (40 lowercase hex), `sha256` (of the exact JSON bytes), and an HTTPS `url` ending in `/registry-search.json`. All three domains are required. `approved_revisions` pins every additional object repository allowed in provenance. `sequence` is a monotonically increasing publication number. `smoke_queries` contains at least one `{domain,q,id}` for each domain. Source exports contain `format`, `domain`, `source_revisions`, `fixture`, and `documents`. Shared source pins must agree across all exports and with the manifest. The three canonical site hosts and route prefixes are fixed in code.

Prepare JSONL without an engine:

```sh
node registry-search/cli.mjs prepare /path/to/manifest.json /path/to/output.jsonl
```

For explicit local fixture proof, set `REGISTRY_SEARCH_ALLOW_FIXTURES=1` and use `file` entries in the manifest. For a local Typesense service bound by a named owner to loopback, the importer accepts loopback HTTP only with `REGISTRY_SEARCH_MODE=local` and the same fixture flag. No service is started by this package.

Publication requires `REGISTRY_SEARCH_MODE=staging|production` (or `local` for explicit fixture proof), `REGISTRY_TYPESENSE_ORIGIN`, `REGISTRY_TYPESENSE_ADMIN_KEY`, and `REGISTRY_SEARCH_STATE_DIR`. Production requires `REGISTRY_TYPESENSE_DEPLOYMENT=vm-pilot` with the exact HTTPS hostname `vm1.sneat.dev`, or `REGISTRY_TYPESENSE_DEPLOYMENT=cloud` with a matching `REGISTRY_TYPESENSE_CLOUD_HOST`, and Python 3 with `fcntl` on macOS/Linux. Keep the admin key in the process environment, never command arguments or a tracked file. The state directory must be durable and owned by one publisher. The CLI acquires a kernel advisory lock and replaces the lock helper with the Node publisher in the same process, retaining an inherited lock descriptor for its full lifetime. The lock releases automatically if the publisher dies; its file remains to avoid an unlink/recreate race. The sequence fence is persisted before the alias switch. Do not run a second publisher on another host against the same alias; direct library callers must hold the same lock.

For VM pilot production, run the CLI only on that VM. It checks the configured public HTTPS origin, then connects its admin client to the fixed `127.0.0.1:8108` loopback address. The public Caddy route exposes only search and cannot carry admin publication. The scheduled refresh below uses the same private path.

```sh
node registry-search/cli.mjs publish /path/to/manifest.json
```

The importer validates the complete corpus, creates `registry_metadata_<generation>`, checks every JSONL response row, count, and a smoke query for each domain, then updates `registry_metadata`. It retains the current and immediately previous generations for rollback and prunes only its own inactive generation names after a successful switch. It may delete and recreate an abandoned inactive candidate on retry; it never writes into the live collection. A failed import cannot switch the alias. Retrying the same active content is idempotent.

The gateway accepts `POST /v1/registry-search` with `{q,domain,kind?,parent_id?,page?}` and returns `{hits,found,page,generation}`. Errors have an `error` code and non-2xx status. It requires a rate-limit binding and a search-only Typesense key. The base `registry-search/wrangler.jsonc` remains disabled; `registry-search/wrangler.live.jsonc` is the production custom-domain configuration for `search.openvaultdb.com`, with 60 requests per minute per domain and connecting IP. The three production site origins are fixed; staging origins require an explicit setting. Set `REGISTRY_TYPESENSE_SEARCH_KEY` through Wrangler's secret input, never in either configuration or a shell argument. `npm run deploy:registry-search:live:dry` validates the bundle and `npm run deploy:registry-search:live` deploys it. The gateway accepts only the exact HTTPS VM host in VM mode and sends a single bounded `/multi_search` request. Cloud mode still requires the configured Cloud hostname to match exactly.

## Live VM origin and automatic refresh

The existing `vm1.sneat.dev` Caddy site has other routes and a fallback. On that VM, `node registry-search/vm-caddy-route.mjs install` as root inserts the marked `vm1-search.caddy` snippet only at the exact existing `vm1.sneat.dev {` / `route {` anchor. It saves a root-only baseline and ownership receipt before changing the Caddyfile, validates the baseline and candidate, atomically installs the candidate and reloads Caddy; a reload failure restores the baseline. `node /opt/datatug/registry-search/caddy-live/vm-caddy-route.mjs status|teardown` checks for drift or removes only that owned block. Owner is `alex`, task `01a114c1-e447-7a81-a869-0d82ca2a7747`; unrelated host routes are preserved. The matcher accepts only `POST /multi_search` with no URL query and a 64 KB body; `log_skip` keeps requests to this path, including rejected query strings, out of Caddy access logs. Every other Typesense API path stays behind loopback. The engine itself requires the search-only API key. Verify direct `GET /collections` and `POST /multi_search?x=1` through the HTTPS host fail before enabling the gateway.

On the VM, `node registry-search/vm-refresh-service.mjs install` as root installs a root-owned one-shot systemd service and a five-minute timer. It verifies the existing pilot's owner receipt, private admin-key file and `/opt/datatug/registry-search/state/publication.json` before copying its source, recording hashes and ownership, and enabling the timer. Its owner is `alex`, task `01a114c1-e447-7a81-a869-0d82ca2a7747`. The service uses the root-only `typesense.env` from the pilot and calls the engine only at `127.0.0.1:8108`; it never sends the admin key through Caddy. `node /opt/datatug/registry-search/live/vm-refresh-service.mjs status|stop|teardown` inspects, disables or removes only its owned service and timer. Teardown retains the private engine, data, credentials and publication state.

Each run fetches the three fixed public `/registry-search.json` URLs over HTTPS with redirects refused, a 30-second timeout per source, and a 32 MB limit per export. It rejects fixtures, conflicting pins, invalid routes and more than 10,000 expanded documents. Fresh smoke queries are chosen from the current exports. The publisher uses the existing single-host advisory lock and durable sequence; a failed fetch, validation, import or smoke test leaves the active alias intact. The one-shot service has a 240-second start timeout, 768 MB memory and one-CPU limit. Check `systemctl list-timers registry-search-refresh.timer`, `systemctl show registry-search-refresh.service -p TimeoutStartUSec`, and `journalctl -u registry-search-refresh.service` for the last successful generation before calling the corpus current.

`npm test` runs the existing Vitest suite and the dedicated Node tests for validation, import failures, sequence fencing, query policy, and unavailable errors. `node registry-search/benchmark.mjs` measures synthetic 10k/50k/100k source-record expansion and JSONL bytes. One source record means a graph concept, model descriptor or collection, or registered database; child fields/entities/collections and grouped servers are derived documents. The fixture includes repeated `CustomerId_N` field labels under different parents, component-like effective fields, a sparse ModelSpec tail of 100-field descriptors, and a heavy database tail. `node registry-search/benchmark.mjs --profile 10000 --out /private/tmp/registry-10k` emits three explicit fixture exports and a pinned manifest for local Typesense import; repeat with 50000 and 100000 in separate processes. These figures are not engine disk/RAM, query latency, relevance, or live capacity measurements. The synthetic mix is an experiment shape, not a forecast or capacity qualification; run the plan's real Typesense and browser gates separately.

## Document kinds and the `model_record` transition

The service accepts these document kinds and refuses a document or request with any other: `meaning_entity`, `meaning_field`, `model`, `model_entity`, `model_record`, `model_collection`, `model_field`, `ovdb_server`, `ovdb_database`, `ovdb_collection`. The list lives once, in `provenance.mjs`, and the validator, merger, gateway and VM refresh service all read it.

ModelSpec renamed its `entity` construct to `record`, so a ModelSpec record type is now a `model_record` document; `model_entity` is the earlier name. Both are accepted and treated identically: validation, `kind_priority` 2, `field_count` and `field_preview`, and the gateway's check of a stored priority. `model_collection` also stays accepted; nothing is removed.

- A request that filters on `model_entity` or on `model_record` (or on both) returns documents of both kinds, because during the transition one site may export `model_record` while another still exports `model_entity`. Every hit carries the kind its document was stored with; stored kinds are never rewritten. Other kind filters are unchanged.
- The document id hashes the kind (`stableId`), so a document that changes kind gets a new id, and the fields under it a new `parent_id`. The next refresh after a site switches therefore publishes a new generation; the five-minute timer does this by itself and no manual reindex step exists.
- With a corpus that holds only `model_entity` documents the merged output is byte for byte what it was before this change (same generation hash), so deploying either part changes nothing a user can see.

### Deploy order for this change

Two parts are deployed by hand and may briefly run different versions: the Worker (`gateway.mjs` with `provenance.mjs`) and the VM refresh service (`refresh.mjs`, `merge.mjs`, `schema.mjs`, `provenance.mjs`, `publish.mjs`). Behaviour of each pairing, from the code:

| | corpus has only `model_entity` | corpus has `model_record` documents (a site switched) |
|---|---|---|
| New Worker, old VM service | works; the index is unchanged | the old refresh refuses the whole export set (`invalid public scope`), the active generation stays, the corpus goes stale; the gateway never sees a `model_record` hit |
| Old Worker, new VM service | works; the new refresh produces the identical generation, so nothing is republished | **unsafe**: the first refresh indexes `model_record` documents and the old gateway answers 503 (`unavailable`) to every query that returns one, and 400 to a request that filters on `model_record` |
| Both new | works | works |

So the safe order is: **Worker first, then the VM refresh service, then the sites**. Do not switch any site to export `model_record` until both parts are deployed; a site that switches earlier makes the refresh fail for all three domains (the alias is kept, the data goes stale), not only its own. The VM service must not be deployed before the Worker once any site exports `model_record`.

These steps were **not executed by the author of this change**; they are the existing commands of this package, in the order above, for the owner or operator.

1. Worker, from a checkout containing this change: `npm run deploy:registry-search:live:dry`, then `npm run deploy:registry-search:live`.
2. VM refresh service, on the VM as root, from a checkout containing this change. An installed version is not replaced in place (`installed version differs; stop and teardown before installing a new version`), so first `node /opt/datatug/registry-search/live/vm-refresh-service.mjs teardown`, then `node registry-search/vm-refresh-service.mjs install`, then `node /opt/datatug/registry-search/live/vm-refresh-service.mjs status`. Teardown retains the engine, data, credentials and publication state; no refresh runs between teardown and install, and the active generation keeps serving.
3. Only then may a site export `model_record`. After that, check `journalctl -u registry-search-refresh.service` for the new generation.

Rollback of either part to the previous version is safe while no site exports `model_record`.
