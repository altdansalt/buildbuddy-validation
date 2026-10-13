# Malformed digest aborts valid peers in CAS batch RPCs

Title: Malformed digest aborts valid peers in CAS batch RPCs
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Low: one invalid client item prevents independent valid items from receiving their results. This affects batch error recovery and availability within that client request; no shared outage was demonstrated.
Status: confirmed
Repro test target: //tests/app/cas-batch:malformed_digest_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/6d0a9067-8a33-40ec-9c24-4a50f9fc0f61

## Expected behavior

BatchUpdateBlobs requests may succeed or fail independently, with digest mismatches reported as individual INVALID_ARGUMENT. BatchReadBlobs explicitly states that every individual read error is returned in the corresponding digest status. A malformed hash should produce an item error while the valid peer returns OK and its data.

See the [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto), ContentAddressableStorage batch method contracts (consulted 2026-10-12).

## Actual behavior

Both BatchUpdateBlobs and BatchReadBlobs return whole-RPC INVALID_ARGUMENT and no per-item responses when a 12-character malformed hash precedes a valid digest. The valid peer was successfully uploaded independently before both subtests. Both fresh explicit-SHA256 runs fail identically.

Reproductions: [run 1](https://app.buildbuddy.io/invocation/6d0a9067-8a33-40ec-9c24-4a50f9fc0f61) and [run 2](https://app.buildbuddy.io/invocation/4ddafb57-e506-42d3-89ca-8913f4d0edc3), both with `--test_tag_filters=bug --nocache_test_results --test_output=all`. The independent verification below establishes the final status.

Harness/config errors were ruled out by successful authenticated fixture-local uploads and [passing regression coverage](https://app.buildbuddy.io/invocation/2cf13f8d-54f6-454f-9524-8b204d42d7ee), including ordinary per-item hash/size mismatch handling. Each test starts only the released app with the default local SQLite/Pebble and self-auth setup; no external component endpoint or host dispatch key is used. The errors match the [released implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go).

## Logs excerpt

Test-log excerpts (container logs/configs/inspect metadata are preserved by the harness in undeclared test outputs):

```text
--- FAIL: TestMalformedDigestIsPerItemFailure/update
rpc error: code = InvalidArgument desc = Invalid length hash. Expected len 64 for SHA256 function. Got 12
--- FAIL: TestMalformedDigestIsPerItemFailure/read
rpc error: code = InvalidArgument desc = Invalid length hash. Expected len 64 for SHA256 function. Got 12
```

## Notes

The released implementation returns rn.Validate() errors directly from both per-item loops. Return validation failures in the item status and continue processing valid peers. Requests explicitly select SHA256, use a positive size, and remain far below batch limits. The malformed hash is intentional input, not fixture configuration.

No image overrides or custom app flags/configs. Fixture starts its local Redis sidecar. Reproduce using the public fetch remote and `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-batch:malformed_digest_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all`. Findings remain in this repository; no upstream report.

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

The [REAPI batch contracts](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L363)
describe independent individual requests, with per-item upload digest-mismatch
errors and explicitly every individual read error in its digest status. A bad
hash belongs to one item; it is not an invalid shared instance, unsupported
request-wide digest function or excessive batch size. Even if the upload wording
is read as less prescriptive, the read contract explicitly forbids this outcome.
The test explicitly requests supported SHA256, successfully uploads its valid
peer through the fixture's Upload helper before the subtests, and uses tiny
positive sizes. Both update and read return whole-RPC INVALID_ARGUMENT with
"Expected len 64 ... Got 12" on both runs.

The [released update loop](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L274)
and [read loop](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L442)
return rn.Validate errors directly, rather than adding an individual response.
The [validator](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go#L205)
explains the precise length error. Validation itself is correct; its placement
at the RPC level violates the batch error contract. No timing-sensitive operation
explains the repeated failures. Retain **low** severity: request-local loss of
independent results, without evidence of a shared availability failure.
