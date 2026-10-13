# ByteStream reads beyond EOF panic through both cold and warm cache proxy

Title: ByteStream reads beyond EOF panic through both cold and warm cache proxy
Components: cacheproxy / app
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: medium
Severity justification: An authenticated cache client can trigger a recovered panic and an unexpected internal error. No process exit or wider denial of service is established.
Status: confirmed
Repro test target: //tests/cacheproxy/bytestream:read_beyond_eof
BuildBuddy invocation: https://app.buildbuddy.io/invocation/7b6c0f6f-cfee-4df3-b690-7621d2b8007e

## Expected behavior

The [ByteStream ReadRequest contract](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto) requires OUT_OF_RANGE when read_offset exceeds the resource size.

## Actual behavior

Reading one byte beyond the size of a small inline Pebble blob returns INTERNAL: A panic occurred on the direct app and on both cold and warm proxy paths. The cold proxy forwards the upstream error; the warm proxy also panics in its own inline reader. Normal readback after each malformed request preserves the bytes.

The first uncached run reproduced this using fixture-local app/proxy containers,
default SQLite/Pebble configuration, self-auth and a disposable local API key.
Successful upload/readback and direct-app comparisons rule out missing data,
authentication failures and a proxy peer configuration error. No overrides or
shared component deployments were used.

Reproduction 1: https://app.buildbuddy.io/invocation/7b6c0f6f-cfee-4df3-b690-7621d2b8007e
Reproduction 2 (independent verification): https://app.buildbuddy.io/invocation/7750d4f6-4fef-436f-b123-75abca038b28

## Logs excerpt

```text
rpc error: code = Internal desc = A panic occurred
Unexpected event "recovered_panic": runtime error: slice bounds out of range [47:46]
enterprise/server/filestore/filestore.go:741
```

## Notes

This appears to be the same defect as `findings/app-bytestream-read-past-end-panic.md` on
[PR #37](https://github.com/altdansalt/buildbuddy-validation/pull/37).
The proxy repro adds forwarding/local-cache coverage; it is not evidence of a
separate upstream defect. Independent verification verdict: confirmed; see below.

Run `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/bytestream:read_beyond_eof --test_tag_filters=bug --nocache_test_results`.
Full container logs and inspect metadata are preserved in undeclared outputs.

## Verification

Independently reviewed PR #72 and its bug-tagged test on 2026-10-13.
Verdict: **confirmed**. Both uncached hosted-runner executions used the exact
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

The [REAPI CAS contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto#L292-L306) delegates downloads to ByteStream and defines the uncompressed
resource name used by the test. [ByteStream ReadRequest](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto#L99-L104) explicitly requires `OUT_OF_RANGE` when the offset exceeds the resource size.
The test supplies the correct digest and size, then reads at size+1; it does not
confuse EOF itself with beyond EOF, or compressed with uncompressed offsets.

Pinned v2.314.0 source explains the observed failure:
[CheckReadPreconditions](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/server/remote_cache/byte_stream_server/byte_stream_server.go#L110-L118) checks negative offsets/limits but not the upper offset bound;
[InlineReader](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/enterprise/server/filestore/filestore.go#L740-L746) directly executes `f.GetData()[offset:]`. The
[proxy read path](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go#L265-L280) tries local storage then remote storage on a miss. Both runs returned
`INTERNAL: A panic occurred` in cold/proxy, cold/app, warm/proxy and warm/app,
with recovered slice-bounds panics in app and proxy logs. The normal readback
after each malformed request succeeded. Distinct cold/warm blob contents prevent
the cold case from accidentally using the warm blob, and subtests are sequential.
This deterministic bounds error is not readiness, networking, or timing flakiness.

Retain medium severity: malformed authenticated input triggers a server panic
and violates the explicit error contract, but recovery preserves the process and
blob; the test establishes neither data loss nor broader denial of service. The
release documentation reviewed in `docs/` contains no intended exception to this
offset contract. This remains shared app/proxy storage behavior, not a distinct
proxy-only defect.
