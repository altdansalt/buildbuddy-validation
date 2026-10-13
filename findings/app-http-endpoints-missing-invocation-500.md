# Artifact download reports missing invocation as HTTP 500

Title: Artifact download reports missing invocation as HTTP 500
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Ordinary missing or expired invocation links become apparent server failures, affecting client error handling and error-rate monitoring. No data loss or authorization bypass was observed.
Status: confirmed
Repro test target: //tests/app/http-endpoints:missing_invocation_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/9f22f187-f8c6-4a8c-9c94-a3104bfc2d16

## Expected behavior

A nonexistent invocation should return HTTP 404, not a server error. The
[released serveArtifact handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/buildbuddy_server/buildbuddy_server.go)
explicitly has a NotFound-to-404 branch for invocation lookup. See
[RFC 9110 section 15.5.5](https://www.rfc-editor.org/rfc/rfc9110.html#section-15.5.5).

## Actual behavior

Cookie-authenticated `GET /file/download?artifact=build_log&invocation_id=00000000-0000-0000-0000-000000000000`
returns HTTP 500 and `rpc error: code = Internal desc = Internal server error`.
Both isolated reproductions fail on the expected 404 assertion.

Each test starts a fresh app and SQLite database, and does not create this
invocation. Readiness, user provisioning and API key creation all succeed before
the request. This path does not require a remote cache, executor, external service,
or existing log blob: invocation lookup occurs before artifact dispatch.

The [released InvocationDB.LookupInvocation implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/backends/invocationdb/invocationdb.go)
returns the query Take error directly. The HTTP handler only recognizes gRPC
NotFound errors, making a database record-not-found error fall into the generic
Internal branch. This is source-based diagnosis, not a claim about other DB backends.

Reproductions: [run 1](https://app.buildbuddy.io/invocation/9f22f187-f8c6-4a8c-9c94-a3104bfc2d16), [run 2](https://app.buildbuddy.io/invocation/2e8708a8-4639-4ffb-a9ce-b4fa36cf5ab4). Both ran with
`--test_tag_filters=bug --nocache_test_results` and failed as expected.
Independent review verdict and additional reproductions are recorded below.

## Logs excerpt

```text
nonexistent invocation: HTTP 500; want 404; body=rpc error: code = Internal desc = Internal server error
```

This excerpt is from the Bazel test log. Full container logs, configs and inspect
metadata are retained by the fixture in undeclared test outputs.

## Notes

Default harness app configuration: SQLite, local Pebble cache and storage,
self-auth, disposable local group and API key, fixture-local Redis. No image
version overrides. Tests run with `bb remote --runner_exec_properties=init-dockerd=true`.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote --runner_exec_properties=init-dockerd=true test //tests/app/http-endpoints:missing_invocation_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

Also observed through the missing-CAS artifact fallback during exploratory coverage. The minimal reproducer uses the direct artifact path to eliminate ByteStream authentication and fallback configuration as possible causes.
Findings remain in this repository; no upstream report.

## Verification

Independent review (2026-10-13): **confirmed**, severity **low** retained. An
ordinary absent invocation is misclassified as an internal server failure. The
impact shown is incorrect HTTP error handling/monitoring, not data loss or an
authorization bypass. The fresh fixture database has no invocation with the
specified ID; authenticated login and group/key provisioning succeed first.

Reviewed PR #54's bug-tagged test and pinned v2.314.0 source (commit
`974929724a4c75f824ab18fbcb560e48bbf2803a`).
[ServeHTTP and serveArtifact](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/buildbuddy_server/buildbuddy_server.go#L2611)
route any nonempty artifact parameter to invocation lookup and explicitly map
gRPC NotFound to HTTP 404 before artifact dispatch.
[LookupInvocation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/backends/invocationdb/invocationdb.go#L179)
returns the database error unchanged;
[rawQuery.Take](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/db/db.go#L986)
returns GORM's error directly, while
[IsNotFoundError](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/status/status.go#L169)
only checks gRPC status. A missing database row therefore takes the generic 500
branch. No blob fetch, executor, or external cache is involved.

Potential mundane explanation checked: the test spells the artifact `build_log`,
whereas the released handler and
[UI client](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/app/service/rpc_service.ts#L183)
use `buildlog` (with an attempt parameter). This is not a valid build-log download
example for an existing invocation. It does not invalidate this *missing-invocation*
reproducer: invocation lookup and its intended 404 happen before the artifact
switch or attempt parsing. The failure is the lookup error mapping regardless
of that later validation. A future test cleanup can use the canonical spelling
and `attempt=0`; this verification changes finding files only.

[RFC 9110 section 15.5.5](https://www.rfc-editor.org/rfc/rfc9110.html#section-15.5.5)
supports 404 for a missing resource; the strongest intended-behavior evidence is
the explicit 404 branch in this very handler. The released docs do not specify
500 as intentional for missing invocations.
[bazelbuild/remote-apis](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
does not define BuildBuddy's invocation artifact HTTP endpoint, so no REAPI
cache-miss requirement is being applied to it. The fixture uses supported
SQLite/local storage configuration and an HTTP client bounded by a 30-second
timeout, whose transport addresses only the fixture container.

Two independent uncached reviewer runs used the exact pinned app digest above
and fresh containers/databases. Both reached the assertion with HTTP 500 and
`rpc error: code = Internal desc = Internal server error`; neither failed on
readiness, authentication, image pull or networking. No flakiness was observed.
Commands were run from PR #54's checkout with no test/image/config changes:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/http-endpoints:protolet_content_type_test //tests/app/http-endpoints:missing_invocation_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Reviewer invocation links: [run 1](https://app.buildbuddy.io/invocation/6d09348a-6c65-411a-95ea-5d19e65d64eb),
[run 2](https://app.buildbuddy.io/invocation/31ee20ab-95f9-40fb-b1fe-27ac6f68cac5).

## Version history

Verified on 2026-10-13 using the unchanged bug-tagged target `//tests/app/http-endpoints:missing_invocation_test`
from `origin/area/app-http-endpoints` at `0696f031f8f04b30c83792b0cb01c602449699c4`.
Tests ran inside disposable fixture-local released containers on hosted runners.

| Release | Published | Result | Bazel invocation | Runner invocation |
| --- | --- | --- | --- | --- |
| enterprise-v2.314.0 | 2026-10-08 | Reproduces | [results](https://app.buildbuddy.io/invocation/ab214fa0-ff83-47c9-9644-59a8b3038d04) | [runner](https://app.buildbuddy.io/invocation/eb049c2d-145f-41cc-81ab-b2990e8c3c2a) |
| enterprise-v2.313.0 | 2026-10-06 | Reproduces | [results](https://app.buildbuddy.io/invocation/6e73827e-18af-442b-9b6f-8c73691a69bd) | [runner](https://app.buildbuddy.io/invocation/db9c3c9c-e3c9-4f4b-8e1e-7a7ac25d9ae4) |
| enterprise-v2.290.0 | 2026-07-29 | Reproduces | [results](https://app.buildbuddy.io/invocation/aacbf37e-1f48-4340-813d-15aed8ee6455) | [runner](https://app.buildbuddy.io/invocation/256d903c-ab1e-4e21-9c3e-853734a3441d) |
| enterprise-v2.275.0 | 2026-06-04 | Reproduces | [results](https://app.buildbuddy.io/invocation/5a6b83d7-3c9e-41c4-bd5a-8ff71cd6d859) | [runner](https://app.buildbuddy.io/invocation/1b9de404-2fca-46e2-9965-535de71d8564) |
| enterprise-v2.260.0 | 2026-04-16 | Reproduces | [results](https://app.buildbuddy.io/invocation/4d91574b-8fda-4cbf-a1b8-0356237b74d0) | [runner](https://app.buildbuddy.io/invocation/fafe222f-5034-4854-b934-fe650f7d8d23) |

Observed in every release marked Reproduces: Downloading a nonexistent invocation returned HTTP 500 instead of 404.

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
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/http-endpoints:missing_invocation_test \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image='gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843'
```

Each linked invocation ran all 12 in-scope reproducers together, with explicit
app, executor, and cacheproxy digest overrides; unused components were not started.
The final bug filter overrides the default nobug exclusion, and test-result
caching was disabled. Container configs, logs, and inspect metadata are retained
in the invocations' undeclared test outputs.
