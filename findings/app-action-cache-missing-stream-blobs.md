# ActionCache returns results referencing missing stdout and stderr blobs

Title: GetActionResult skips availability checks for stdout_digest and stderr_digest
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: An authenticated cache writer can publish a result that is returned as a cache hit even though its stdout or stderr cannot be downloaded. Normal CAS eviction can also expose this inconsistency. Clients may lose build diagnostics or fail fetching cached output; no cross-group access or code execution is demonstrated.
Status: confirmed
Repro test target: //tests/app/action-cache:missing_streams_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/e65038da-417c-46e4-b86a-e2cb7af6504e

## Expected behavior

[REAPI ActionCache.GetActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) says implementations SHOULD ensure any referenced CAS blobs are available when returning an ActionResult. This includes stdout_digest and stderr_digest. A missing referenced blob should cause a cache miss (`NOT_FOUND`), matching this release's handling of missing output files and Trees.

This is a SHOULD-level conformance and consistency finding; it does not claim REAPI requires UpdateActionResult to check blob existence, nor that inline_stdout/inline_stderr hints must be honored.

## Actual behavior

The test uploads a valid Command, input root and Action, then updates the result with only a stdout_digest or stderr_digest whose nonempty CAS blob has never been uploaded. A direct ByteStream read verifies `NOT_FOUND` for that digest. GetActionResult nonetheless succeeds and returns the dangling digest in both subtests.

Reproduced in two independent uncached runs: https://app.buildbuddy.io/invocation/e65038da-417c-46e4-b86a-e2cb7af6504e and https://app.buildbuddy.io/invocation/ca7034c0-4281-469b-822e-2628e8228a38. Both ran the pinned released app in disposable fixture-local containers. Authentication, CAS and AC writes work, and regression coverage rejects missing output files, missing Trees, and missing root/child file blobs and succeeds after missing files are uploaded. This rules out a general CAS outage or authentication/configuration issue.

## Logs excerpt

```text
--- FAIL: TestMissingStreamBlobs/stdout
expected: 0x5  (NOT_FOUND)
actual  : 0x0  (OK)
dangling stdout reference returned: stdout_digest:{hash:"3d075b6e17a8d1a60b2da61ece9f947e690037b97e0ac2c677e9691c3b6132c6" size_bytes:21}
--- FAIL: TestMissingStreamBlobs/stderr
expected: 0x5  (NOT_FOUND)
actual  : 0x0  (OK)
dangling stderr reference returned: stderr_digest:{hash:"aab671e5da0116aef1230f7d56776c6f1a91441517f7be577ed9a057d97db851" size_bytes:21}
```

These are RPC assertion logs; full container logs and inspect metadata are preserved by the harness in the test's undeclared outputs.

## Notes

Default harness config: SQLite, local Pebble cache, self-auth with a disposable local group/key, no config or image overrides. Only app plus its fixture-local Redis sidecar starts; no executor or shared deployment is involved.

[Pinned ValidateActionResult source](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go) appends OutputFile digests and traverses output Trees but never adds stdout_digest or stderr_digest to the availability check. Extending that check is the likely fix. Results with inline raw streams would need an explicit policy to avoid rejecting self-contained output.

Final test revision also reproduced the same failure: https://app.buildbuddy.io/invocation/44f004cb-dcdc-4fca-b299-0e762ec6358b.

Regression invocation: https://app.buildbuddy.io/invocation/6dfd75ac-653f-444e-ae77-d807345cfe9a. Findings remain in this repository; no upstream report was filed.


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

[REAPI GetActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L158-L174)
recommends availability of **any** referenced CAS blob; ActionResult explicitly
defines stdout_digest and stderr_digest as CAS references. This is a SHOULD,
not a MUST, and NOT_FOUND for a dangling reference follows BuildBuddy's existing
file/Tree policy rather than an independently mandatory REAPI error mapping.
The test never requests stream inlining and sets no raw stream data, so the
permission to ignore inline hints or return self-contained raw data does not
explain the hit. Its direct ByteStream control returns NOT_FOUND before each
successful GetActionResult; both runs return the missing stdout and stderr
references unchanged.

