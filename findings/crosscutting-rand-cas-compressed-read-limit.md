# Compressed ByteStream reads accept a forbidden nonzero limit

Title: Compressed ByteStream reads succeed with nonzero read_limit and return truncated content
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: Protocol conformance and integrity-validation failure for clients supplying a nonzero compressed read limit. The server returns a successful truncated blob under the full blob's resource name. Conforming clients use zero limits and verify digests; no stored-content corruption or authorization bypass was observed.
Status: unverified
Duplicate of: [app-compression-read-limit.md](app-compression-read-limit.md) (confirmed app finding; this crosscutting repro also exercises the proxy).
Repro test target: //tests/crosscutting/rand-cas:compressed_read_limit_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/ba00f540-b039-4ebb-9281-b529ba0634c7

## Expected behavior

The [REAPI ByteStream compressed download contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L313-L318) says:

> Servers MUST return `INVALID_ARGUMENT` if `ReadRequest.read_limit` is non-zero.

After uploading `ab` using ordinary ByteStream, a read of
`compressed-limit/compressed-blobs/zstd/fb8e20fc2e4c3f248c60c39bd652f3c1347298bb977b8b4d5903b85055620603/2`
with `read_offset=0` and `read_limit=1` must fail with `INVALID_ARGUMENT`.

## Actual behavior

Both the direct app and fixture-local cache proxy return a successful stream
(normal EOF) with 14 compressed bytes decoding to `a`, rather than rejecting the
request. This response cannot match the requested two-byte digest.

First reproduction: [ba00f540-b039-4ebb-9281-b529ba0634c7](https://app.buildbuddy.io/invocation/ba00f540-b039-4ebb-9281-b529ba0634c7).
Second reproduction: [e4cdc8a8-e9f6-4a9e-929a-056af1b79a90](https://app.buildbuddy.io/invocation/e4cdc8a8-e9f6-4a9e-929a-056af1b79a90).

Both runs use fresh test-local containers and default harness configuration,
locally provisioned API keys, and the pinned image digests above. The upload
succeeds and validates its committed size. The [ordinary round-trip property
suite](https://app.buildbuddy.io/invocation/0bac2e81-e968-43a3-957a-e1043860480f) passes on the same fixture configuration, including zstd reads with zero
limits and ordinary reads with nonzero limits. This isolates the forbidden
compressed-limit behavior from auth, networking, and storage setup errors.

[Released source](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go) provides a matching explanation: `CheckReadPreconditions` checks negative offsets/limits, but `ReadCASResource` does not reject a positive limit for a compressed resource. Instead, it disables compressed passthrough when the limit is nonzero, reads a limited portion of the uncompressed cache entry and compresses that portion.

The finding remains `unverified` as requested for area submissions, despite two
independent reproductions; maintainer/coordinator review is pending.

## Logs excerpt

```text
--- FAIL: TestCompressedReadRejectsNonzeroLimit/app
  compressed Read(read_limit=1): status=EOF, wire_bytes=14 decoded="a"; want INVALID_ARGUMENT
--- FAIL: TestCompressedReadRejectsNonzeroLimit/cacheproxy
  compressed Read(read_limit=1): status=EOF, wire_bytes=14 decoded="a"; want INVALID_ARGUMENT
```

The excerpt is test output; full container logs, configs and Docker inspect
metadata are preserved by the fixture in undeclared outputs.

## Notes

Run explicitly (default/nobug suites exclude this target):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-cas:compressed_read_limit_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

No version overrides, custom cache settings, external peers or executor are
needed. All component calls address test-local containers; the dispatch API key
is never used by the test. Mitigation: use `read_limit=0` for compressed reads,
as required by REAPI. Findings remain in this repository.
