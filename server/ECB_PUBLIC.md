# Default-off public-free ECB candidate

This candidate is synthetic-tested infrastructure. It grants no source rights,
public admission or activation. Production Worker/Go bindings remain absent;
Directory remains unconfigured. Real source reads and operational sink proof
require separate authorization and review.

The separate `ovdb-ecb-public-free-admission/1` decision binds exact host config,
publisher/decoder/rights pins, released dalgo2http v0.4.0, public-free audience,
paid=false, Directory/backend origins, Worker/Go paths, one read/50 rows, an
explicit cost owner and a <=30-day approval window. Operator admission and
secrets cannot authorize this path. Module source verification establishes the
v0.4.0 ECB decoder SHA256 equals the existing reviewed decoder digest.

The new Worker route is `/ecb-public/v1/databases/ecb/dtql`. The Go route remains
`/v1/databases/ecb/dtql` and can select only one of the two ECB compositions.
Only the Worker supplies the separate public proxy secret and admission digest.
No request cookies, Authorization, billing, paid capabilities or caller internal
headers authorize the zero-fee route. Exact-origin OPTIONS stays local.

Go uses the existing native query grammar/decoder and released
`providerreads.ValidateMetadata`. Its public-only wrapper holds row flushes,
requires a complete closed terminal, validates full sourceRights/usedSourceIds
against independent config, validates bound observation digests and dates and
native lexical rows, then signs the exact completed bytes. The MAC domain binds
admission digest, execution ID, HTTP status and content type as newline-separated
fields before the body. One separate 32..128-byte ASCII server-held key is used;
no key list, fallback or operator-key reuse is supported.

Worker holds all bytes to EOF and verifies that MAC before returning success.
This is an attestation from the trusted selected Go completion validator, not a
browser-authored proof or independent legal clearance. Tampered, interrupted,
late, oversized or unauthenticated bytes produce only a fixed no-store error;
no row prefix is sent to the browser. The completion proof is stripped from the
external response. Unrelated sample/operator streaming is unchanged.

## Bounds and transient memory

The public path tightens the reviewed ceilings to request8KiB and completed
response64KiB; it does not reduce native projection/equality/limit1..50 scope.
Go request YAML is capped before parsing and rejects aliases/duplicates,
>1024 nodes/>16 depth/>1024-byte values. Completion JSON is token-scanned before
typed expansion: <=2048 tokens, <=16 depth and <=4096-byte strings; unknown or
duplicate members and trailing bytes reject. Rights metadata is included in the
64KiB cap and remains under the library256KiB bound. Rows have <=3 native
string fields <=128 bytes. No XML parser or metadata protocol is duplicated.

Worker never parses untrusted response JSON. Its owned response buffers are a
fixed64KiB reader buffer, <=64KiB completed slice, <=65KiB MAC input and <=64KiB
Response copy (<260KiB); request buffers add <=16KiB and trusted admission is
<=4KiB. It has no decoded response text/object graph. Oversized chunks reject
before copying. Go owns one64KiB capture plus <=64KiB typed strings. The bounded
YAML/JSON token counts reserve1MiB for parse nodes/maps; bounded metadata
validation/canonicalization reserves another1MiB, and gate scratch/raw copies
reserve512KiB. The combined gate data budget is below4MiB with headroom. This
accounts for gate-owned data, not the process/runtime heap or the separate
existing provider XML decoder's2MiB wire bound. These are explicit structural
caps, not a claim that a sampled heap measurement proves a global memory limit.

Worker has one10-second deadline through admission/read/verification/release;
Go has a9-second request context and socket read/write deadlines. Worker aborts
and cancels owned reader on failure/cancel/deadline and disposes timers/listeners.
Go absorbs late flush/abort/panic, drops buffers and releases the service slot.
Neither side retries, persists, spools, caches or keeps a replay body.

Malformed/unsupported native query bodies can consume one bounded Worker
transport limiter attempt/slot and a Go HTTP call. Go alone performs native
syntax/operation validation, and refuses those bodies before its execution
slot or any ECB provider read. Header/audience/admission/origin/size refusals
happen before backend dispatch. The transport budget deliberately includes
invalid native attempts to bound parser/HTTP work; it is not a successful-query
counter. No before-backend native-syntax guarantee is claimed. Synthetic tests
assert Worker limiter1/backend1 and Go execution-slot0/provider-read0 separately.

The Go instance permits one concurrent read and six executions/minute. Worker
requires a shared ECB_PUBLIC_LIMITER binding and uses one service key plus one
per-isolate slot; activation must prove the binding is6/60s and impose global
instance/service cost bounds. No such production binding is added here. A
rate-limit response is fixed/no-store. Headers alone do not prove CDN/log/SW
sink absence; those remain explicit later gates. No data-bearing logs are added.

Synthetic tests cover complete lexical/zero output, exact admission/isolation,
unsupported requests, cancellation/deadline, budgets, byte tampering and
row-then-invalid/missing/footer failure. CI explicitly executes
`OVDB_ECB_PUBLIC_CROSS_RUNTIME=1 go test . -run TestECBPublic -count=1` against
an owned loopback Worker-to-Go chain with authored XML, including late failure
with zero marker release. No test needs a real ECB request.
