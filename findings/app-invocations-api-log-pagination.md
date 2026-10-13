# GetLog pagination silently skips earlier build log chunks

Title: v1 GetLog starts at the last log chunk instead of the first
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: Authenticated consumers using ordinary pagination silently lose the beginning of multi-chunk build logs, including diagnostics needed for build failure analysis. The data remains stored and accessible through an explicit first-chunk token; this is an API correctness issue, with no demonstrated authorization bypass.
Status: confirmed
Repro test target: //tests/app/invocations-api:log_pagination_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/b0f78873-3eb6-4126-b7c3-90d33ae9b4ed

## Expected behavior

The public [GetLog request and response](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/api/v1/log.proto) describe `page_token` as the `next_page_token` returned by a previous request, and `next_page_token` as empty when there are no more logs. An initial request with no previous token, followed by every returned token, should expose the complete stored build log in order.

The UI's [GetEventLogChunk](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/eventlog.proto) explicitly defines an empty chunk ID as requesting the last chunk. The public API must account for that different starting-position convention.

## Actual behavior

Upload 2,250,021 bytes in small BES Progress events, starting with `FIRST-LINE` and ending with `LAST-LINE`. After the server acknowledges the entire stream and completes it, call `/api/v1/GetLog` with an invocation selector and empty page token. Following every returned token yields only 270,217 bytes: the final nonempty chunk followed by an empty response. The first line and approximately 1.98 MB of earlier output are missing.

Reproduced with independent fixture-local containers twice:

