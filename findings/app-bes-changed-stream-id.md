# BES accepts a changed stream identity and ACKs the wrong invocation

Title: BES accepts a changed stream identity and ACKs the wrong invocation
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Malformed authenticated clients receive misleading successful commit acknowledgements. The repro does not demonstrate impact on conforming clients or a cross-organization authorization bypass; independent review therefore lowers severity from medium to low.
Status: confirmed
Repro test target: //tests/app/bes:changed_identity
BuildBuddy invocation: https://app.buildbuddy.io/invocation/d375f6f1-7f04-4f31-9c9d-578fa786679c

## Expected behavior

[PublishBuildToolEventStream](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto#L72-L75) publishes events belonging to the same stream. Each OrderedBuildEvent identifies its stream, and each ACK identifies the stream containing the committed event. Reject a change of stream identity within an RPC. In particular, do not ACK an event for invocation B as an event for invocation A.

## Actual behavior

One RPC sends BuildStarted for invocation A at sequence 1, then BuildFinished for a distinct invocation B at sequence 2. Both invocation_id and build_id change. The server completes successfully and emits ACKs for sequences 1 and 2, both with A's StreamId. No event for A at sequence 2 was sent.

Reproduced independently in [run 1](https://app.buildbuddy.io/invocation/d375f6f1-7f04-4f31-9c9d-578fa786679c) and [run 2](https://app.buildbuddy.io/invocation/981c7e26-28bc-4eb4-9689-01ebe8313cbf). Both tests failed on the intended successful-completion assertion. The second run includes a valid sequence-1 control in the same fixture before the mixed-ID RPC. The same encoding/client/config also passes [normal upload, retry, and readback coverage](https://app.buildbuddy.io/invocation/7c914798-1c87-492b-86e2-9a7ae6bef94a), ruling out fixture/auth/encoding issues. The generated BES client serializes the two explicitly supplied IDs; the harness attaches only a local API key and does not rewrite requests.

## Logs excerpt

```text
RPC containing distinct streams must be rejected:
sent invocation IDs=[f25833d7-29c5-44e2-89b9-9c3b88c73bf6,77d44b08-def2-4d33-a053-e1388a7c71b8]
ACKs=[stream_id:{build_id:"f25833d7-29c5-44e2-89b9-9c3b88c73bf6" invocation_id:"f25833d7-29c5-44e2-89b9-9c3b88c73bf6" component:TOOL} sequence_number:1
      stream_id:{build_id:"f25833d7-29c5-44e2-89b9-9c3b88c73bf6" invocation_id:"f25833d7-29c5-44e2-89b9-9c3b88c73bf6" component:TOOL} sequence_number:2]
error=<nil>
```

## Notes

No custom image, flags, or config. The default app fixture provides SQLite/local storage, Redis, built-in self-auth, and a disposable API key. No shared service receives component requests. Full configs, logs, and inspect metadata are preserved in undeclared outputs.

The released [server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_server/build_event_server.go) opens its channel and saves StreamId only on the first message. Later messages go to that same channel without an identity equality check. `postProcessStream` emits every ACK with the first StreamId. Comparing all subsequent StreamIds against the first before HandleEvent would avoid this inconsistency. Persistence contents and cross-organization behavior are not asserted by this minimal reproducer.

Reproduce with `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:changed_identity --test_tag_filters=bug --nocache_test_results` with the public fetch remote configured per AGENTS.md. The Remote Execution API does not define BES; this uses the pinned release's BES service contract. The original report left status unverified pending independent review. No upstream report was sent.

Final validation with preserved-container panic checks: [green regression](https://app.buildbuddy.io/invocation/f3221279-bdb0-4c57-a375-ec1522d91113), [third explicit reproduction](https://app.buildbuddy.io/invocation/09b89765-610b-46a7-8bba-b0a493a04b78). Both reproducers failed at the intended assertion again; no panic/fatal-error markers were detected.

## Verification

Verdict: **confirmed**. Sending two identities in one RPC violates the documented same-stream request contract. That does not justify an ACK claiming that A/2 was committed when the client sent B/2. The test correctly expects rejection of the conflicting identity, and both runs instead succeeded with two ACKs naming only A. This is an input-validation and acknowledgement-integrity defect; persisted event mixing and cross-organization effects remain unproven.

The [server receive loop](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_server/build_event_server.go#L188-L209) stores and opens a channel using only the first StreamId and never compares subsequent StreamIds. [postProcessStream](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_server/build_event_server.go#L229-L266) then uses that first ID for every ACK. The [service contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto#L72-L75) requires one stream, and the response identifies the stream containing the committed event. No documented multiplexing policy was found that could make rewriting B to A valid.

Severity changed from medium to **low**: only a malformed authenticated uploader is demonstrated. Ordinary single-identity clients are unaffected by this repro, and it establishes neither an authorization bypass nor persisted cross-invocation corruption.

Reviewed independently on 2026-10-12 against PR #22's tests and the exact app digest above. No image, flag, or fixture configuration overrides were used. RPCs address the fixture's loopback container endpoints with a disposable local API key. The request helpers encode the pinned Bazel protobuf fields and do not rewrite stream identities. Each reproducer first successfully ACKs a sequence-1 control; the full BES regression target also passed in the first verification invocation, including completed invocation readback and legitimate suffix retries. Both independent uncached executions failed at the reported assertion, without a panic marker; this is deterministic behavior rather than readiness, authentication, or storage flakiness.

Checked [bazelbuild/remote-apis](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto): REAPI defines execution/cache APIs and invocation correlation in RequestMetadata, not BES acknowledgement semantics. The applicable contract is the release's [PublishBuildEvent proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto#L138-L159). The release's [write architecture docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/architecture-build-event-write.md) do not document an exception to that contract.

Commands used `GIT_CONFIG_GLOBAL=/dev/null bb remote test <targets> --config=nobug --test_tag_filters= --nocache_test_results --test_output=all`, with the public GitHub fetch remote. [Verification run 1](https://app.buildbuddy.io/invocation/3f50e147-6d05-4e95-9dce-28d0ec43b8a6) ran both bug targets and `//tests/app/bes:regression`; [verification run 2](https://app.buildbuddy.io/invocation/65e0f9cf-fbd6-4f3b-bb1f-8869d57f4233) ran both bug targets. Failures are the expected defect assertions, not build failures.

## Version history

Verified on 2026-10-13 using the unchanged bug-tagged target `//tests/app/bes:changed_identity`
from `origin/area/app-bes` at `f13831aec05f0a4204f08487ce7af2c0278dcdff`.
Tests ran inside disposable fixture-local released containers on hosted runners.

| Release | Published | Result | Bazel invocation | Runner invocation |
| --- | --- | --- | --- | --- |
| enterprise-v2.314.0 | 2026-10-08 | Reproduces | [results](https://app.buildbuddy.io/invocation/ab214fa0-ff83-47c9-9644-59a8b3038d04) | [runner](https://app.buildbuddy.io/invocation/eb049c2d-145f-41cc-81ab-b2990e8c3c2a) |
| enterprise-v2.313.0 | 2026-10-06 | Reproduces | [results](https://app.buildbuddy.io/invocation/6e73827e-18af-442b-9b6f-8c73691a69bd) | [runner](https://app.buildbuddy.io/invocation/db9c3c9c-e3c9-4f4b-8e1e-7a7ac25d9ae4) |
| enterprise-v2.290.0 | 2026-07-29 | Reproduces | [results](https://app.buildbuddy.io/invocation/aacbf37e-1f48-4340-813d-15aed8ee6455) | [runner](https://app.buildbuddy.io/invocation/256d903c-ab1e-4e21-9c3e-853734a3441d) |
| enterprise-v2.275.0 | 2026-06-04 | Reproduces | [results](https://app.buildbuddy.io/invocation/5a6b83d7-3c9e-41c4-bd5a-8ff71cd6d859) | [runner](https://app.buildbuddy.io/invocation/1b9de404-2fca-46e2-9965-535de71d8564) |
| enterprise-v2.260.0 | 2026-04-16 | Reproduces | [results](https://app.buildbuddy.io/invocation/4d91574b-8fda-4cbf-a1b8-0356237b74d0) | [runner](https://app.buildbuddy.io/invocation/fafe222f-5034-4854-b934-fe650f7d8d23) |

Observed in every release marked Reproduces: The mixed-identity RPC succeeded and ACKed sequences 1 and 2 using the first invocation identity.

Earliest tested reproducing release: **enterprise-v2.260.0 (2026-04-16)**.
The defect was present by that release; its introduction release is unknown
and may be earlier. These five samples span nearly six months and do not
establish uninterrupted behavior in untested intermediate releases.

No startup/configuration changes were needed. Each execution reached the
reported protocol assertion rather than failing fixture startup or authentication.
The manifest lookup for the initially considered v2.258.0 returned HTTP 404;
v2.260.0 was used as the April sample instead.

Exact linux/amd64 manifest pins resolved from the GCR manifest API:

| Release | Component | Image reference |
| --- | --- | --- |
| enterprise-v2.314.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` |
| enterprise-v2.313.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6` |
| enterprise-v2.290.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:f3b285107f28da495e9496dd848857ce0cf6cddbf3fac8743b259c2a7a8678b6` |
| enterprise-v2.275.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:75ffd43cf6dc14ccf5a652b760d7e4d7390dfa7870524c57e7f354f271e96db1` |
| enterprise-v2.260.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:0f6376865cc9c92d0e837aa1cb53717b59875db68a1b09b6dd479c571f6ab88e` |

Runner checkout used origin/main at `40ac83dc2c3d00efd8993a68e33a2b6df445c8fc`
with the original test-area files applied from their PR branches in a temporary
checkout; this history branch contains only findings. The reproducible command
for the current release is (replace image references with the row for an older release):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:changed_identity \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image='gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843'
```

Each linked invocation ran all 12 in-scope reproducers together, with explicit
app, executor, and cacheproxy digest overrides; unused components were not started.
The final bug filter overrides the default nobug exclusion, and test-result
caching was disabled. Container configs, logs, and inspect metadata are retained
in the invocations' undeclared test outputs.
