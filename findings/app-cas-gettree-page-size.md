# GetTree ignores page_size and returns an oversized page

Title: GetTree ignores page_size and returns an oversized page
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cache proxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: Clients cannot bound the number of directory messages per response or obtain resumable pages for this small tree. This is API conformance and client resource-control impact, without demonstrated data loss or a security boundary violation.
Status: confirmed
Repro test target: //tests/app/cas-gettree:bugs_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/ea7980d9-4bad-42f1-9db4-5d3463f2e3ac

## Expected behavior

GetTreeRequest.page_size is a maximum number of items per page. A two-directory tree requested with page_size=1 must not place both directories in one GetTreeResponse. Traversal order may vary for fresh calls; issued tokens must resume the corresponding suffix. See the [REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

## Actual behavior

Both app and proxy return the complete two-directory tree in a single response despite page_size=1. Its next_page_token is empty. Both hosted runs show the same failure. Content is otherwise correct, ruling out malformed input. Reproduction links: [run 1](https://app.buildbuddy.io/invocation/ea7980d9-4bad-42f1-9db4-5d3463f2e3ac) and [run 2](https://app.buildbuddy.io/invocation/41a50bf1-c29b-4d87-905c-f17abc3c7f3d). Independent verification is recorded below.

## Logs excerpt

```text
page_size=1: response contains 2 directories; token=""
```

These assertions fail for both `app` and `cacheproxy` subtests. Full fixture container logs and image metadata are preserved in the invocation's undeclared test outputs.

## Notes

The pinned GetTree implementation reads neither PageSize nor PageToken; finishDir flushes only at the gRPC byte-size limit. No issued token is available in this minimal repro, so token handling/stability remains a follow-up rather than a separately demonstrated defect. Source: [released CAS server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go).

Default harness configuration: fixture-local authenticated app, SQLite, Pebble, Redis, and proxy forwarding only to that app. No image/config overrides or shared component endpoints. Passing seeded coverage independently validates nonempty SHA256/SHA1 directory content through both components.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:bugs_test --test_tag_filters=bug --nocache_test_results --test_filter=TestPageSize --test_output=all
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

The [REAPI page_size field](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1998)
is a **maximum**, not a hint; servers may impose smaller limits. Each
GetTreeResponse is a page, and the two valid nonempty Directory messages must
not share a response when page_size=1. The leaf's symlink is legal and does not
need a CAS upload for its target. Content comparisons pass; only the size
constraint fails, with `response contains 2 directories; token=""` on both
endpoints in both runs.

The released [finishDir](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L911)
flushes only at the gRPC byte-size limit. GetTree never reads PageSize or
PageToken. Transport size batching does not satisfy the directory-count
maximum. Traversal concurrency cannot explain this two-node oversized page,
and the test does not depend on a fresh-call traversal order.

Severity remains **low**: this establishes a conformance/resource-control
defect, without a demonstrated outage or data loss. Empty next_page_token is
valid for a completed stream; it is not independently a defect here. The token
resume assertions are conditional and unexercised, so neither token issuance
nor resume correctness is claimed as separately reproduced.
