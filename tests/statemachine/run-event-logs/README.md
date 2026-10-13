# Run log and status conformance

Target: `//tests/statemachine/run-event-logs:go_default_test`.

Released app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
(enterprise-v2.314.0). CLI: released 5.0.492 linux-x86_64, downloaded
inside the remote test and verified against SHA256
`e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`.
All service requests use fixture-local addresses and disposable fixture keys.

## Source inventory at v2.314.0

| Mutation | Read surfaces | Contract and coverage |
| --- | --- | --- |
| `BuildBuddyService.WriteEventLog` (client stream, RUN_LOG) | gRPC chunk and stream; UI JSON, binary proto and prefixed proto chunks; HTTP prefixed stream; run-log download; fixture-local `bb view`; build-log-only API gRPC/REST control | Exact accepted bytes after each completed writer session, repeated identical sessions using two same-org API keys, repeated equal messages, UTF-8 and >2 MB boundary. UI empty chunk ID / 100,000-line tail and previous/next/live metadata agree with gRPC. Active writer live tail is additionally checked before close. |
| `BuildBuddyService.UpdateRunStatus` | gRPC/UI JSON invocation lookup and search; inlined child invocation status in parent lookup; all log surfaces above | Unknown → in-progress → repeated in-progress → logs → repeated in-progress → success/failure/disconnected. Empty HTTP streams stay open while in progress. Already-open gRPC reader observes terminal transition; new gRPC/HTTP streams reach EOF. Terminal state is checked again after 350 ms, with repeated downloads and CLI reads. |

Source references: `proto/eventlog.proto`, `proto/invocation.proto`,
`proto/invocation_status.proto`, `proto/buildbuddy_service.proto`;
`server/buildbuddy_server/buildbuddy_server.go` (`WriteEventLog`,
`UpdateRunStatus`, `GetEventLog`); `server/eventlog/eventlog.go`;
`enterprise/server/api/api_server.go` (`GetLog`);
`enterprise/server/cmd/cache_proxy/cache_proxy.go` (`registerGRPCServices`);
`cli/stream_run_logs/stream_run_logs.go`, `cli/view/view.go`.

`api.v1.GetLog` has no run-log selector and explicitly describes build logs.
The test checks both REST and gRPC return empty build logs for these invocations,
while all run-log surfaces return nonempty run logs. This is separation of two
log types, not a missing run-log consistency surface. Public v1 invocation
responses likewise do not expose `run_status`.

Cache proxy does not register BuildBuddyService or ApiService and does not
store run status/event logs, so no proxy agreement surface applies. Neither
item RPC creates or deletes invocations/logs. BES only seeds an existing,
completed invocation with no console logs; it is not the run-log writer.
Terminal status cannot be updated again by contract (FailedPrecondition);
those requests are excluded under the valid-input requirement.

The reference model records accepted stream bytes and expected run status.
A successful client-stream close is the acceptance boundary; no per-message
ack exists. Provisional live bytes are checked by bounded polling before close.
New writer sessions start at chunk zero in `chunkstore.Writer`; replaying the
same complete content checks repeat sessions without assuming append across
sessions. Within a session, repeated messages append to the reference model.
Two locally provisioned API keys in the same organization alternate mutations
and lookup/search reads. Parent RUN_ID and child PARENT_RUN_ID fixtures exercise
inlined child status on every status step.

Convergence timings are logged per checkpoint: CLI timing includes process
startup and the polling interval, not just product propagation delay. Active-tail
timing is measured from the last Send until full bytes and live=true are observed.
Terminal state is rechecked after a 350 ms stability interval. Persistent errors
fail bounded polls. Download requests are repeated to check stable content.

Additional source references: `app/service/rpc_service.ts` (download and binary
RPC transports), `app/service/length_prefixed.ts` (stream frames and trailers),
`app/invocation/invocation_logs_model.tsx` (empty chunk ID, minLines, live-tail
replacement), `app/invocation/child_invocation_card.tsx` and
`app/invocation/invocation_card.tsx` (child/history status),
`server/http/protolet/protolet.go` (HTTP RPC dispatch).
The UI raw-logs card renders BES events from GetInvocation and downloads
`artifact=raw_json`; raw JSON and raw proto downloads are checked for semantic
stability (normalizing only protobuf metadata map order) after run mutations because they expose BES, not run-log/status state.
Public v1 GetInvocation excludes run status. There is no further run-log metadata RPC.

Create/delete/re-create and cross-proxy writes are inapplicable to these two
RPCs: they only mutate an existing app invocation, and the proxy registers neither
service. No shared harness/dependency changes or non-default app config are used.

Before writer close, full live content is assembled using explicit forward chunk
IDs and compared with the UI's explicit live-chunk read (`live=true`, next ID
unchanged). Empty-ID/minLines requests select durable chunks and do not aggregate
the provisional live chunk; the test therefore applies full cross-surface byte
identity at the acknowledged writer-close boundary. Individual Send calls have
no acceptance acknowledgement in this protocol.

## Round 2 validation

Final targeted uncached pass:
https://app.buildbuddy.io/invocation/0fa9e779-d4e3-4a09-a972-e1180b57ede4
(46.2 s test runtime). Active-tail visibility: 167–169 ms. CLI observation:
313–386 ms including process startup/polling. All three terminal sequences pass;
no product divergence was observed, and no non-default app config was needed.
Earlier expanded sequence/control pass, before adding raw artifact invariants:
https://app.buildbuddy.io/invocation/8a6fdca5-13e9-49ed-9793-3baade3aef73.

Probe-development failures were test assumptions/encoding errors: wrong HTTP
route prefix, case-sensitive trailer header comparison, requesting a live tail
with empty chunk ID, wrong GetLog page-token field, and comparing unordered
protobuf maps as serialized bytes. None is recorded as a product finding.
