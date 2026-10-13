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

## Version history

Verified on 2026-10-13 using the unchanged bug-tagged target `//tests/cacheproxy/bytestream:read_beyond_eof`
from `origin/area/cacheproxy-bytestream` at `0f742f3f8f8bf730d03c0f0b4294a9621fce3a32`.
Tests ran inside disposable fixture-local released containers on hosted runners.

| Release | Published | Result | Bazel invocation | Runner invocation |
| --- | --- | --- | --- | --- |
| enterprise-v2.314.0 | 2026-10-08 | Reproduces | [results](https://app.buildbuddy.io/invocation/ab214fa0-ff83-47c9-9644-59a8b3038d04) | [runner](https://app.buildbuddy.io/invocation/eb049c2d-145f-41cc-81ab-b2990e8c3c2a) |
| enterprise-v2.313.0 | 2026-10-06 | Reproduces | [results](https://app.buildbuddy.io/invocation/6e73827e-18af-442b-9b6f-8c73691a69bd) | [runner](https://app.buildbuddy.io/invocation/db9c3c9c-e3c9-4f4b-8e1e-7a7ac25d9ae4) |
| enterprise-v2.290.0 | 2026-07-29 | Reproduces | [results](https://app.buildbuddy.io/invocation/aacbf37e-1f48-4340-813d-15aed8ee6455) | [runner](https://app.buildbuddy.io/invocation/256d903c-ab1e-4e21-9c3e-853734a3441d) |
| enterprise-v2.275.0 | 2026-06-04 | Reproduces | [results](https://app.buildbuddy.io/invocation/5a6b83d7-3c9e-41c4-bd5a-8ff71cd6d859) | [runner](https://app.buildbuddy.io/invocation/1b9de404-2fca-46e2-9965-535de71d8564) |
| enterprise-v2.260.0 | 2026-04-16 | Startup blocked (proxy port unavailable) | [results](https://app.buildbuddy.io/invocation/4d91574b-8fda-4cbf-a1b8-0356237b74d0) | [runner](https://app.buildbuddy.io/invocation/fafe222f-5034-4854-b934-fe650f7d8d23) |

Observed in every release marked Reproduces: Direct app and cold/warm proxy reads at size+1 returned INTERNAL: A panic occurred; normal readback still succeeded.

Earliest tested reproducing release: **enterprise-v2.275.0 (2026-06-04)**.
The defect was present by that release; its introduction release is unknown
and may be earlier. These five samples span nearly six months and do not
establish uninterrupted behavior in untested intermediate releases.

No configuration changes were made. The four reproducing releases reached the
reported protocol assertions. On v2.260.0 both proxy targets failed during startup
at harness/fixture.go:300: `no mapping for <fixture>-cacheproxy:8080`.
The app started successfully, but the proxy never exposed its HTTP port; no
proxy RPC assertion ran. The cause/configuration adjustment was not investigated
further, per the older-release startup time limit. This is **inconclusive**, not
evidence of a fix or a new product defect. The linked undeclared outputs retain
the stopped proxy logs/config/inspect metadata for follow-up.
Image overrides changed all started components together; proxy results therefore
measure same-version app/proxy pairs, not an isolated proxy introduction date.
The manifest lookup for the initially considered v2.258.0 returned HTTP 404;
v2.260.0 was used as the April sample instead.

Exact linux/amd64 manifest pins resolved from the GCR manifest API:

| Release | Component | Image reference |
| --- | --- | --- |
| enterprise-v2.314.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` |
| enterprise-v2.314.0 | cacheproxy | `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906` |
| enterprise-v2.313.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6` |
| enterprise-v2.313.0 | cacheproxy | `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:111df7a8d4bb57993bc78f8ed18458f32a43a93e5028f906f4c6e537e10434c6` |
| enterprise-v2.290.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:f3b285107f28da495e9496dd848857ce0cf6cddbf3fac8743b259c2a7a8678b6` |
| enterprise-v2.290.0 | cacheproxy | `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:489014eaf088f8b8e19bee3de97c98770897edabbe614b368b74b64e72858df5` |
| enterprise-v2.275.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:75ffd43cf6dc14ccf5a652b760d7e4d7390dfa7870524c57e7f354f271e96db1` |
| enterprise-v2.275.0 | cacheproxy | `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:8ff5709daa40bcde7968b9687d42201b71b223f50e6cdf5160c00137bfe709ad` |
| enterprise-v2.260.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:0f6376865cc9c92d0e837aa1cb53717b59875db68a1b09b6dd479c571f6ab88e` |
| enterprise-v2.260.0 | cacheproxy | `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:f5863331f261446428fd370bbc4c55a8317ab983121a6629e1dea1de701802de` |

Runner checkout used origin/main at `40ac83dc2c3d00efd8993a68e33a2b6df445c8fc`
with the original test-area files applied from their PR branches in a temporary
checkout; this history branch contains only findings. The reproducible command
for the current release is (replace image references with the row for an older release):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/bytestream:read_beyond_eof \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image='gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843' \
  --test_arg=-harness.cacheproxy_image='gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906'
```

Each linked invocation ran all 12 in-scope reproducers together, with explicit
app, executor, and cacheproxy digest overrides; unused components were not started.
The final bug filter overrides the default nobug exclusion, and test-result
caching was disabled. Container configs, logs, and inspect metadata are retained
in the invocations' undeclared test outputs.
