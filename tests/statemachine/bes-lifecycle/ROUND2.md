# Round 2 verification

Only the assigned directory is changed. No PR is opened.

| Mutating RPC | Asserted surfaces | Result |
|---|---|---|
| PublishLifecycleEvent | Enqueue/repeat/started: absent UI/gRPC detail, public ID/commit, streaming logs. Completion notifications: invocation model readers, stats/trends, detail/raw JSON/proto graph, build-log downloads/views, streaming logs and CLI | Standalone no-op; proxy Unimplemented |
| PublishBuildToolEventStream | ACK/EOF; UI/gRPC detail/search/stat/trend; public REST/gRPC invocation ID/commit including metadata; UI/gRPC chunks, streaming logs, public REST/gRPC log pages; raw JSON/proto and build-log download/view; bb view; populated UI/public target/action projections; named-set artifact HTTP reads; app/proxy ByteStream bytes | Normal coverage passes, with isolated default-tail consistency defect |
| DeleteInvocation | UI/gRPC detail/search/stats/trends absent; public ID/commit absent; log readers/stream/CLI missing; UI/public targets/actions NotFound; artifact download/view missing-resource contract; two-key same-ID re-creation and subsequent graph/log checks | Normal coverage passes; isolated artifact error-mapping defect |

Final tests, uncached:

- https://app.buildbuddy.io/invocation/1d5673a2-a358-4ce8-85e3-d4f65d827cdd
- https://app.buildbuddy.io/invocation/c29ed9b6-c243-40e9-b8d7-a9be7fadd029

Both runs pass `conformance` and intentionally fail `deleted_download_repro` and
`live_tail_repro` at their documented contract assertions. Default `nobug` suite
excludes the repros. The normal suite also passed standalone in
https://app.buildbuddy.io/invocation/01267101-eb80-4342-840e-f4ac0efe1781.

Final observed model convergence (including initial state polling): 279–313 ms.
Live growing log content plus two stable observations: 601–620 ms; final log
snapshot: 622 ms. Re-creation publication plus graph/artifact/CLI verification:
1.469 s and 638 ms. These observations include reader overhead. Stability is
bounded repeated observation, not a soak.

Confirmed default-config divergences:

- Deleted raw_json/raw_proto/buildlog download/view: permanent 500/Internal vs
  RPC NotFound; no convergence in two seconds per combination, in two full runs.
  Source shows deterministic DB error mapping; no stale content observed.
- Live default chunk/page selectors and CLI omit output that explicit chunk and
  streaming readers expose. No idle convergence in two seconds; completion
  resolves it. Observed mutation-dependent windows 2.765 s and 2.241 s.

One combined dispatch was canceled after more than ten minutes without further
runner output; it is not counted as completed verification or a product defect.
The explicit eight-minute remote timeout was used for subsequent completed runs.

## Remaining work against the universal contract

`SURFACES.md` enumerates callers, projections and exclusions. This expansion does
not complete EVERY surface after EVERY send: initial invocation metadata events
remain batched; configured OLAP, live raw/build-log downloads, large pagination,
child-invocation expansion and execution/profile/coverage/log artifact types remain
unvalidated. Two keys share one authenticated identity. Proxy BES writes cannot
be exercised successfully because the released proxy has no BES service.

Coordinator request: add a shared harness facility for a digest-pinned ClickHouse
sidecar on the owned Docker daemon, with network/config injection and cleanup/log
preservation. No shared harness/dependency changes were made here.
