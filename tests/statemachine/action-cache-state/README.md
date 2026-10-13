# Action cache cross-surface state machine

Run normal coverage with:

```
bb remote test //tests/statemachine/action-cache-state/... --config=nobug --nocache_test_results --test_arg=-test.v --test_output=all
```

Run the isolated known divergence with:

```
bb remote test //tests/statemachine/action-cache-state:cas_delete_repro --test_tag_filters=bug --nocache_test_results --test_output=all
```

Released images are enterprise-v2.314.0: app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`, proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`. Actual fixture image references and container inspect metadata are preserved. Calls address only fixture-local peers; CLI children receive PATH, temporary HOME and disposable fixture credentials.

| Mutation | Checked surfaces | Result |
| --- | --- | --- |
| UpdateActionResult | Both endpoints' complete GetActionResult; all eight inline variants; HTTP `/file/download` AC fallback; app Cache.GetMetadata and UI GetCacheMetadata via gRPC/HTTP | Shared action alternating app/proxy writes, warm projections, repeated writes, complete fields, independent AC serialized size; explicit stdout/stderr representation |
| BatchUpdateBlobs | Both endpoints' ByteStream.Read, BatchReadBlobs status/digest/count/bytes, FindMissingBlobs, Cache metadata; UI metadata; HTTP download; public GetFile streaming gRPC/HTTP and GetFileRange gRPC/HTTP (full and nonzero-offset ranges); bb download at both endpoints | All modeled historical blobs checked after every prerequisite mutation; prior action result remains unchanged |
| ByteStream.Write | Same CAS content surfaces and metadata; QueryWriteStatus | Immutable content agrees; released QueryWriteStatus explicitly returns Unimplemented (unsupported, not a new finding) |
| Directory upload | CAS content surfaces plus REv2 GetTree at both endpoints, UI GetTree via gRPC and browser's framed HTTP transport, GetTreeDirectorySizes via gRPC/JSON HTTP/framed HTTP, bb download directory extraction | Repeated directories and alternating writers with two API keys in one org; exact directory fields, size/count and extracted bytes/executable bit |
| DeleteFile (AC) | Both endpoints' GetActionResult absence, followed by three separated stable reads and re-creation checked through inline and HTTP content projections | Opt-in API; bounded 10-second convergence poll, HTTP download and both app metadata RPCs assert absence, measured duration in test log |
| DeleteFile (CAS) | App ByteStream absence and FindMissing; warmed proxy ByteStream content polled every 100 ms for 10 seconds | Confirmed persistent divergence; see FINDING-cas-delete-proxy.md |

The original round-one audit probe is incorporated with original expectations, without perturbations. The action-result model supplies Worker metadata independently, so expected serialized size does not derive from the server response. Complete protobuf equality includes metadata, symlinks, output fields and stream representation, including default/absent fields. Output file inlining is required exactly when requested. Source inspection establishes that stdout/stderr flags are ignored in this release: raw stays raw and digest stays digest, rather than accepting either representation.

Metadata timestamps are required to exist for stored nonempty CAS blobs; app JSON mtime/stored size are compared to app Cache metadata. Access timestamps legitimately change during reads; no equality across independently maintained app/proxy timestamps is assumed. The size-model fixture uses an explicit app and proxy compression threshold of 1 MiB for independently predictable AC stored size. CAS app stored sizes equal the modeled bytes; proxy read-fill may preserve upstream zstd, so its stored size must match exactly the reference raw length or independently computed zstd length. This is a physical storage representation difference, not changed content. Default-config shared-action coverage runs separately. Proxy AC metadata is not keyed by the action digest: its internal storage is salted request-variant pointers, so it is not a public action-digest metadata surface.

The proxy does not register ApiService (released `enterprise/server/cmd/cache_proxy/cache_proxy.go`); DeleteFile is an app-only mutation, so proxy-originated writes followed by app deletion are covered. There is no supported proxy DeleteFile endpoint to exercise in reverse.

Deletion tests explicitly enable `--enable_cache_delete_api=true`. CAS deletion divergence is not described as a default-config deletion feature. AC sequence stability probes are separated by 100 ms, and common AC/CAS observers use three probes separated by 20 ms; successful CAS surface observation times are logged as upper bounds, not exact replication latency. Public CLI supports CAS and interpreted Action/Command/Directory downloads (both raw bytes and parsed JSON are asserted); it has no action-cache-result read or cache deletion command.

## Source inventory

Inspected the released checkout under `~/buildbuddy-src`: `proto/{remote_execution,cache,cache_service,resource,buildbuddy_service}.proto`, `proto/api/v1/{service,file}.proto`; `server/remote_cache/{action_cache_server,content_addressable_storage_server,byte_stream_server,directory_size}`, `server/cache_server`, `server/buildbuddy_server`; `enterprise/server/{action_cache_server_proxy,byte_stream_server_proxy,api}`; `cli/download/download.go`; `app/invocation/invocation_action_card.tsx`, `app/service/{rpc_service,length_prefixed}.ts`.

The action card obtains Action, Command, input Directory, stdout/stderr/log content and ActionResult fallback through `/file/download`; protobuf/JSON formatting happens in the browser. Directory traversal and sizes also have dedicated RPC surfaces. Invocation GetAction/GetExecutionDownloads/GetLog are BES/execution-derived state, not projections created by these cache mutations without an invocation/execution; they are not populated by this suite. No execution or BES mutation is fabricated.

## Validation

Expanded overwrite, delete/recreate, public-file and tree baseline passed:
https://app.buildbuddy.io/invocation/c632a337-e3b7-4d92-8d46-29a33794cd45.
The isolated CAS deletion mismatch reproduced twice; links and observed durations are in the finding. A fresh two-run repro also failed as expected: https://app.buildbuddy.io/invocation/83a8a8e5-e9bf-4b57-aff1-52b054a0c140 (both runs retained warmed proxy bytes for the entire 10-second window). Final expanded suite passed (371.5 seconds): https://app.buildbuddy.io/invocation/be664240-05c0-47e8-b2e2-4353997131d7. Use `--test_arg=-test.v` to retain per-step convergence measurements in successful test output. No shared dependency/harness changes or PR required.

Verbose shared-action sequence passed: https://app.buildbuddy.io/invocation/76d3ac51-8d17-42d0-a7bc-3481a938327e. Revision 3 AC observation completed within 57.706413 ms; a CAS all-content-surface check completed within 603.210385 ms. AC deletion validation including separated stable reads and retained-blob checks took 2.746906281 seconds. These are observation upper bounds including reads and CLI overhead, not exact replication latency.
