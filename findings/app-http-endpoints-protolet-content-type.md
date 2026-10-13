# JSON HTTP RPC responses omit Content-Type

Title: JSON HTTP RPC responses omit Content-Type
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Clients that dispatch decoding based on the response media type cannot reliably recognize JSON. Requires a successful HTTP RPC; no corruption or authorization bypass was observed.
Status: confirmed
Repro test target: //tests/app/http-endpoints:protolet_content_type_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/9f22f187-f8c6-4a8c-9c94-a3104bfc2d16

## Expected behavior

A successful JSON protolet response should declare `Content-Type: application/json`.
The [released WriteProtoToResponse implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/http/protolet/protolet.go)
explicitly sets that media type. See also [RFC 9110 section 8.3](https://www.rfc-editor.org/rfc/rfc9110.html#section-8.3).

## Actual behavior

Cookie-authenticated `POST /rpc/BuildBuddyService/GetUser` with Content-Type
`application/json` and body `{}` returns HTTP 200 with valid user JSON but no
Content-Type header. Both isolated reproductions fail on that header assertion.
The same endpoint is used successfully by the harness to provision the user and
API key, ruling out failed login, missing service configuration, or invalid JSON.

Source inspection shows WriteProtoToResponse calls Write before Header().Set.
The response headers have already been committed, so the intended media type is
not sent. The response is served directly from the fixture, with no external proxy.

Reproductions: [run 1](https://app.buildbuddy.io/invocation/9f22f187-f8c6-4a8c-9c94-a3104bfc2d16), [run 2](https://app.buildbuddy.io/invocation/2e8708a8-4639-4ffb-a9ce-b4fa36cf5ab4). Both ran with
`--test_tag_filters=bug --nocache_test_results` and failed as expected.
Independent review verdict and additional reproductions are recorded below.

## Logs excerpt

```text
JSON RPC returned Content-Type ""; want application/json; body={"displayUser":...}
```

This excerpt is from the Bazel test log. Full container logs, configs and inspect
metadata are retained by the fixture in undeclared test outputs.

## Notes

Default harness app configuration: SQLite, local Pebble cache and storage,
self-auth, disposable local group and API key, fixture-local Redis. No image
version overrides. Tests run with `bb remote --runner_exec_properties=init-dockerd=true`.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote --runner_exec_properties=init-dockerd=true test //tests/app/http-endpoints:protolet_content_type_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

An additional successful reproduction is https://app.buildbuddy.io/invocation/a1fe7b17-579f-4a22-a43b-8f67a8a8c30b. A separate attempt failed during Redis image pull due to Docker Hub rate limiting and is not counted as product evidence.
Findings remain in this repository; no upstream report.

## Verification

Independent review (2026-10-13): **confirmed**, severity **low** retained. The
successful, authenticated GetUser response loses its intended JSON media type;
there is no evidence of data corruption, access-control failure, or unavailable
RPC functionality. The test correctly parses the media type rather than requiring
an exact header string, and succeeds through fixture login/provisioning before
checking the response.

Reviewed PR #54's bug-tagged test and the pinned v2.314.0 source (commit
`974929724a4c75f824ab18fbcb560e48bbf2803a`).
[WriteProtoToResponse lines 110–119](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/http/protolet/protolet.go#L110)
explicitly intends application/json but writes the body first.
[Go's ResponseWriter contract](https://pkg.go.dev/net/http#ResponseWriter)
says header changes after Write have no effect. The harness uses an ordinary
Go HTTP transport with automatic gzip negotiation/decompression; it only maps
the fixture issuer to its published loopback port and does not remove headers.
The released [gzip middleware](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/http/interceptors/interceptors.go#L230)
writes the gzip stream before the late header assignment, explaining why this
request can have no Content-Type rather than an automatically sniffed type.
This is normal HTTP negotiation, not a harness/configuration error.

[RFC 9110 section 8.3](https://www.rfc-editor.org/rfc/rfc9110.html#section-8.3)
uses SHOULD, not MUST, for sending a known media type; confirmation rests also
on the implementation's explicit intent, not a claim that every absent header
violates an unconditional protocol requirement. The released
[HTTP API documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-api.md)
describes JSON HTTP RPCs and does not document this header omission as intended.
[bazelbuild/remote-apis](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
defines the remote execution/cache RPCs, not this BuildBuddy UI protolet endpoint;
no REAPI media-type rule is being used as the oracle.

Two independent uncached reviewer runs used the exact pinned app digest above,
fresh fixture-local app/Redis containers, SQLite and self-auth; neither failed
on setup or networking. Both returned HTTP 200 and user JSON, then failed solely
on Content-Type "". Commands were run from PR #54's checkout (tests are not yet
on main), with no test/image/config changes:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/http-endpoints:protolet_content_type_test //tests/app/http-endpoints:missing_invocation_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Reviewer invocation links: [run 1](https://app.buildbuddy.io/invocation/6d09348a-6c65-411a-95ea-5d19e65d64eb),
[run 2](https://app.buildbuddy.io/invocation/31ee20ab-95f9-40fb-b1fe-27ac6f68cac5).

## Version history

Verified on 2026-10-13 using the unchanged bug-tagged target `//tests/app/http-endpoints:protolet_content_type_test`
from `origin/area/app-http-endpoints` at `0696f031f8f04b30c83792b0cb01c602449699c4`.
Tests ran inside disposable fixture-local released containers on hosted runners.

| Release | Published | Result | Bazel invocation | Runner invocation |
| --- | --- | --- | --- | --- |
| enterprise-v2.314.0 | 2026-10-08 | Reproduces | [results](https://app.buildbuddy.io/invocation/ab214fa0-ff83-47c9-9644-59a8b3038d04) | [runner](https://app.buildbuddy.io/invocation/eb049c2d-145f-41cc-81ab-b2990e8c3c2a) |
| enterprise-v2.313.0 | 2026-10-06 | Reproduces | [results](https://app.buildbuddy.io/invocation/6e73827e-18af-442b-9b6f-8c73691a69bd) | [runner](https://app.buildbuddy.io/invocation/db9c3c9c-e3c9-4f4b-8e1e-7a7ac25d9ae4) |
| enterprise-v2.290.0 | 2026-07-29 | Reproduces | [results](https://app.buildbuddy.io/invocation/aacbf37e-1f48-4340-813d-15aed8ee6455) | [runner](https://app.buildbuddy.io/invocation/256d903c-ab1e-4e21-9c3e-853734a3441d) |
| enterprise-v2.275.0 | 2026-06-04 | Reproduces | [results](https://app.buildbuddy.io/invocation/5a6b83d7-3c9e-41c4-bd5a-8ff71cd6d859) | [runner](https://app.buildbuddy.io/invocation/1b9de404-2fca-46e2-9965-535de71d8564) |
| enterprise-v2.260.0 | 2026-04-16 | Reproduces | [results](https://app.buildbuddy.io/invocation/4d91574b-8fda-4cbf-a1b8-0356237b74d0) | [runner](https://app.buildbuddy.io/invocation/fafe222f-5034-4854-b934-fe650f7d8d23) |

Observed in every release marked Reproduces: GetUser returned HTTP 200 with JSON but an empty Content-Type header.

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
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/http-endpoints:protolet_content_type_test \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image='gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843'
```

Each linked invocation ran all 12 in-scope reproducers together, with explicit
app, executor, and cacheproxy digest overrides; unused components were not started.
The final bug filter overrides the default nobug exclusion, and test-result
caching was disabled. Container configs, logs, and inspect metadata are retained
in the invocations' undeclared test outputs.
