# Completed ByteStream upload status is unimplemented through the cache proxy

Title: Completed ByteStream upload status is unimplemented through the cache proxy
Components: cacheproxy / app
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: Downgraded from medium: this is an intentionally unsupported RPC. The test establishes no lost-response recovery failure, data loss, or exposure; redundant transfer is a possible cost for clients restarting uploads.
Status: rejected
Repro test target: //tests/cacheproxy/bytestream:completed_write_status
BuildBuddy invocation: https://app.buildbuddy.io/invocation/7b6c0f6f-cfee-4df3-b690-7621d2b8007e

## Expected behavior

The [ByteStream QueryWriteStatus contract](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto) describes a supported status query returning complete=true and committed_size equal to its size after completion. The original test assumes this RPC is supported; verification below rejects that assumption for this release.

## Actual behavior

After a proxy Write with finish_write=true succeeds and reports the exact committed size, direct app readback matches the data. QueryWriteStatus for that same upload name returns UNIMPLEMENTED: not implemented on both the proxy and direct app.

The first uncached run reproduced this using fixture-local app/proxy containers,
default SQLite/Pebble configuration, self-auth and a disposable local API key.
Successful upload/readback and direct-app comparisons rule out missing data,
authentication failures and a proxy peer configuration error. No overrides or
shared component deployments were used.

Reproduction 1: https://app.buildbuddy.io/invocation/7b6c0f6f-cfee-4df3-b690-7621d2b8007e
Reproduction 2 (independent verification): https://app.buildbuddy.io/invocation/7750d4f6-4fef-436f-b123-75abca038b28

## Logs excerpt

```text
completed upload QueryWriteStatus
rpc error: code = Unimplemented desc = not implemented
```

## Notes

This appears to be the same defect as `findings/app-bytestream-query-write-status.md` on
[PR #37](https://github.com/altdansalt/buildbuddy-validation/pull/37).
The proxy repro adds forwarding/local-cache coverage; it is not evidence of a
separate upstream defect. Independent verification verdict: rejected; see below.

Run `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/bytestream:completed_write_status --test_tag_filters=bug --nocache_test_results`.
Full container logs and inspect metadata are preserved in undeclared outputs.

## Verification

Independently reviewed PR #72 and its bug-tagged test on 2026-10-13.
Verdict: **rejected**. Both uncached hosted-runner executions used the exact
app/proxy digests listed above, without image or fixture configuration overrides.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test \
  //tests/cacheproxy/bytestream:read_beyond_eof \
  //tests/cacheproxy/bytestream:completed_write_status \
  --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Verification run 1](https://app.buildbuddy.io/invocation/9d65cf82-cc70-42cd-8348-9cc1ced8bce1) ([outer runner](https://app.buildbuddy.io/invocation/d06d0ac9-1121-4b16-867a-9630aa3c568c)).
- [Verification run 2](https://app.buildbuddy.io/invocation/7750d4f6-4fef-436f-b123-75abca038b28) ([outer runner](https://app.buildbuddy.io/invocation/c689bd0c-9539-4ed4-a579-bed6b413f0b5)).

Both targets failed at their protocol assertions in both runs, rather than during
fixture startup. The harness uses a fixture-local app peer and disposable local
auth; successful writes and byte-exact readbacks exclude a missing blob, bad
credentials, or an unreachable/misconfigured upstream. No shared deployment was
used for component RPCs.

Both runs successfully committed the uncompressed upload and verified app
readback, then received `UNIMPLEMENTED: not implemented` from both query endpoints.
This observation is real and stable, but the alleged regression is rejected:
[pinned app QueryWriteStatus](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/server/remote_cache/byte_stream_server/byte_stream_server.go#L867-L876) deliberately returns `UNIMPLEMENTED` and explicitly explains that
Bazel should restart from the beginning and avoid future queries. The
[pinned proxy implementation](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go#L1742-L1752) correctly forwards the request to that app. There is no lost
resource name, missing auth, or proxy-local status bookkeeping error. The app's
return is unconditional for these authenticated requests, including completed
writes; it is not restricted to incomplete uploads despite the comment's wording.

The [REAPI CAS contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto#L216-L290) specifies ByteStream writes, resource naming, and successful
short-circuiting of uploads for blobs already present. It makes no additional
promise that this release implements status queries. The
[generic ByteStream definition](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto#L76-L91) and [response fields](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto#L170-L179) describe the status API; they do not establish that BuildBuddy
advertises support for it. This is a deliberate limitation of the generic API,
not evidence that the observed response satisfies its successful-query contract.

[Bazel PR #28235](https://github.com/bazelbuild/bazel/pull/28235), referenced
by the pinned app, explicitly handles unsupported status queries by restarting
uploads and suppressing later queries after `UNIMPLEMENTED`. Its
[patch](https://github.com/bazelbuild/bazel/pull/28235.patch) shows this fallback
already existed before that optimization. The release's `docs/` has no specific
completed-status support promise; copied generic RPC comments are weaker evidence
than the explicit implementation decision. The test does not simulate a lost
write response or prove that restart recovery fails.

Downgrade medium to low because only an unsupported feature is demonstrated.
A future status-query feature may be useful, but this bug-tagged assertion should
be retired or changed to cover the supported restart fallback; test changes are
outside this findings-only review.
