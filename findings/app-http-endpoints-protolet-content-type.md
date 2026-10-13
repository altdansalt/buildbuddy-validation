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

Tested on 2026-10-13 against five released linux/amd64 app images, spanning
2026-04-16 through 2026-10-08. The unchanged bug-tagged target
`//tests/app/http-endpoints:protolet_content_type_test` from PR branch
`area/app-http-endpoints` at `0696f031f8f04b30c83792b0cb01c602449699c4` ran once per release
with fresh fixture-local containers and test-result caching disabled.

| Enterprise release | Release date | GCR manifest digest | Result | BuildBuddy invocation |
| --- | --- | --- | --- | --- |
| 2.314.0 | 2026-10-08 | `sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/21e1b4c9-c4dd-494d-999e-044e17a596f7) ([runner](https://app.buildbuddy.io/invocation/151cd06c-5580-45ac-aa18-4e7b1214d00e)) |
| 2.313.0 | 2026-10-06 | `sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/1f181914-01c6-42e2-968f-9b2c1a7e454f) ([runner](https://app.buildbuddy.io/invocation/06473ca1-fe24-44b8-8b37-33d411626e44)) |
| 2.300.0 | 2026-09-01 | `sha256:03bd9ccb5888936beb301a577bf01560511cded6fa909079cbb53277aa32ed68` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/eeb7da86-8c3b-47ee-a572-b58fc15141bc) ([runner](https://app.buildbuddy.io/invocation/04a7dd4a-fbdd-4674-9a34-fea7ff5e7256)) |
| 2.280.0 | 2026-06-23 | `sha256:d9fbae912f786a0f95d351ffb7c580a6ecb8b1fb636fd3fe8899bf1cb8350a35` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/8127dd8b-c03d-4407-9120-d6a10629c05c) ([runner](https://app.buildbuddy.io/invocation/bcfec8a6-fc2c-4985-915d-d9bb74bda922)) |
| 2.260.0 | 2026-04-16 | `sha256:0f6376865cc9c92d0e837aa1cb53717b59875db68a1b09b6dd479c571f6ab88e` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/9216fc0f-055a-40aa-a9c2-d0ef67770b98) ([runner](https://app.buildbuddy.io/invocation/8c8f24c4-d6eb-41aa-b3dc-24a022bedab3)) |

All image references use
`gcr.io/flame-public/buildbuddy-app-enterprise@<digest>`; digests were resolved
from the GCR manifest API for the corresponding `enterprise-v<version>` tags.
The 2.314.0 digest matches the repository default.

Observed on every tested version: The successful authenticated GetUser JSON response has an empty Content-Type instead of application/json.

**Earliest tested release reproducing: 2.260.0 (2026-04-16).**
The defect was present by that release; the introduction version is unknown.
All sampled releases reproduce, so this matrix does not establish an
introduction interval or imply every intervening release was tested.

No older release required startup configuration changes. All five reached
the intended product assertion after fixture startup and authentication.

Command pattern (run from the repro branch; substitute the digest above):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote --runner_exec_properties=init-dockerd=true test \
  //tests/app/http-endpoints:protolet_content_type_test \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image=gcr.io/flame-public/buildbuddy-app-enterprise@<digest>
```
