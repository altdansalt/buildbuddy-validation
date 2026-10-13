# FetchBlob reports every origin failure as NOT_FOUND

Title: FetchBlob maps origin timeout, access denial, and checksum mismatch to NOT_FOUND
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: Ordinary Remote Asset clients cannot distinguish retryable origin timeouts from absent assets, access denial, or integrity failures. This can prevent appropriate retry/recovery and obscure consistency failures in builds. No invalid content was observed being accepted; this is an availability and error-contract defect, not demonstrated CAS corruption.
Status: confirmed
Repro test target: //tests/app/remote-asset:origin_status_bug
BuildBuddy invocation: https://app.buildbuddy.io/invocation/58a062aa-636e-4daa-bb52-6ee1bcad8657

## Expected behavior

The [pinned Remote Asset API](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto) explicitly requires inline `DEADLINE_EXCEEDED` when `FetchBlobRequest.timeout` expires retrieving origin content. Its response status documentation identifies `PERMISSION_DENIED` for rejection by the remote server and `ABORTED` for a failed consistency check.

The reproducer supplies a 100 ms origin timeout with a 30 s RPC context to distinguish the fetch timeout from an RPC deadline. Its origin also supplies HTTP 403 and content that mismatches a valid SHA256 SRI qualifier. Expect inline statuses 4, 7, and 10 respectively, with a successful gRPC transport response.

## Actual behavior

All three requests return gRPC OK with inline status 5 (`NOT_FOUND`). The messages retain evidence of deadline expiry, HTTP 403, or rejected digest validation, but the structured code loses those distinctions.

Two independent uncached runs reproduced all three assertions:

