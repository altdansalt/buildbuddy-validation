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
