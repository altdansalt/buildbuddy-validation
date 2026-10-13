# FetchBlob ignores freshness constraints on checksum cache hits

Title: Checksum cache hits bypass oldest_content_accepted
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Clients cannot force revalidation/retrieval of cached assets using the protocol's freshness lower bound. This affects origin freshness and availability checks, but the tested SHA256-qualified bytes are identical and no content corruption or cross-group access was demonstrated.
Status: confirmed
Repro test target: //tests/app/remote-asset:freshness_bug
BuildBuddy invocation: https://app.buildbuddy.io/invocation/58a062aa-636e-4daa-bb52-6ee1bcad8657

## Expected behavior

The [pinned Remote Asset API](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto) permits reuse of cached fetched content subject to `oldest_content_accepted`. That field is the oldest accepted time measured when content was pushed or its underlying origin retrieval started.

After fetching a SHA256-qualified blob, wait until after the completed retrieval and set `oldest_content_accepted` to the current timestamp. The next successful FetchBlob must retrieve content satisfying the new lower bound. For this implementation and a test-owned HTTP origin with no Push association, expect a second origin request.

## Actual behavior

The initial fetch returns OK and produces exactly one origin request. The second fetch also returns OK, but there is still exactly one origin request: the older checksum cache entry was returned without consulting the origin.

Two independent uncached runs reproduce the request count of 1 instead of 2:

