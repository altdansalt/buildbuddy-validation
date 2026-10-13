# ActionCache accepts and returns negative output file sizes

Title: Negative OutputFile digest size bypasses ActionResult validation
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: An authenticated cache writer can store malformed results that other clients receive as cache hits. The output digest is impossible to retrieve through valid CAS requests and may trigger client errors. This demonstrates cache integrity loss within the writer's cache namespace, not cross-group access or a server crash.
Status: confirmed
Repro test target: //tests/app/action-cache:malformed_result_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/e65038da-417c-46e4-b86a-e2cb7af6504e

## Expected behavior

[REAPI Digest](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) defines size_bytes as the size of the blob in bytes; a negative value cannot describe a blob. UpdateActionResult documents `INVALID_ARGUMENT` for invalid arguments. The app should reject an ActionResult containing a negative OutputFile digest size with `INVALID_ARGUMENT` and should not serve it as a successful cache hit.

[Pinned digest validation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go) explicitly rejects negative sizes. Regression coverage verifies that this validation applies to negative top-level action digests on both Get and Update.

## Actual behavior

After uploading a valid Command, input root and Action, UpdateActionResult accepts an OutputFile with a valid SHA256 hash and size_bytes=-1. GetActionResult returns the same malformed file digest successfully. No output blob was uploaded or could have the stated size.

Reproduced in two independent uncached runs: https://app.buildbuddy.io/invocation/e65038da-417c-46e4-b86a-e2cb7af6504e and https://app.buildbuddy.io/invocation/ca7034c0-4281-469b-822e-2628e8228a38. These use only the pinned fixture-local app and default config. Positive-size missing output files are rejected on read by the regression test, and top-level negative action digest requests are rejected with `INVALID_ARGUMENT`; the defect is specific to nested ActionResult validation rather than a harness transport or authentication issue.

## Logs excerpt

```text
--- FAIL: TestNegativeOutputDigest
GetActionResult: result=output_files:{path:"out" digest:{hash:"e0ee8bb50685e05fa0f47ed04203ae953fdfd055f5bd2892ea186504254f8c3a" size_bytes:-1}} execution_metadata:{worker:"3fd2e389-0ca7-48ce-a785-3f50d9d6a39b"} error=<nil>
expected: 0x3  (INVALID_ARGUMENT)
actual  : 0x0  (OK)
UpdateActionResult accepted a negative output size
```

These are RPC assertion logs; full container logs and inspect metadata are preserved in the harness's undeclared outputs.

## Notes

Default harness config: SQLite, local Pebble cache, self-auth with a disposable local group/key, no config or image overrides. Only app plus its fixture-local Redis sidecar starts.

[Pinned ActionCache source](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go) validates the action digest but serializes the ActionResult without validating nested output digests. On read, ValidateActionResult only adds output file digests with size_bytes > 0 to its CAS availability check, so a negative size bypasses that check. Rejecting malformed nested digests at update time and validating them on read would prevent this result from being served.

Final test revision also reproduced the same failure: https://app.buildbuddy.io/invocation/44f004cb-dcdc-4fca-b299-0e762ec6358b.

Regression invocation: https://app.buildbuddy.io/invocation/6dfd75ac-653f-444e-ae77-d807345cfe9a. No server crash or client exploitation is claimed. Findings remain in this repository; no upstream report was filed.


## Verification

Independent review on 2026-10-12: **confirmed**. Reviewed PR #12 at
`dc2fa7b3f2870f963478030af4b1d10fea9b92b9`, including the bug target,
`helpers_test.go`, regression coverage, and harness configuration/client routing.
The helper uploads the Command, input Directory and Action before updating the
result, satisfying the REAPI upload prerequisite. RPCs use the fixture-local
app and disposable authenticated key; there are no image/config overrides or
shared component endpoints. Both runs logged the pinned app digest stated above.

Independent uncached runs (both bug targets executed in each):

