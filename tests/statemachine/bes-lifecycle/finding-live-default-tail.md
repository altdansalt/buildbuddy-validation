# Default log selectors omit the first live chunk

Title: Default UI/public log reads and bb view omit buffered live output
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Valid running builds expose their console output to explicit chunk/stream readers while default-tail readers and the CLI show empty output. This loses observability during the build; no unauthorized data access was observed.
Status: confirmed
Repro test target: //tests/statemachine/bes-lifecycle:live_tail_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/1d5673a2-a358-4ce8-85e3-d4f65d827cdd

## Expected behavior

The `GetEventLogChunkRequest.chunk_id` proto comment says that an empty selector
gets the last chunk. After a valid progress event, an empty/default selector should
return the latest live console text, as explicit `0000` and streaming reads do.
Public GetLog's default page and `bb view` should expose the same text.

## Actual behavior

Before any durable log chunk exists, explicit chunk `0000` returns the exact
published text with `live=true`, while empty/default selectors return empty content
and `nextChunkId=0000`. The public API propagates the empty result. CLI `bb view`
prints nothing. Its successful exit status does not reveal the missing output.

First observation (strict live CLI oracle):
https://app.buildbuddy.io/invocation/b2178ad5-2bac-4d14-8f2a-39d05e80adeb.

An isolated repro keeps a valid BES stream open, verifies the explicit live
chunk, compares HTTP and gRPC default selectors, and polls CLI consistency for
2 seconds without further writes. It then sends the remaining valid completion
events and verifies that CLI output is restored. The isolated full-surface reproduction is
https://app.buildbuddy.io/invocation/1d5673a2-a358-4ce8-85e3-d4f65d827cdd.
CLI output was still empty after 2.056 seconds of observation. Valid stream
completion restored the CLI; the total observed divergence window was 2.764560
seconds. Both HTTP/gRPC default selectors also returned empty output. This is a
second remote reproduction of the same failure first caught by the live CLI
oracle above. Additional isolated repeat: https://app.buildbuddy.io/invocation/c29ed9b6-c243-40e9-b8d7-a9be7fadd029. The CLI remained empty after 2.053
seconds; completion restored it, with a total observed window of 2.240824 seconds.
Both HTTP and gRPC default readers reproduced again. Neither window is an
intrinsic convergence time: the test deliberately completed the stream after
its two-second idle bound.

## Logs excerpt

```text
explicit=map[buffer:c3RhdGUtbWFjaGluZSBjb25zb2xlIG91dHB1dAo= live:true nextChunkId:0000]
default=map[nextChunkId:0000]
expected CLI: "===== BUILD LOGS =====\nprogress step 0\n"
actual CLI: ""
```

## Notes

Default fixture configuration; no flags or image overrides. The pinned released
CLI is 5.0.492 linux-x86_64 with SHA256
`e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`.
Only disposable local keys and fixture-local endpoints are used. Auth, metadata
publication, and explicit chunk content succeed before any failing comparison.

Source: `server/eventlog/eventlog.go:GetEventLogChunk` handles no on-disk chunks by
checking `req.ChunkId == liveChunk.ChunkId`. The empty selector fails that check,
so the function returns empty text and points to `0000`. `enterprise/server/api`
passes the page token directly as chunk ID. `cli/view.ViewLogs` passes no chunk ID.
The explicit UI/stream reader is not the same path as the empty tail selector.

This is a mutation-dependent transient: idle polling does not resolve it within
the tested bound, but completion flushes logs and restores default-tail reads.
The observed window includes the deliberately bounded polling and observation
work; it is not an intrinsic fixed propagation delay. No non-default configuration
is required. Clients can request the returned `0000` chunk as a workaround.

The failing consistency contract is isolated behind `bug`. Normal live coverage
asserts the explicit content after every progress event and documents the known
empty default-tail result, then asserts correct default/CLI content on completion.
Findings stay in this repository.