1. [Run 1](https://app.buildbuddy.io/invocation/ce04f0b0-cc3e-43cf-a4c2-8e77d4be554c): 2,250,021 bytes uploaded, 270,217 returned; first line missing, last line present.
2. [Run 2](https://app.buildbuddy.io/invocation/b0f78873-3eb6-4126-b7c3-90d33ae9b4ed): same mismatch. This run adds a control that starts v1 GetLog at `page_token: "0000"` and verifies the full uploaded length and both boundary lines before executing the failing assertion.

Both runs also successfully fetch UI chunk `0000`, verify its first line, and require a next chunk to ensure the log actually spans multiple chunks. Authentication, BES upload, storage, and explicit v1 chunk readback work. No version override, reduced chunk size, external service, or nondefault app configuration is involved. [Passing regression coverage](https://app.buildbuddy.io/invocation/3a9c24c2-4ed8-44df-b564-2137a4bfa42a) verifies one-chunk Unicode logs, metadata, invocation success/failure, targets/test results, and search pagination.

## Logs excerpt

```text
GetLog page 0: bytes=270217 next=0002
GetLog page 1: bytes=0 next=<nil>
uploaded=2250021 bytes, paginated GetLog=270217 bytes, begins with first line=false, ends with last line=true
Not equal:
  expected: 2250021
  actual  : 270217
GetLog starting without a page token must expose all uploaded log chunks
```

This excerpt is test output. The harness preserves full app/Redis container logs, inspect metadata, and configs in the test's undeclared outputs.

## Notes

- Fixture options: `Components: []harness.Component{harness.App}`; default SQLite, disk blob storage, local Redis, self-auth and fixture-created group/API key. No extra config or flags.
- Repro: `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/invocations-api:log_pagination_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all`.
- Likely cause: [APIServer.GetLog](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/api/api_server.go#L381) passes `req.GetPageToken()` unchanged to `GetEventLogChunk`. The [eventlog helper](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/eventlog/eventlog.go#L171) starts an empty chunk request at the last chunk; GetLog returns only `NextChunkId`, discarding the backward navigation token.
- Workaround: supply `page_token: "0000"` for the first request, then follow the returned tokens. This relies on the internal chunk ID format rather than a token supplied by a preceding API response.
- Remote Execution API RequestMetadata defines invocation correlation, not this endpoint's pagination contract. This finding is based on the released BuildBuddy API protos and implementation.
- The original report left status `unverified` pending independent review despite two reproductions. No upstream issue or report was filed.


## Verification

Verdict: **confirmed**, severity **medium** retained. Independently reviewed PR #25's bug test and helpers and executed the unchanged test twice on 2026-10-12 against the exact pinned app digest above. Both uncached runs returned 270,217 of 2,250,021 uploaded bytes, missing FIRST-LINE while retaining LAST-LINE. Both passed the explicit `0000` control, which retrieves the entire log through the same authenticated v1 endpoint. The first run also passed `//tests/app/invocations-api:regression_test`.

The completed BES stream is fully ACKed before retrieval; there is no live-log timing assumption. The test uploads small Progress messages rather than an oversized RPC. The UI first-chunk control establishes multiple chunks, and the v1 first-token control rules out truncation during ingestion, missing storage, auth failures, incorrect protobuf encoding, and a byte-count discrepancy. Only fixture-local app/Redis containers are involved, with default SQLite/disk configuration and disposable local credentials. No configuration or image overrides were used. Identical failures in separate fixtures and the source path below rule out test flakiness.

The [v2.314.0 API documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-api.md#GetLog) shows an initial request with only the invocation selector and describes a token from a previous response, consistent with the [public log proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/api/v1/log.proto). It documents no tail-only initial-page convention. In contrast, the [UI eventlog proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/eventlog.proto) deliberately starts an empty chunk ID at the end. That UI behavior is intended, but carrying it into the public forward-pagination API loses access to preceding chunks through returned tokens.

[APIServer.GetLog](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/api/api_server.go#L381-L411) forwards the empty page token unchanged and returns only NextChunkId. The [eventlog implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/eventlog/eventlog.go#L171-L181) explicitly initializes the empty-ID start position to the last chunk. This explains the deterministic omission. Checked [bazelbuild/remote-apis](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto): RequestMetadata invocation IDs provide correlation; REAPI specifies neither this endpoint nor log pagination. The applicable contract is BuildBuddy's public v1 API.

Commands used `GIT_CONFIG_GLOBAL=/dev/null bb remote test <targets> --config=nobug --test_tag_filters= --nocache_test_results --test_output=all`, with the public GitHub runner fetch remote. [Verification run 1](https://app.buildbuddy.io/invocation/da3cc187-af79-4677-b31a-3178d9806cdc) ran `//tests/app/invocations-api:log_pagination_bug_test` and the regression target; [verification run 2](https://app.buildbuddy.io/invocation/e8912f5e-f334-487b-b251-dfbdd5bc9f27) ran the bug target alone. Both failed at the intended missing-length assertion. An earlier attempted repeat [did not run a test](https://app.buildbuddy.io/invocation/fc66f81c-f942-474e-94e5-65cd600db7b0): a reviewer checkout race captured the BES branch and caused a missing-package build error. It is excluded from the two reproductions.

Medium severity is appropriate because conforming authenticated API consumers silently miss most of a multi-chunk log, impairing build diagnosis. The bytes remain stored; the internal `0000` workaround limits impact, and this is not an authorization bypass or permanent storage loss. No test or shared-harness changes are needed.

## Version history

Verified on 2026-10-13 using the unchanged bug-tagged target `//tests/app/invocations-api:log_pagination_bug_test`
from `origin/area/app-invocations-api` at `2b9f8d922e9e3cf108a2c9bf2d94b1f5829cc30b`.
Tests ran inside disposable fixture-local released containers on hosted runners.

| Release | Published | Result | Bazel invocation | Runner invocation |
| --- | --- | --- | --- | --- |
| enterprise-v2.314.0 | 2026-10-08 | Reproduces | [results](https://app.buildbuddy.io/invocation/ab214fa0-ff83-47c9-9644-59a8b3038d04) | [runner](https://app.buildbuddy.io/invocation/eb049c2d-145f-41cc-81ab-b2990e8c3c2a) |
| enterprise-v2.313.0 | 2026-10-06 | Reproduces | [results](https://app.buildbuddy.io/invocation/6e73827e-18af-442b-9b6f-8c73691a69bd) | [runner](https://app.buildbuddy.io/invocation/db9c3c9c-e3c9-4f4b-8e1e-7a7ac25d9ae4) |
| enterprise-v2.290.0 | 2026-07-29 | Reproduces | [results](https://app.buildbuddy.io/invocation/aacbf37e-1f48-4340-813d-15aed8ee6455) | [runner](https://app.buildbuddy.io/invocation/256d903c-ab1e-4e21-9c3e-853734a3441d) |
| enterprise-v2.275.0 | 2026-06-04 | Reproduces | [results](https://app.buildbuddy.io/invocation/5a6b83d7-3c9e-41c4-bd5a-8ff71cd6d859) | [runner](https://app.buildbuddy.io/invocation/1b9de404-2fca-46e2-9965-535de71d8564) |
| enterprise-v2.260.0 | 2026-04-16 | Reproduces | [results](https://app.buildbuddy.io/invocation/4d91574b-8fda-4cbf-a1b8-0356237b74d0) | [runner](https://app.buildbuddy.io/invocation/fafe222f-5034-4854-b934-fe650f7d8d23) |

Observed in every release marked Reproduces: GetLog returned only 270,217 of 2,250,021 uploaded bytes; the first line was absent and pagination did not recover earlier chunks.

Earliest tested reproducing release: **enterprise-v2.260.0 (2026-04-16)**.
The defect was present by that release; its introduction release is unknown
and may be earlier. These five samples span nearly six months and do not
establish uninterrupted behavior in untested intermediate releases.

No startup/configuration changes were needed. Each execution reached the
reported protocol assertion rather than failing fixture startup or authentication.
The manifest lookup for the initially considered v2.258.0 returned HTTP 404;
v2.260.0 was used as the April sample instead.

Exact linux/amd64 manifest pins resolved from the GCR manifest API:

| Release | Component | Image reference |
| --- | --- | --- |
| enterprise-v2.314.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` |
| enterprise-v2.313.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6` |
| enterprise-v2.290.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:f3b285107f28da495e9496dd848857ce0cf6cddbf3fac8743b259c2a7a8678b6` |
| enterprise-v2.275.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:75ffd43cf6dc14ccf5a652b760d7e4d7390dfa7870524c57e7f354f271e96db1` |
| enterprise-v2.260.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:0f6376865cc9c92d0e837aa1cb53717b59875db68a1b09b6dd479c571f6ab88e` |

Runner checkout used origin/main at `40ac83dc2c3d00efd8993a68e33a2b6df445c8fc`
with the original test-area files applied from their PR branches in a temporary
checkout; this history branch contains only findings. The reproducible command
for the current release is (replace image references with the row for an older release):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/invocations-api:log_pagination_bug_test \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image='gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843'
```

Each linked invocation ran all 12 in-scope reproducers together, with explicit
app, executor, and cacheproxy digest overrides; unused components were not started.
The final bug filter overrides the default nobug exclusion, and test-result
caching was disabled. Container configs, logs, and inspect metadata are retained
in the invocations' undeclared test outputs.
