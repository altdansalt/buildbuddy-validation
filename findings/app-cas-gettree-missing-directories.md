# GetTree aborts partial trees and returns the wrong missing-root status

Title: GetTree aborts partial trees and returns the wrong missing-root status
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cache proxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: medium
Severity justification: A single absent child Directory prevents enumeration of all available content, which can occur after CAS eviction. Clients lose the specified partial-tree behavior, and the wrong root error can interfere with cache-miss handling. No elevated permissions or unusual configuration are required.
Status: confirmed
Repro test target: //tests/app/cas-gettree:bugs_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/671e4675-2fac-4a15-ac91-f8364045b7b2

## Expected behavior

[REAPI GetTree](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) explicitly requires: "If part of the tree is missing from the CAS, the server will return the portion present and omit the rest." A stored root referencing one absent child must therefore return the root and omit that child. If the root itself is absent, the documented status is `NOT_FOUND`.

## Actual behavior

Both app and proxy return `FailedPrecondition` for a missing child and emit no available root Directory. A missing root also returns `FailedPrecondition` instead of `NotFound`.

The fixed minimal cases reproduced independently in [run 1](https://app.buildbuddy.io/invocation/671e4675-2fac-4a15-ac91-f8364045b7b2) and [run 2](https://app.buildbuddy.io/invocation/5daad1ab-69d3-4d8d-b8b3-97dd6d8224b6), on both endpoints. The seeded precursor also reproduced missing-child errors across SHA256/SHA1 in [this invocation](https://app.buildbuddy.io/invocation/6638a982-d198-412b-a7c9-ec763707d92f). Independent verification is recorded below.

## Logs excerpt

```text
TestMissingDirectories/app/missing-root:
missing root: got rpc error: code = FailedPrecondition desc = Digest hash:"6bc8e1dd01755ed12dfd69c122f0e45f15dc0e595b361d8cf2c0432822a72fb0" size_bytes:12 not found want NotFound
TestMissingDirectories/app/missing-child:
missing child must be omitted, retaining root: rpc error: code = FailedPrecondition desc = Digest hash:"514b5c79c96318fd6892d61a7af2fd931281f32bf32fd4581f5a73fdc3dd4550" size_bytes:16 not found
```

The corresponding cacheproxy subtests produce the same statuses. Full container logs/configs/image metadata are preserved in the undeclared test outputs.

## Notes

Default harness configuration only: authenticated fixture-local app with SQLite, Pebble, Redis, and cache proxy forwarding to that app. No image/config overrides. The test successfully uploads the root Directory using BatchUpdateBlobs; the referenced digest is deliberately never uploaded. Passing nonempty-tree regression cases validate the same clients and fixture configuration through both endpoints, ruling out auth/connectivity/setup errors.

The [released CAS server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go) maps missing roots to `digest.MissingDigestError` in `fetchDir`. `fetchDirectory` similarly returns that error for any absent child, and GetTree propagates it before sending the collected Directory messages. These paths explain both observed errors.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:bugs_test --test_tag_filters=bug --nocache_test_results --test_filter=TestMissingDirectories --test_output=all
```

Findings remain in this repository; no upstream report was sent.


## Verification

Independent review on 2026-10-13: **confirmed**. Reviewed PR #75 at
`fbf895672569664e0d17e4bbccab4737a5febf11`, including `bugs_test.go` and its
upload, receive, and digest-set comparison helpers. Both independent uncached
runs below executed all three bug tests against the exact app/proxy digests
listed above, without image overrides. Each case failed on both app and
cacheproxy with the reported assertions, rather than readiness, authentication,
upload, timeout, or connection errors. The first run also passed
`//tests/app/cas-gettree:go_default_test`, exercising the same fixture and clients
with seeded nonempty SHA256/SHA1 trees (seed 314159).

The open PR predates main's Redis registry correction. To avoid its unqualified
Redis image, verification used an isolated checkout of `origin/main` at
`be8c4484824a2a7338eb3667527e6abc92c637ef` with only the unchanged
`tests/app/cas-gettree/` files copied from PR #75. Main's harness uses the same
pinned app/proxy images and mirror.gcr.io Redis. An initial PR-checkout dispatch
was stopped and is not counted as verification. No test or harness changes are
included in this review PR.

```sh
# Run 1: all three reproducers, plus the nonempty-tree control.
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:bugs_test //tests/app/cas-gettree:go_default_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
# Run 2: all three reproducers again, with a fresh fixture per test.
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:bugs_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Independent run 1: Bazel results](https://app.buildbuddy.io/invocation/36c82701-6f52-49ee-8db6-e4b0c048bb16), [runner](https://app.buildbuddy.io/invocation/bbf1f824-0c20-4cf0-a2e0-f59492f11aec).
- [Independent run 2: Bazel results](https://app.buildbuddy.io/invocation/947a7a6a-e421-4b15-b9db-2b5a43cbb8ec), [runner](https://app.buildbuddy.io/invocation/29f45463-c822-45e6-988f-33f417986c9b).

The [released proxy implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go#L501)
defaults `cache_proxy.enable_get_tree_caching` to false and forwards GetTree
requests, response messages, and errors to/from the fixture-local app. Thus the
proxy observations demonstrate the app defect through forwarding; they do not
establish an independent proxy root cause or cover proxy tree caching enabled.
Reviewed the released [cache configuration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md),
[cache deployment documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-cache-setup.md),
and [remote execution documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/remote-build-execution.md);
none documents an exception to the relevant GetTree requirements.

The [REAPI GetTree contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L433)
explicitly requires the present portion of a partially missing tree and
`NOT_FOUND` for an absent root. The test uploads the valid parent and checks
per-blob OK statuses before querying it. Its deliberately absent child is a
well-formed SHA256/size reference: missing bytes need not be decodable, since
there are no bytes to decode. No file blobs are needed to enumerate this
parent. The absent root uses an empty instance name, which is permitted and
supported by this app; the missing-child failure independently uses the same
instance for upload and GetTree. No instance/auth mismatch explains it.

[fetchDir](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L553)
maps a missing root to digest.MissingDigestError;
[fetchDirectory](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L815)
returns that same error for any missing child. The
[error helper](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go#L733)
constructs `FAILED_PRECONDITION`. That status is useful for missing execution
inputs but is wrong for these GetTree cases. Traversal fails before sending
collected directories; both runs report FailedPrecondition for both cases on
both endpoints. The deterministic minimal cases do not depend on a CAS
eviction race or randomized traversal.

Severity remains **medium**: unlike reconstructible empty-directory omission,
one absent nonempty child aborts enumeration of otherwise available content,
and the wrong root status can break a client's normal cache-miss handling.
Actual eviction, data destruction, and an executor failure are not claimed;
the test demonstrates the API behavior using a deliberately absent digest.
