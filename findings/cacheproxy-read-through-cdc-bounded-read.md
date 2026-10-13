# CDC-uploaded blobs return NotFound for bounded ByteStream reads

Title: CDC-uploaded blobs return NotFound for bounded ByteStream reads
Components: cacheproxy (with app fallback)
Image version/digest: enterprise-v2.314.0; cacheproxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
Severity: medium
Severity justification: Opt-in proxy CDC interception can make a successfully uploaded CAS resource unavailable to ordinary bounded ByteStream readers. The contents remain readable in full, so this is a protocol/availability defect rather than demonstrated data loss or a trust-boundary violation. Default write interception is disabled.
Status: confirmed
Repro test target: //tests/cacheproxy/read-through:cdc_bounded_read_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/ff5bc714-8a08-443d-8472-7cd024e14427
Second invocation: https://app.buildbuddy.io/invocation/af2cffde-3523-44e1-abb3-ca8fd7877f87

## Expected behavior

An identity ByteStream Read of an existing CAS resource with `read_offset=12345`
and `read_limit=17` returns bytes `[12345:12362]` with successful completion.
[ByteStream ReadRequest](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto)
permits positive limits. Upload interception and internal chunk storage should
preserve that resource's externally visible semantics.

## Actual behavior

A seeded 1 MiB incompressible payload is uploaded successfully through a
CDC-enabled writer proxy. Full reads return exactly the original payload from
all three endpoints: app, writer proxy, and a separate cold reader proxy.
The reader proxy then returns `NotFound` for the same resource with offset 12345
and limit 17. Both explicit, uncached repro runs failed at that assertion;
fixture startup, upload, all full-read checks, and cleanup succeeded.

The controls rule out wrong instance, digest, authentication, connectivity,
missing uploaded data, or an unstarted experiment provider. The default
non-CDC range coverage passes, and CDC full/suffix coverage separately requires
positive chunk-write and remote-chunk-read metrics. All peer endpoints and
keys are fixture-local; the failure is unrelated to the worker dispatch key.
Independent verification below confirms this behavior on the pinned release.

## Logs excerpt

Both repro runs:

```text
--- FAIL: TestCDCBoundedRead
rpc error: code = NotFound desc = key "PTdefault/81586469c44849cb50b28dd8d9126ffac00e5d2eb19c83bc730bee770168e9cc/1/cas/v5" not found
Messages: CDC full read succeeded but bounded read failed
```

The test reaches this assertion only after successful byte-for-byte full reads
from the app and both proxies. Container configs, logs, and inspect metadata,
plus the flagd sidecar log, are preserved in undeclared outputs.

## Notes

Run:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test \
  //tests/cacheproxy/read-through:cdc_bounded_read_bug_test \
  --test_tag_filters=bug --nocache_test_results --test_output=all
```

No image version overrides are used. App and proxies use
`--cache.avg_chunk_size_bytes=65536`. A fixture-local flagd v0.12.4 sidecar,
`ghcr.io/open-feature/flagd@sha256:186f35094d0493b4149e8c8f1a7413e2d1a0336622e009d0a2dbeca30c14aa89`,
sets `cache_proxy.intercept_and_chunk_large_writes=true` and
`cache_proxy.cdc_read_fast_path=true`; proxies use
`--experiments.flagd_backend=<fixture-sidecar>:8015`.
The payload uses `math/rand` seed `20261013`. The app cache and both proxy caches
remain at their default 1 GiB capacities; this repro does not induce eviction.

Source inspection suggests the positive limit bypasses chunked reconstruction:
[ShouldReadChunkedOnProxy and ShouldReadChunked](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/chunking/chunking.go)
both require `limit == 0`. A manifest-only resource can therefore fall through
to a missing whole-blob CAS key. This is an explanation from released source,
not an independently verified internal trace. The assertion targets the proxy;
it does not separately claim that bounded reads directly against the app were
reproduced. Workaround: leave proxy CDC write interception disabled, or fetch
an unlimited suffix and truncate on the client. No upstream report was filed.

## Verification

Verdict: **confirmed** (independent review, 2026-10-13). Severity remains
**medium**: this breaks valid range reads when opt-in CDC write interception is
used, but full reads still recover the bytes and the default write path is unaffected.

Reviewed PR #114 commit `bb5c0afcf5a53eb41378f48b1f16f602221aecd8`, including the
bug target, `cdcFixture`, and the raw `read` helper. The helper sends the same
instance/digest with identity compression and the fixture-local key; it does not
set `proxy_skip_remote`. The 17-byte range lies within the seeded 1 MiB resource.
Startup, authenticated upload, and byte-for-byte full reads from the app and both
proxies succeed before the failing assertion. Separate fixture networks/caches,
default 1 GiB capacity, and a five-minute context exclude missing credentials,
wrong peer, deliberate eviction, and client timeout as explanations. The reader
starts cold, although the test warms its chunk cache with a full read before the
bounded request; the claim is not a first-request-only failure.

The [REAPI CAS download rules](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L290)
use ByteStream for identity downloads. The nonzero-limit prohibition there
applies only to compressed downloads. The [ByteStream ReadRequest contract](https://github.com/googleapis/googleapis/blob/f5f34cc12374c1b3c11240e3d733b34ad0f63f49/google/bytestream/bytestream.proto#L95)
permits positive limits and nonnegative in-range offsets. Internal chunking does
not change the uploaded resource name or grant an exception to that contract.

At release source commit `974929724a4c75f824ab18fbcb560e48bbf2803a`,
[ShouldReadChunked / ShouldReadChunkedOnProxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/chunking/chunking.go#L130)
require `limit == 0`. The [proxy read fallback](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go#L265)
therefore tries whole-resource storage for this bounded request; the
[app fallback](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go#L204)
also skips reconstruction. The [write experiment gate](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go#L1224)
is intentionally opt-in. That limits exposure, but does not justify returning
NotFound for a valid, successfully uploaded resource. Reviewed the released
[cache setup documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-cache-setup.md);
found no documented identity-range restriction. Source inspection explains the
observations; no internal tracing or direct app bounded-read reproduction is claimed.

Independent uncached runs, both using the exact image digests above:

- [Run 1](https://app.buildbuddy.io/invocation/e3d605ff-298f-4942-8c66-88016fd01557): failed at `cdc_range_bug_test.go:24` with the reported CAS-key NotFound, after all three full-read checks; test duration 25.60s.
- [Run 2](https://app.buildbuddy.io/invocation/381544dc-a26f-4ed6-b43f-55ab1e3b77f5): identical assertion and key; test duration 26.75s.

Both ran `GIT_CONFIG_GLOBAL=/dev/null bb remote test
//tests/cacheproxy/read-through:cdc_bounded_read_bug_test --config=nobug
--test_tag_filters= --nocache_test_results --test_output=all` from the PR checkout.
The explicit empty tag filter overrides the bug exclusion; both logs show one
test actually executed. No image overrides or test changes were used.