- [Run 1](https://app.buildbuddy.io/invocation/58a062aa-636e-4daa-bb52-6ee1bcad8657), 2026-10-12 23:45 UTC.
- [Run 2](https://app.buildbuddy.io/invocation/a2884544-035e-499c-8511-731b7b66267f), 2026-10-12 23:46 UTC.

The test uses an atomic counter in its own HTTP handler. The timestamp is generated after the first successful response plus a 20 ms delay, using the same hosted runner clock as the containers. Each test has a new fixture and fresh cache volumes. A separate successful Fetch-to-CAS preflight rules out startup/internal-client errors and is excluded from the origin counter. The initial request and both successful status assertions pass before the failing count assertion.

## Logs excerpt

```text
origin requests after freshness-constrained fetch: 1
expected: int(2)
actual  : int32(1)
Messages: content retrieved before oldest_content_accepted must be fetched again
```

## Notes

The [released FetchBlob implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_asset/fetch_server/fetch_server.go) calls `cachedBlobResponse()` and returns a checksum cache hit before examining `oldest_content_accepted` when deciding whether to coalesce origin work. The cache lookup does not check an origin retrieval timestamp. This reproducer specifically covers the checksum-cache path, not every freshness mode.

Only the app component is started, with the harness's local auth, SQLite, Pebble, and Redis defaults. The only config override is `remote_asset.allowed_private_ips` for the test-owned origin's runner interface `/32`. The SHA256 SRI is valid and both requests have the same URI and checksum. There are no image overrides, remote Push calls, or shared service requests. The harness preserves configs, container logs, and inspect metadata in undeclared outputs.

Reproduce with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/remote-asset:freshness_bug --test_tag_filters=bug --nocache_test_results --test_output=all
```

Outer runner invocations: [run 1](https://app.buildbuddy.io/invocation/6f3c9072-6043-4526-a97e-8a3303a8ccfb), [run 2](https://app.buildbuddy.io/invocation/f078ea84-c8f1-4067-97d0-0f9bbb767066). Independent verification is recorded below. Findings stay in this repository.


## Verification

Independent review of PR #24 on 2026-10-12: **confirmed**. Reviewed the PR's
bug-tagged test and helper at commit `5d92e9a`, then ran both unchanged targets
twice on hosted runners against the default digest-pinned enterprise-v2.314.0
app image recorded above. Each run created fresh app/cache volumes and a
runner-local HTTP origin; no component requests used shared deployments.
The successful Fetch-to-CAS preflight and local API key rule out startup,
network allowlist, authentication, and cache configuration failures. Only these
two finding files are copied from PR #24 into this verification branch; the
reproducers remain in PR #24 until it merges.

Verification command (run twice, with the public runner fetch remote configured):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/remote-asset:origin_status_bug //tests/app/remote-asset:freshness_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

The final empty tag filter overrides `nobug`'s exclusion; both bug targets
actually executed, with test-result caching disabled.

- [Verification run 1](https://app.buildbuddy.io/invocation/e99a89c8-2fde-4a63-ba31-44be0d642c03)
  ([outer runner](https://app.buildbuddy.io/invocation/2cfedf19-706f-4673-b1da-38f8e0cabe62)).
- [Verification run 2](https://app.buildbuddy.io/invocation/c9804b6b-364f-4da1-86ca-dadd4c9d8f29)
  ([outer runner](https://app.buildbuddy.io/invocation/1bfe4d0e-931e-4f47-b9c5-cbe14e427969)).

Both runs passed the initial status and one-request assertions, then passed the
second OK-status assertion and failed only the request count (1 instead of 2).
The preflight path is excluded from the atomic counter. The bound is generated
after the first fetch completes plus 20 ms on the same kernel clock used by the
container, so neither overlapping origin work nor clock skew explains the hit.
There is no Push call, concurrent fetch, or HTTP intermediary in this test.

The [Remote Asset Fetch contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto#L90-L103)
permits cache reuse explicitly subject to oldest_content_accepted;
[the field definition](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto#L177-L185)
measures age from Push or the start of origin retrieval. A checksum constrains
bytes; it does not waive this independent age constraint. CAS access-time renewal
is not a new origin retrieval and cannot satisfy the newer bound. With no Push
or other retrieval in the fixture, a second successful response requires a new
origin request; identical immutable bytes do not invalidate this assertion.

The [v2.314.0 checksum-cache path](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_asset/fetch_server/fetch_server.go#L300-L336)
returns cachedBlobResponse before checking GetOldestContentAccepted. The later
freshness check only disables coalescing; findBlobInCache renews CAS presence
without checking origin age. This directly explains the repeatable observation.
The released [cache configuration docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md)
and [enterprise cache setup docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-cache-setup.md)
do not document a checksum exception to Remote Asset freshness. Their cache
access-time/TTL guidance is about storage retention, not origin retrieval age.

Severity remains **low**: the freshness contract is violated, but the validated
checksum selects the same bytes and the test demonstrates no corruption or
security boundary violation. This finding covers checksum cache hits, not all
freshness handling.

## Version history

Verified on 2026-10-13 using the unchanged bug-tagged target `//tests/app/remote-asset:freshness_bug`
from `origin/area/app-remote-asset` at `5d92e9a5576224c08c8319e3c1ee65f23615ee38`.
Tests ran inside disposable fixture-local released containers on hosted runners.

| Release | Published | Result | Bazel invocation | Runner invocation |
| --- | --- | --- | --- | --- |
| enterprise-v2.314.0 | 2026-10-08 | Reproduces | [results](https://app.buildbuddy.io/invocation/ab214fa0-ff83-47c9-9644-59a8b3038d04) | [runner](https://app.buildbuddy.io/invocation/eb049c2d-145f-41cc-81ab-b2990e8c3c2a) |
| enterprise-v2.313.0 | 2026-10-06 | Reproduces | [results](https://app.buildbuddy.io/invocation/6e73827e-18af-442b-9b6f-8c73691a69bd) | [runner](https://app.buildbuddy.io/invocation/db9c3c9c-e3c9-4f4b-8e1e-7a7ac25d9ae4) |
| enterprise-v2.290.0 | 2026-07-29 | Reproduces | [results](https://app.buildbuddy.io/invocation/aacbf37e-1f48-4340-813d-15aed8ee6455) | [runner](https://app.buildbuddy.io/invocation/256d903c-ab1e-4e21-9c3e-853734a3441d) |
| enterprise-v2.275.0 | 2026-06-04 | Reproduces | [results](https://app.buildbuddy.io/invocation/5a6b83d7-3c9e-41c4-bd5a-8ff71cd6d859) | [runner](https://app.buildbuddy.io/invocation/1b9de404-2fca-46e2-9965-535de71d8564) |
| enterprise-v2.260.0 | 2026-04-16 | Reproduces | [results](https://app.buildbuddy.io/invocation/4d91574b-8fda-4cbf-a1b8-0356237b74d0) | [runner](https://app.buildbuddy.io/invocation/fafe222f-5034-4854-b934-fe650f7d8d23) |

Observed in every release marked Reproduces: The freshness-constrained second fetch reused stale content; the local origin request count remained 1 instead of 2.

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
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/remote-asset:freshness_bug \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image='gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843'
```

Each linked invocation ran all 12 in-scope reproducers together, with explicit
app, executor, and cacheproxy digest overrides; unused components were not started.
The final bug filter overrides the default nobug exclusion, and test-result
caching was disabled. Container configs, logs, and inspect metadata are retained
in the invocations' undeclared test outputs.
