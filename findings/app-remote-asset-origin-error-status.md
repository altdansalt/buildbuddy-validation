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

Tested on 2026-10-13 against five released linux/amd64 app images, spanning
2026-04-16 through 2026-10-08. The unchanged bug-tagged target
`//tests/app/remote-asset:origin_status_bug` from PR branch
`area/app-remote-asset` at `5d92e9a5576224c08c8319e3c1ee65f23615ee38` ran once per release
with fresh fixture-local containers and test-result caching disabled.

| Enterprise release | Release date | GCR manifest digest | Result | BuildBuddy invocation |
| --- | --- | --- | --- | --- |
| 2.314.0 | 2026-10-08 | `sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/52c4a356-dc40-4d34-ba0a-b73ea09990e4) ([runner](https://app.buildbuddy.io/invocation/c5a43cce-e183-4b85-83d4-520ee55c1d2e)) |
| 2.313.0 | 2026-10-06 | `sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/03483bef-2c99-41d3-8a56-4b0104d35b44) ([runner](https://app.buildbuddy.io/invocation/479006cc-041e-42f5-acc6-c8a6f87665f6)) |
| 2.300.0 | 2026-09-01 | `sha256:03bd9ccb5888936beb301a577bf01560511cded6fa909079cbb53277aa32ed68` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/bba2fdb3-4368-4afa-b89d-b2dc32e26542) ([runner](https://app.buildbuddy.io/invocation/4541522a-ba20-4f7f-8ed5-6699719eec9a)) |
| 2.280.0 | 2026-06-23 | `sha256:d9fbae912f786a0f95d351ffb7c580a6ecb8b1fb636fd3fe8899bf1cb8350a35` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/16c655fb-4adb-4d69-b683-8fadf4764364) ([runner](https://app.buildbuddy.io/invocation/782a5982-b0b7-41aa-b5b5-0285c18c5017)) |
| 2.260.0 | 2026-04-16 | `sha256:0f6376865cc9c92d0e837aa1cb53717b59875db68a1b09b6dd479c571f6ab88e` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/f8485bde-097b-45b2-866a-af9c9ae41638) ([runner](https://app.buildbuddy.io/invocation/5bdf0bf7-c9c8-4998-8ec0-ddf85e49f5d2)) |

All image references use
`gcr.io/flame-public/buildbuddy-app-enterprise@<digest>`; digests were resolved
from the GCR manifest API for the corresponding `enterprise-v<version>` tags.
The 2.314.0 digest matches the repository default.

Observed on every tested version: All three subtests return inline NOT_FOUND (5): origin timeout instead of DEADLINE_EXCEEDED (4), HTTP 403 instead of PERMISSION_DENIED (7), and checksum mismatch instead of ABORTED (10).

**Earliest tested release reproducing: 2.260.0 (2026-04-16).**
The defect was present by that release; the introduction version is unknown.
All sampled releases reproduce, so this matrix does not establish an
introduction interval or imply every intervening release was tested.

No older release required startup configuration changes. All five reached
the intended product assertion after fixture startup and authentication.
The existing test-owned-origin IP allowlist override was retained, and the
Fetch-to-CAS preflight succeeded before the reproducing requests; transient
self-RPC startup reconnect messages are not counted as defect evidence.

Command pattern (run from the repro branch; substitute the digest above):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote --runner_exec_properties=init-dockerd=true test \
  //tests/app/remote-asset:origin_status_bug \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image=gcr.io/flame-public/buildbuddy-app-enterprise@<digest>
```
