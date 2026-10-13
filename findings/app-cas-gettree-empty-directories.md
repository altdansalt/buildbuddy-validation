# GetTree omits empty root and child Directory messages

Title: GetTree omits empty root and child Directory messages
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cache proxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: GetTree omits required Directory messages, violating enumeration completeness. The empty contents can be inferred from their digest, and BuildBuddy’s executor explicitly handles this case; this reproducer does not demonstrate filesystem data loss or failed execution.
Status: confirmed
Repro test target: //tests/app/cas-gettree:bugs_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/ea7980d9-4bad-42f1-9db4-5d3463f2e3ac

## Expected behavior

GetTree must return the Directory messages reachable from its root, including an empty root or child. An empty Directory has a valid zero-byte protobuf encoding and the SHA256 empty-blob digest. Uploading that encoding succeeds. See the [REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

## Actual behavior

Both app and proxy return no Directory for an empty root (expected one), and only the parent for a parent containing one empty child (expected two). Both cases reproduce in independent hosted runs. Reproduction links: [run 1](https://app.buildbuddy.io/invocation/ea7980d9-4bad-42f1-9db4-5d3463f2e3ac) and [run 2](https://app.buildbuddy.io/invocation/41a50bf1-c29b-4d87-905c-f17abc3c7f3d). Independent verification is recorded below.

## Logs excerpt

```text
reachable directory set: got 0 want 1
reachable directory set: got 1 want 2
```

These assertions fail for both `app` and `cacheproxy` subtests. Full fixture container logs and image metadata are preserved in the invocation's undeclared test outputs.

## Notes

GetTree returns immediately when rootDirRN.IsEmpty(); fetchDirectory skips empty child resource names. The repro successfully uploads the Directory encodings and uses authenticated fixture-local clients. This distinguishes the behavior from missing blobs or auth failure. Source: [released CAS server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go).

Default harness configuration: fixture-local authenticated app, SQLite, Pebble, Redis, and proxy forwarding only to that app. No image/config overrides or shared component endpoints. Passing seeded coverage independently validates nonempty SHA256/SHA1 directory content through both components.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:bugs_test --test_tag_filters=bug --nocache_test_results --test_filter=TestEmptyDirectories --test_output=all
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

The [REAPI GetTree contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L416)
requires recursive enumeration; [Directory](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1048)
has no required nonempty field. The test's `{}` encoding and SHA256 digest are
valid, uploads return OK, and comparisons accept any traversal order and
duplicate messages. The missing empty child is decisive even if “descended
from the root” is read ambiguously for the root itself.

A mundane storage explanation does not resolve this: the released app
[acknowledges empty uploads without storing them](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L291),
which is a valid optimization if reads synthesize the implicit empty blob.
However, [GetTree returns immediately for an empty root](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L888)
and [fetchDirectory skips empty children](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L803)
instead of emitting the Directory. Observed counts were 0 versus 1 and 1 versus
2 on both endpoints in both runs. This is deterministic digest handling,
not eviction or ordering flakiness.

Severity adjusted from medium to **low**: clients can synthesize these contents
from the known empty digest. BuildBuddy's own
[directory materialization](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L1697)
creates the child path and explicitly skips fetching empty children. The
reported omission is real, but filesystem loss or executor failure is not
established by this test. Enumeration completeness remains nonconformant.