- [Run 1](https://app.buildbuddy.io/invocation/58a062aa-636e-4daa-bb52-6ee1bcad8657), 2026-10-12 23:45 UTC.
- [Run 2](https://app.buildbuddy.io/invocation/a2884544-035e-499c-8511-731b7b66267f), 2026-10-12 23:46 UTC.

Both runs first successfully fetch a separate preflight blob and read its contents from the fixture CAS. That rules out a missing cache, bad internal gRPC target, authentication failure, or blocked test-origin networking. Early exploratory runs lacked this preflight and hit the app's startup self-RPC reconnect window; those are not counted as checksum reproductions.

## Logs excerpt

```text
origin_timeout: inline status: code:5 message:"failed to fetch .../slow: HTTP GET failed: deadline exceeded"
expected: codes.Code(0x4), actual: int32(5)
origin_denies_access: inline status: code:5 message:"failed to fetch .../forbidden: HTTP 403 Forbidden"
expected: codes.Code(0x7), actual: int32(5)
checksum_mismatch: inline status: code:5 message:"failed to upload ... to cache: rpc error: code = InvalidArgument desc = Digest of uploaded bytes ... did not match provided digest ..."
expected: codes.Code(0xa), actual: int32(5)
```

## Notes

The [released FetchBlob implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_asset/fetch_server/fetch_server.go) unconditionally constructs a NOT_FOUND response after its URI loop, regardless of the last origin failure. The three subtests exercise this common mapping defect.

Only the app component is started. The harness provisions a local group/key, SQLite, Pebble, and test-local Redis. The HTTP origin is a test-owned listener on the hosted runner, and `remote_asset.allowed_private_ips` includes only its interface `/32`. No image overrides or shared service requests are used. Config, inspect metadata, and container logs are retained by the harness as undeclared test outputs.

Reproduce with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/remote-asset:origin_status_bug --test_tag_filters=bug --nocache_test_results --test_output=all
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

Both runs completed preflight and all three subtests, returning gRPC OK and
inline code 5 for the 100 ms timeout, HTTP 403, and SHA256 mismatch. The timeout
completed in 0.10 s, well within the separate 30 s RPC budget. The mismatch
message identifies the uploaded and expected hashes, ruling out an unrelated
cache write failure. No intermittent assertion or setup failure was observed.

The [Remote Asset spec at the repository's pinned revision](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto#L162-L175)
explicitly requires inline DEADLINE_EXCEEDED for an expired origin timeout;
[FetchBlobResponse.status](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto#L213-L225)
describes PERMISSION_DENIED for origin rejection and ABORTED for consistency
failure. The response list is descriptive rather than a separate MUST for each
mapping, but NOT_FOUND's absent-asset meaning does not describe these cases;
the timeout alone establishes an unambiguous contract violation.

The [v2.314.0 implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_asset/fetch_server/fetch_server.go#L437-L447)
intentionally collapses errors to NOT_FOUND, with a comment questioning the
benefit of PermissionDenied/Aborted. Its
[source tests](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_asset/fetch_server/fetch_server_test.go#L459-L478)
even expect NOT_FOUND on timeout, and checksum tests expect NOT_FOUND as well.
This disproves an accidental implementation regression, but intended behavior
still conflicts with the protocol. The adjacent Bazel <=8 dummy-digest
workaround addresses error-response digest lookup, not the choice of status.
The released [cache configuration docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md)
and [enterprise cache setup docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-cache-setup.md)
provide no exception to this error contract.

Severity remains **medium**: loss of structured retry/access/integrity distinctions
can impair recovery, but the reproducer shows neither accepted invalid content
nor data corruption. It does not demonstrate that every possible origin failure
has this mapping; the confirmed scope is the three exercised cases.

## Version history

Verified on 2026-10-13 using the unchanged bug-tagged target `//tests/app/remote-asset:origin_status_bug`
from `origin/area/app-remote-asset` at `5d92e9a5576224c08c8319e3c1ee65f23615ee38`.
Tests ran inside disposable fixture-local released containers on hosted runners.

| Release | Published | Result | Bazel invocation | Runner invocation |
| --- | --- | --- | --- | --- |
| enterprise-v2.314.0 | 2026-10-08 | Reproduces | [results](https://app.buildbuddy.io/invocation/ab214fa0-ff83-47c9-9644-59a8b3038d04) | [runner](https://app.buildbuddy.io/invocation/eb049c2d-145f-41cc-81ab-b2990e8c3c2a) |
| enterprise-v2.313.0 | 2026-10-06 | Reproduces | [results](https://app.buildbuddy.io/invocation/6e73827e-18af-442b-9b6f-8c73691a69bd) | [runner](https://app.buildbuddy.io/invocation/db9c3c9c-e3c9-4f4b-8e1e-7a7ac25d9ae4) |
| enterprise-v2.290.0 | 2026-07-29 | Reproduces | [results](https://app.buildbuddy.io/invocation/aacbf37e-1f48-4340-813d-15aed8ee6455) | [runner](https://app.buildbuddy.io/invocation/256d903c-ab1e-4e21-9c3e-853734a3441d) |
| enterprise-v2.275.0 | 2026-06-04 | Reproduces | [results](https://app.buildbuddy.io/invocation/5a6b83d7-3c9e-41c4-bd5a-8ff71cd6d859) | [runner](https://app.buildbuddy.io/invocation/1b9de404-2fca-46e2-9965-535de71d8564) |
| enterprise-v2.260.0 | 2026-04-16 | Reproduces | [results](https://app.buildbuddy.io/invocation/4d91574b-8fda-4cbf-a1b8-0356237b74d0) | [runner](https://app.buildbuddy.io/invocation/fafe222f-5034-4854-b934-fe650f7d8d23) |

Observed in every release marked Reproduces: Origin timeout, HTTP 403, and checksum mismatch all returned inline NOT_FOUND (5) after the successful fetch/CAS preflight.

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
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/remote-asset:origin_status_bug \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image='gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843'
```

Each linked invocation ran all 12 in-scope reproducers together, with explicit
app, executor, and cacheproxy digest overrides; unused components were not started.
The final bug filter overrides the default nobug exclusion, and test-result
caching was disabled. Container configs, logs, and inspect metadata are retained
in the invocations' undeclared test outputs.
