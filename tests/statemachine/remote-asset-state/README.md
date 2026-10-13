# Remote Asset state conformance, round 2

Released images: app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`, proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906` (enterprise-v2.314.0; harness defaults).

## Mutation / surface matrix

| Mutating RPC | Surfaces checked after every successful step | Result |
| --- | --- | --- |
| FetchBlob(app) | Response status/digest/function/URI/qualifiers/expiry; origin count; both CAS presence, identity and zstd BatchRead, identity and zstd ByteStream, Cache metadata, bb download; app UI metadata gRPC/JSON; GetFile streaming gRPC/HTTP; HTTP download; gRPC/HTTP range; for serialized Directory: both CAS GetTree, bb Directory JSON, app UI GetTree gRPC and HTTP proto+prefixed, UI tree sizes | Agreement for supported reads. Proxy upstream presence is not a claim of independent replication. |
| FetchBlob(proxy) | Same proxy CAS/bytes/CLI/metadata checks; app CAS absence, both app metadata RPC errors and UI JSON error; proxy-addressed app full-file readers versus gRPC/HTTP range; proxy Directory GetTree versus CLI decoding, app HTTP tree | Known permanent disagreement: full-file succeeds, range fails; Directory bytes succeed, GetTree fails. |
| FetchDirectory(app/proxy) | Runtime Unimplemented, valid URI and digest function | No mutation implemented. |
| PushBlob(app/proxy) | Runtime Unimplemented, valid URI and digest | App method unimplemented; proxy Push service unregistered. |
| PushDirectory(app/proxy) | Runtime Unimplemented, valid URI and Directory digest | App method unimplemented; proxy Push service unregistered. |

Both default and nondefault instances run deterministic create, immutable repeat, origin update, retained-version lookup, freshness update and repeat, second update to serialized Directory content, and re-create of initial content. Every successful operation checks every retained digest. Two separately provisioned API keys in the same org read each blob. CAS is immutable; there is no implemented Remote Asset delete or association update RPC, so deleting content is not a valid Remote Asset sequence. Unsupported mutation RPCs are called on both endpoints with valid requests; they cannot create observable state.

Presence convergence is bounded at 10s, logged per observation, then checked three more times at 50ms intervals. Observed successful presence is around 51–53ms, including the poller's initial 50ms interval; these are upper bounds, not replication latency. Default proxy presence forwards upstream. Subsequent ByteStream reads can warm local cache, so this suite makes no unsupported claim of autonomous app-to-proxy replication. Proxy-origin app absence is checked for 1s on the first mutation, and again after every subsequent step. Routing source explains why this does not converge without a separate app write.

Metadata checks require positive stored sizes and access/modify timestamps within the clock bound, exact digest size, and agreement of stored size / modification time between gRPC and UI JSON. Last-access values are observational and may change with reads; exact cross-container timestamp equality would be incorrect. Batch responses assert digest identity as well as status and bytes. Fetch responses explicitly assert omitted URI on checksum hits and origin URI on downloads; qualifiers and expiry are absent in this release. QueryWriteStatus is invoked and asserts the released Unimplemented result.

## Source inventory and surface applicability

Reviewed released `server/remote_asset/{fetch_server,push_server}`, `proto/{cache,cache_service,resource,buildbuddy_service}.proto`, `proto/api/v1/file.proto`, `enterprise/server/api/api_server.go`, `enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go`, `server/remote_cache/{directory_size,content_addressable_storage_server,byte_stream_server}`, `enterprise/server/cmd/cache_proxy/cache_proxy.go`, and `cli/download/download.go`.

Frontend callers: `app/service/rpc_service.ts` maps raw/text/arraybuffer/log downloads to `/file/download` and streams to `application/proto+prefixed`; `app/invocation/invocation_action_card.tsx` uses tree sizes and downloads Directory bytes for decoding; `app/invocation/cache_requests_card.tsx` uses cache metadata. `app/util/get_tree.ts` uses streaming tree RPCs. These are checked directly; there is no separate raw-JSON blob storage endpoint. CLI `download --type=Directory` decodes the same immutable bytes to JSON, asserted by protobuf equality. Invocation artifacts, execution logs and action results require mutations in other services and are not created by Remote Asset requests.

The proxy has no native BuildBuddyService or API v1 surface in this fixture; app APIs are checked using its URI. Findings in this directory describe the known cross-surface contract divergences without claiming novel upstream bugs. The origin is test-local and requires only the private-IP allowlist override. All images run inside remote tests.

## Findings and CI

- [Range disagreement](app-remote-asset-range.md): full-file proxy URI succeeds; range gRPC NotFound / HTTP 500. Positive contract target `:proxy_range_bug_test`, tagged bug.
- [Tree disagreement](cacheproxy-remote-asset-tree.md): proxy Directory bytes succeed; proxy CAS GetTree FailedPrecondition. Positive contract target `:proxy_tree_bug_test`, tagged bug.

The default target asserts the known behavior and stays green. Each isolated positive-contract test reproduced twice in fresh fixtures in https://app.buildbuddy.io/invocation/5d069f6e-a0c2-4533-838f-944260af5806 (`-test.count=2`). Independent main-sequence repetitions: https://app.buildbuddy.io/invocation/2d78fe4b-c911-4783-8758-c870709fa482 and https://app.buildbuddy.io/invocation/c22a3509-1175-40d4-b8cd-69f89f315c25 . Main suite including metadata JSON comparisons passed in the latter invocation.

## Explicit limits

SHA256 is the tested digest function; alternative hash algorithms are not covered. No eviction, deletion, auth fault injection, or origin checksum mismatch is injected. No independent autonomous replication promise is inferred from upstream-backed reads. Range assertions use valid interior ranges; boundary combinations remain additional coverage. Directory tests use a single level with retained file content, not deeply nested trees. No shared harness or dependency changes are needed.