[Pinned source, ValidateActionResult and fetchActionResult](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go#L153-L252)
checks OutputFiles and files in output Trees, but never enqueues stdout_digest
or stderr_digest. This explains the selective failure without relying on a
race or a misconfigured cache. The release is source commit
`974929724a4c75f824ab18fbcb560e48bbf2803a`.

Reviewed the pinned [enterprise cache setup](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-cache-setup.md)
and [missing-digest troubleshooting](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/troubleshooting-rbe.md#L74-L115).
Pebble is a documented enterprise backend. The troubleshooting guide explains
client assumptions and eviction, but does not document an intentional exception
for stream availability checks. Eviction can cause later reads to fail; here
the stream is demonstrably absent before the cache hit, while equivalent file
references are checked. Confirmed as the bounded SHOULD-level consistency
defect described in the report, not a mandatory-protocol violation.

Severity remains **medium**: broken cached diagnostics and potential fetch
failures affect build reliability. Ordinary eviction is a plausible route,
but these runs reproduce a never-uploaded blob, not an eviction scenario.
No cross-group access, server crash, or execution compromise was demonstrated.

## Version history

Verified on 2026-10-13 using the unchanged bug-tagged target `//tests/app/action-cache:missing_streams_test`
from `origin/area/app-action-cache` at `dc2fa7b3f2870f963478030af4b1d10fea9b92b9`.
Tests ran inside disposable fixture-local released containers on hosted runners.

| Release | Published | Result | Bazel invocation | Runner invocation |
| --- | --- | --- | --- | --- |
| enterprise-v2.314.0 | 2026-10-08 | Reproduces | [results](https://app.buildbuddy.io/invocation/ab214fa0-ff83-47c9-9644-59a8b3038d04) | [runner](https://app.buildbuddy.io/invocation/eb049c2d-145f-41cc-81ab-b2990e8c3c2a) |
| enterprise-v2.313.0 | 2026-10-06 | Reproduces | [results](https://app.buildbuddy.io/invocation/6e73827e-18af-442b-9b6f-8c73691a69bd) | [runner](https://app.buildbuddy.io/invocation/db9c3c9c-e3c9-4f4b-8e1e-7a7ac25d9ae4) |
| enterprise-v2.290.0 | 2026-07-29 | Reproduces | [results](https://app.buildbuddy.io/invocation/aacbf37e-1f48-4340-813d-15aed8ee6455) | [runner](https://app.buildbuddy.io/invocation/256d903c-ab1e-4e21-9c3e-853734a3441d) |
| enterprise-v2.275.0 | 2026-06-04 | Reproduces | [results](https://app.buildbuddy.io/invocation/5a6b83d7-3c9e-41c4-bd5a-8ff71cd6d859) | [runner](https://app.buildbuddy.io/invocation/1b9de404-2fca-46e2-9965-535de71d8564) |
| enterprise-v2.260.0 | 2026-04-16 | Reproduces | [results](https://app.buildbuddy.io/invocation/4d91574b-8fda-4cbf-a1b8-0356237b74d0) | [runner](https://app.buildbuddy.io/invocation/fafe222f-5034-4854-b934-fe650f7d8d23) |

Observed in every release marked Reproduces: GetActionResult returned OK with never-uploaded stdout/stderr digests; both stream subtests failed.

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
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/action-cache:missing_streams_test \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image='gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843'
```

Each linked invocation ran all 12 in-scope reproducers together, with explicit
app, executor, and cacheproxy digest overrides; unused components were not started.
The final bug filter overrides the default nobug exclusion, and test-result
caching was disabled. Container configs, logs, and inspect metadata are retained
in the invocations' undeclared test outputs.
