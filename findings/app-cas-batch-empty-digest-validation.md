# BatchUpdateBlobs accepts nonempty data for the empty digest

Title: BatchUpdateBlobs accepts nonempty data for the empty digest
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Low: malformed uploads receive false success and their supplied bytes are discarded. No persisted cache corruption or cross-group effect was demonstrated. Requires a client to submit inconsistent data.
Status: confirmed
Repro test target: //tests/app/cas-batch:empty_digest_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/6d0a9067-8a33-40ec-9c24-4a50f9fc0f61

## Expected behavior

BatchUpdateBlobs must report per-item INVALID_ARGUMENT when the digest does not match the supplied data. The SHA-256 empty digest with size zero does not describe the supplied bytes `not empty`. A valid peer should still succeed.

See the [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto), ContentAddressableStorage batch method contracts (consulted 2026-10-12).

## Actual behavior

The RPC succeeds and returns per-item OK for the empty digest despite nonempty data. The test submits this alongside a valid upload. Both fresh runs return expected code 3 versus actual code 0.

Reproductions: [run 1](https://app.buildbuddy.io/invocation/6d0a9067-8a33-40ec-9c24-4a50f9fc0f61) and [run 2](https://app.buildbuddy.io/invocation/4ddafb57-e506-42d3-89ca-8913f4d0edc3), both with `--test_tag_filters=bug --nocache_test_results --test_output=all`. The independent verification below establishes the final status.

Harness/config errors were ruled out by successful authenticated fixture-local uploads and [passing regression coverage](https://app.buildbuddy.io/invocation/2cf13f8d-54f6-454f-9524-8b204d42d7ee), including ordinary per-item hash/size mismatch handling. Each test starts only the released app with the default local SQLite/Pebble and self-auth setup; no external component endpoint or host dispatch key is used. The errors match the [released implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go).

## Logs excerpt

Test-log excerpts (container logs/configs/inspect metadata are preserved by the harness in undeclared test outputs):

```text
--- FAIL: TestEmptyDigestRejectsNonemptyData
expected: 3
actual  : 0
response: digest:{hash:"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"} status:{}
```

## Notes

The released implementation checks rn.IsEmpty() before checksum and size validation and skips storing these bytes. Validate bytes before the empty-blob optimization. The digest function is inferred from a valid 64-character SHA-256 hash; the same inference works in passing normal mismatch tests.

No image overrides or custom app flags/configs. Fixture starts its local Redis sidecar. Reproduce using the public fetch remote and `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-batch:empty_digest_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all`. Findings remain in this repository; no upstream report.

## Verification

Independent review on 2026-10-12: **confirmed** against the default app image
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
(enterprise-v2.314.0). Reviewed PR #11 at
`8b51635f87dae3bf3c7ed2cc7221f40cc2312901`, including the bug tests and shared
fixture. Both targets were executed twice on hosted runners with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-batch:empty_digest_bug_test //tests/app/cas-batch:malformed_digest_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Independent test invocations: [run 1](https://app.buildbuddy.io/invocation/97ee6317-a677-4a5c-910c-8c83e3e4d2b4)
and [run 2](https://app.buildbuddy.io/invocation/f8841db5-dcf7-4907-8bd2-a6b70ae59654).
Both targets ran uncached and failed at their intended assertions both times,
not during setup or readiness. Runner invocations:
[run 1](https://app.buildbuddy.io/invocation/945c638c-e513-438e-8cfd-958e9abd0aaf),
[run 2](https://app.buildbuddy.io/invocation/1dc3d306-9f36-41f0-8b2f-7391e3a36171).

The fixture starts an isolated local app with SQLite/Pebble, local Redis and
self-auth. `harness/fixture.go` provisions a CACHE_WRITE key, and
`harness/clients.go` attaches it to RPCs. This rules out the released app's
intentional anonymous-blackhole/read-only-key fake-success branches. No image,
server flag, endpoint or credential override was used. The
[released enterprise cache documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-cache-setup.md)
and [cache configuration documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md)
do not document an exception to these batch contracts.

The [REAPI CAS contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L342)
requires empty blobs to be available without upload. That permits skipping storage,
not accepting nonempty data whose digest does not match. BatchUpdateBlobs specifies
individual INVALID_ARGUMENT for data/digest mismatch. The test uses identity
compression, a valid SHA-256 empty digest and an ordinary valid peer, with no
batch-size issue. Omitting digest_function is permitted for SHA-256; the
[released digest helper](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go#L123)
infers it from the hash. In both runs the empty item returns OK instead of
INVALID_ARGUMENT (expected 3, actual 0).

The [released BatchUpdateBlobs implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L274)
checks IsEmpty and continues before validating the supplied data's checksum or
length. This deterministic path explains the result without a race or cache-state
dependency. The test stops at the bad item's assertion, so these review runs do
not independently establish the later peer's status; that does not weaken the
observed empty-item defect. Retain **low** severity: false success and discarded
malformed input, with no demonstrated stored corruption or wider impact.
