# Public registry metadata search service

This package is independent of the existing Cloud Worker. Its dedicated gateway is disabled by default. Production requires a reviewed Typesense Cloud hostname and deployment configuration. The Git registries and their trusted site exporters remain authoritative.

## Private VM development pilot

On the designated Linux VM, Node.js 20+ and Docker are required. From an `openvaultdb/cloud` checkout, run `node registry-search/vm-pilot.mjs install` as root. The installer pulls the pinned Typesense 30.2 image, creates `/opt/datatug/registry-search` with root-only ownership and a durable `owner.json` receipt, and copies itself there before starting the container. The receipt names operator `alex`, this task, and the exact stop and teardown commands. An admin key is generated into a root-only `typesense.env` file; it is never passed as a command argument. The data mount persists under `data/`.

The container uses Docker bridge networking and binds `127.0.0.1:8108`, has a 2 CPU and 3 GiB memory limit with swap disabled, and uses bounded Docker logs. Run `node /opt/datatug/registry-search/vm-pilot.mjs status` to check its Docker configuration and readiness. Run `node /opt/datatug/registry-search/vm-pilot.mjs stop` to stop it, or `node /opt/datatug/registry-search/vm-pilot.mjs teardown` to remove the container. Teardown retains data and credentials for deliberate recovery or disposal. Re-running install accepts only an exact matching container and owner receipt; configuration drift requires investigation instead of silent adoption.

The install, status, stop, teardown and reinstall sequence was executed on the Hetzner `vmai` host on 2026-10-07. The persistent index and search-only credential survived recreation. The current 1,274-document corpus passed exact queries and 60-second loads at 10 and 50 requests/second with zero errors and p95 below 5 ms. These measurements used an in-process gateway and test limiter on the VM; they do not qualify WAN latency, a deployed gateway, growth capacity or concurrent indexing.

For the executed snapshot-and-restore proof, run as root:

```sh
node registry-search/vm-recovery.mjs prove /opt/datatug/registry-search/exports/manifest.json
```

The helper installs itself as `/opt/datatug/registry-search/recover.mjs`, records ownership before starting resources, requests a supported Typesense snapshot, encrypts the archive, and restores it into an isolated loopback container on port 8109. It checks the live alias, count and manifest smoke queries, then removes its owned temporary container and directories. Retained encrypted archives and private passfiles are under `backups/`; copy both off-host into private storage without logging their contents. The 2026-10-07 archive was copied into the operator's private `.wb/private/registry-search-vm-pilot/` directory and its digest verified. If interrupted, run `node /opt/datatug/registry-search/recover.mjs cleanup`; cleanup requires matching ownership receipts. The primary data and credentials are retained.

This pilot does not configure DNS, TLS, a firewall rule, a public gateway route, automatic publication or scheduled backup. Publishing requires an explicit manifest and the single-publisher lock described below. Full staging qualification and Typesense Cloud cutover remain required before public search. Detailed inventory and measured receipts live in the backstage `registry-metadata-search` plan.

`registry-search-export/v1` inputs are pinned in a manifest. Each `sources[]` entry has `domain`, `repository`, `revision` (40 lowercase hex), `sha256` (of the exact JSON bytes), and an HTTPS `url` ending in `/registry-search.json`. All three domains are required. `approved_revisions` pins every additional object repository allowed in provenance. `sequence` is a monotonically increasing publication number. `smoke_queries` contains at least one `{domain,q,id}` for each domain. Source exports contain `format`, `domain`, `source_revisions`, `fixture`, and `documents`. Shared source pins must agree across all exports and with the manifest. The three canonical site hosts and route prefixes are fixed in code.

Prepare JSONL without an engine:

```sh
node registry-search/cli.mjs prepare /path/to/manifest.json /path/to/output.jsonl
```

For explicit local fixture proof, set `REGISTRY_SEARCH_ALLOW_FIXTURES=1` and use `file` entries in the manifest. For a local Typesense service bound by a named owner to loopback, the importer accepts loopback HTTP only with `REGISTRY_SEARCH_MODE=local` and the same fixture flag. No service is started by this package.

Publication requires `REGISTRY_SEARCH_MODE=staging|production` (or `local` for explicit fixture proof), `REGISTRY_TYPESENSE_ORIGIN`, `REGISTRY_TYPESENSE_ADMIN_KEY`, and `REGISTRY_SEARCH_STATE_DIR`. Production additionally requires `REGISTRY_TYPESENSE_DEPLOYMENT=cloud` and Python 3 with `fcntl` on macOS/Linux. Keep the admin key in the process environment, never command arguments or a tracked file. The state directory must be durable and owned by one publisher. The CLI acquires a kernel advisory lock and replaces the lock helper with the Node publisher in the same process, retaining an inherited lock descriptor for its full lifetime. The lock releases automatically if the publisher dies; its file remains to avoid an unlink/recreate race. The sequence fence is persisted before the alias switch. Do not run a second publisher on another host against the same alias; direct library callers must hold the same lock. Reconciliation/trigger dispatch must be connected to a single durable owner before staged auto-publication can be claimed.

```sh
node registry-search/cli.mjs publish /path/to/manifest.json
```

The importer validates the complete corpus, creates `registry_metadata_<generation>`, checks every JSONL response row, count, and a smoke query for each domain, then updates `registry_metadata`. It leaves the prior live collection intact for rollback. It may delete and recreate an abandoned inactive candidate on retry; it never writes into the live collection. A failed import cannot switch the alias. Retrying the same active content is idempotent.

The gateway accepts `POST /v1/registry-search` with `{q,domain,kind?,parent_id?,page?}` and returns `{hits,found,page,generation}`. Errors have an `error` code and non-2xx status. It requires a rate-limit binding and a search-only Typesense key. Use the dedicated `registry-search/wrangler.jsonc`; configure secrets with Wrangler and bind an approved gateway route only after staging verification. Its production mode checks `REGISTRY_TYPESENSE_DEPLOYMENT=cloud` and an exact `REGISTRY_TYPESENSE_CLOUD_HOST` matching the origin. The three production site origins are fixed; staging origins require an explicit setting. No public route is configured here.

`npm test` runs the existing Vitest suite and the dedicated Node tests for validation, import failures, sequence fencing, query policy, and unavailable errors. `node registry-search/benchmark.mjs` measures synthetic 10k/50k/100k source-record expansion and JSONL bytes. One source record means a graph concept, model descriptor or collection, or registered database; child fields/entities/collections and grouped servers are derived documents. The fixture includes repeated `CustomerId_N` field labels under different parents, component-like effective fields, a sparse ModelSpec tail of 100-field descriptors, and a heavy database tail. `node registry-search/benchmark.mjs --profile 10000 --out /private/tmp/registry-10k` emits three explicit fixture exports and a pinned manifest for local Typesense import; repeat with 50000 and 100000 in separate processes. These figures are not engine disk/RAM, query latency, relevance, or live capacity measurements. The synthetic mix is an experiment shape, not a forecast or capacity qualification; run the plan's real Typesense and browser gates separately.