- [Run 1, Bazel results](https://app.buildbuddy.io/invocation/7a7d1e15-87d0-4093-b82e-f5696c013545)
  ([runner](https://app.buildbuddy.io/invocation/2e28459b-0030-43a6-bde4-182c16e7400e)).
  Both reproducers failed at their intended RPC assertions; the action-cache
  regression target passed, including missing-file/Tree checks, repair after
  upload, valid CAS/AC operations, and top-level invalid-digest rejection.
- [Run 2, Bazel results](https://app.buildbuddy.io/invocation/2c5c2c9b-d8a7-4604-8293-669208f00834)
  ([runner](https://app.buildbuddy.io/invocation/10b9c2d6-cdab-4142-a824-583b9f626ab9)).
  Both reproducers again failed at the same assertions with fresh fixtures.

Commands used public GitHub for runner checkout and
`GIT_CONFIG_GLOBAL=/dev/null bb remote test
//tests/app/action-cache:missing_streams_test
//tests/app/action-cache:malformed_result_test --config=nobug
--test_tag_filters= --nocache_test_results --test_output=all`.
Run 1 additionally selected `//tests/app/action-cache:go_default_test`.
The empty tag filter overrides the default bug exclusion; the logs verify the
bug targets actually ran. Test execution was not cached. The repeated precise
RPC outcomes and passing controls rule out a startup, auth, CAS outage, or
flakiness explanation.

[REAPI Digest](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1146-L1184)
says the size is inseparable from the hash and the server MUST reject a request
with an incorrect size, even if the hash is correct. A negative size cannot
describe any blob. [UpdateActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L175-L194)
lists INVALID_ARGUMENT for invalid arguments. Accepting missing valid blobs at
update time is a separate policy and does not justify accepting an impossible
digest. Both runs accepted the update and returned the exact size_bytes=-1
output with OK on read.

[Pinned UpdateActionResult](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go#L360-L419)
validates the action digest but marshals nested result digests without validating
them. [ValidateActionResult](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go#L153-L194)
only enqueues file digests with size greater than zero, so this malformed output
also bypasses read-time checking. [digest.Validate](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go#L205-L216)
explicitly rejects negative sizes. The release is source commit
`974929724a4c75f824ab18fbcb560e48bbf2803a`.

Reviewed the pinned [enterprise cache setup](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-cache-setup.md)
and [missing-digest troubleshooting](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/troubleshooting-rbe.md#L74-L115).
They support the Pebble configuration and discuss missing/evicted blobs, but
provide no intended negative-size semantics. This is nested validation loss,
not a supported empty-file representation or an eviction/configuration issue.

The test accepts update-time INVALID_ARGUMENT as success; otherwise it logs
the subsequent read and asserts INVALID_ARGUMENT there. BuildBuddy currently
maps failed read-time validation to NOT_FOUND, so a future read-only mitigation
would still fail this test. That does not invalidate this reproduction: the
negative-size update itself violates the Digest contract, and the observed read
is OK with the malformed digest. The existing write-up accurately identifies
both outcomes; no needs-work verdict is necessary for the released behavior.

Severity remains **medium** for cache integrity and potential downstream client
errors within a writer-authorized namespace. A writable cache already permits
writers to publish incorrect outputs, so this is not evidence of a new privilege
boundary crossing; no client exploit or server crash was demonstrated.

## Version history

Tested on 2026-10-13 against five released linux/amd64 app images, spanning
2026-04-16 through 2026-10-08. The unchanged bug-tagged target
`//tests/app/action-cache:malformed_result_test` from PR branch
`area/app-action-cache` at `dc2fa7b3f2870f963478030af4b1d10fea9b92b9` ran once per release
with fresh fixture-local containers and test-result caching disabled.

| Enterprise release | Release date | GCR manifest digest | Result | BuildBuddy invocation |
| --- | --- | --- | --- | --- |
| 2.314.0 | 2026-10-08 | `sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/b3d99ba0-c97f-4a8f-8902-47173fc5a5a2) ([runner](https://app.buildbuddy.io/invocation/03f4be89-f916-448e-94e4-a6e8c1ee27e8)) |
| 2.313.0 | 2026-10-06 | `sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/7ca24178-567a-4e9b-8672-72cb61c1f01b) ([runner](https://app.buildbuddy.io/invocation/55ac5262-0b2a-4c38-9949-429b15ca9da0)) |
| 2.300.0 | 2026-09-01 | `sha256:03bd9ccb5888936beb301a577bf01560511cded6fa909079cbb53277aa32ed68` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/4caa1a54-2d1c-40a8-9dbd-deecfc37df94) ([runner](https://app.buildbuddy.io/invocation/1ce360d4-0117-4c96-a458-6ce2f1f627bc)) |
| 2.280.0 | 2026-06-23 | `sha256:d9fbae912f786a0f95d351ffb7c580a6ecb8b1fb636fd3fe8899bf1cb8350a35` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/f9ddf7a7-cd2e-4178-a233-32595ab3c2db) ([runner](https://app.buildbuddy.io/invocation/9e2de6d0-0266-4933-a947-122785bcd893)) |
| 2.260.0 | 2026-04-16 | `sha256:0f6376865cc9c92d0e837aa1cb53717b59875db68a1b09b6dd479c571f6ab88e` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/afac6fba-dc7f-4160-bb0e-77d63bf0b565) ([runner](https://app.buildbuddy.io/invocation/55d2d116-6299-4b5f-b905-6a68769b6a36)) |

All image references use
`gcr.io/flame-public/buildbuddy-app-enterprise@<digest>`; digests were resolved
from the GCR manifest API for the corresponding `enterprise-v<version>` tags.
The 2.314.0 digest matches the repository default.

Observed on every tested version: UpdateActionResult accepts size_bytes=-1 and GetActionResult returns the same negative output size with OK.

**Earliest tested release reproducing: 2.260.0 (2026-04-16).**
The defect was present by that release; the introduction version is unknown.
All sampled releases reproduce, so this matrix does not establish an
introduction interval or imply every intervening release was tested.

No older release required startup configuration changes. All five reached
the intended product assertion after fixture startup and authentication.

Command pattern (run from the repro branch; substitute the digest above):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote --runner_exec_properties=init-dockerd=true test \
  //tests/app/action-cache:malformed_result_test \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image=gcr.io/flame-public/buildbuddy-app-enterprise@<digest>
```
