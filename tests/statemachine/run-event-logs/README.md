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
| `BuildBuddyService.WriteEventLog` (client stream, RUN_LOG) | `GetEventLogChunk`, streaming `GetEventLog`, UI HTTP `GetEventLogChunk`, `bb view` | Existing owned invocation; newline-terminated UTF-8, repeated equal writes, >2 MB chunk boundary, exact concatenation against reference bytes after successful stream close, before and after terminal status. |
| `BuildBuddyService.UpdateRunStatus` | `GetInvocation` gRPC and UI HTTP; run-log chunk/stream completion | Unknown → in-progress → repeated in-progress → each of success/failure/disconnected on separate invocations. Terminal streaming response must reach EOF. |

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
Bounded polling permits writer-close visibility before checking each surface.
A successful client-stream close is the acceptance boundary; no per-message
ack exists. Tests do not claim visibility after each unacknowledged Send,
concurrent live-tail replacement semantics, or repeated writer sessions.
No shared harness or dependency edits are needed.
