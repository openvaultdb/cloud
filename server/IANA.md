# Default-off IANA operator query component

This component prepares the fixed IANA HTTP Status Codes CSV path for independently
admitted operator requests. It ships no accepted operator decision, publisher
definition evidence, decoder digest, rights digest, source response, or production
configuration. Its synthetic tests do not contact IANA. Directory registration,
public access, paid use, live runtime/browser verification and sink review remain
separate gates.

The Go service selects IANA only when `OVDB_IANA_ENABLED=true`. Selection requires
all of `OVDB_IANA_HOST_CONFIG`, `OVDB_IANA_ADMISSION_FILE`,
`OVDB_IANA_ADMISSION_SHA256` and `OVDB_IANA_PROXY_SECRET`. An unset or `false` flag
leaves IANA unavailable; any other flag value refuses startup. The admission SHA
must be the independently reviewed lowercase SHA-256 of the exact decision bytes.
The proxy secret must be 32–128 ASCII letters, digits, `_` or `-`.

## Operator-owned files

The admission is a closed JSON object bounded to 4 KiB. Its required fields are:

| Field | Required value |
| --- | --- |
| `format` | `ovdb-iana-operator-admission/1` |
| `decision` | `operator-free-transient-read-only` |
| `approvedBy`, `approvedAt` | Nonempty trimmed operator identity; nonzero RFC3339 timestamp |
| `hostConfigSHA256` | Exact raw host JSON SHA-256 |
| `definitionSHA256`, `decoderSHA256`, `rightsSHA256` | Independently reviewed definition, decoder and canonical source-right digests |
| `executorId`, `resourceId` | `openvaultdb-cloud`, `iana-http-status-codes` |
| `method`, `path` | `POST`, `/v1/databases/iana-http-status/dtql` |
| `maxReadsPerExecution`, `maxRows` | `1`, `50` |
| `publicAccess`, `paidAccess` | `false`, `false` |

The approver fields record an operator assertion, not a verified signer identity.
This gate permits at most one upstream read **per explicit execution**. It has no
persistent single-use activation ledger. Before configuring flags and secrets, the
separate operator pilot process must accept the exact pins, code, rights, sinks,
and operational conditions and control how many invocations are authorized. A
failed execution never authorizes an automatic retry or broader query. The
transport envelope `maxRows: 50` is not a one-row pilot authorization. A private
one-attempt control must independently bind the exact limit-one query digest and
consume a durable owner/session claim before dispatch, including on failure.

The host JSON is bounded to 32 KiB, must match `hostConfigSHA256`, and has exactly:
`format` (`ovdb-iana-host-candidate/1`), `publisherDefinition` (local metadata file),
`httpManifest` (local transport manifest), `decoderVersion`
(`strict-csv-three-column/1`), `decoderModuleVersion` (`v0.4.0`), `sourceRight`, and
`binding`, with optional `directoryDefinition` for the HTML evidence route. The
binding fixes `provider:iana/HttpStatusRegistryRow`,
`ovdb:openvaultdb-cloud/iana-http-status/rows`, `iana-http-status-codes` and the three
admitted digests. The source right must match its canonical digest, preserve the
mounted declaration. The `publisher-definition-verified` origin carries exactly
one valid provider GitHub pin whose byte count and SHA match the local definition
metadata. The IANA-specific `publisher-html-metadata-verified` origin instead
binds `publisherDefinition` to the exact canonical `publisherHtmlDefinition`
descriptor of independently checked official registry/licensing HTML. Its
separate Directory `discovery` pin must match the bytes and SHA of
`directoryDefinition`. The authored Directory metadata is not publisher
verification; neither evidence artifact is a captured or verified CSV response.
No production approval is inferred from a synthetic fixture.

The local transport file must byte-match `ianaHTTPManifest` in
[`iana_candidate.go`](iana_candidate.go). The mounted manifest is checked again to
refuse a file swap. `server.NewChecked` additionally validates the native IANA
request profile, source-right notices, publisher-definition evidence and fixed original
resource/licensing URLs. Missing files, duplicate or unknown fields, malformed
digests, changed bytes and broadened admission fail closed. Keep all operator files
and service bindings under operator control; the service does not fetch metadata
to fill admission blanks.

## Selected request boundary

Only `POST /v1/databases/iana-http-status/dtql`, with no query string and exactly one
valid `X-OVDB-IANA-Proxy-Secret`, reaches the Go IANA handler. The Worker exposes
only `POST /iana/v1/databases/iana-http-status/dtql` when `IANA_ENABLED=true`.
It additionally requires `IANA_RUN_ORIGIN`, equal to `CHINOOK_RUN_ORIGIN`, a
canonical root HTTPS `*.run.app` origin without credentials, explicit ports or URL
parameters; `IANA_PROXY_SECRET`; and a separate `IANA_OPERATOR_TOKEN` with the same
32–128 character syntax. The caller supplies the operator token in
`X-OVDB-IANA-Operator-Token`. Missing, wrong or duplicated tokens refuse before
upstream I/O. The Worker strips that token, Authorization, Cookie and client proxy
secrets, then supplies only the trusted IANA proxy secret to the fixed Go path.
ECB credentials do not select IANA. None of these bindings is in `wrangler.jsonc`.

The selected native profile requires explicit limits of 1–50 and native string
fields `Value`, `Description`, `Reference`. It does not interpret ranges as scalar
status codes or dereference RFC links. The released transport bounds one fixed CSV
GET to 64 KiB, 512 parsed rows and 10 seconds, with no redirects or snapshots. The
Worker uses the existing 15-second no-retention request/stream deadline and caller
cancellation. Request/response caches are disabled; only fixed diagnostic markers
are emitted, with no queries, rows, credentials or provider error text. This local
behavior is not a claim about deployed intermediary/platform sinks.

## Synthetic verification

Go tests construct invented definition metadata and CSV entirely in test-local
files/memory. Worker tests inject a fake upstream. The opt-in
`OVDB_IANA_CROSS_RUNTIME=1` / `TestIANASelectedCrossRuntimeChain` test owns a temporary
loopback listener and drives `createWorker().fetch` through `configuredHandler`:
one bounded native result, refusal before upstream I/O, and a marker-bearing
provider panic. It checks Worker cache/log sinks and Go slog/net-http markers.
No real IANA GET, Cloud Run call, source snapshot or public activation is part of
these tests.
