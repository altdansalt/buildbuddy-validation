# Cache proxy advertises an Execution service it does not implement

Title: Cache proxy reports exec_enabled=true but Execute returns unknown service
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: Incorrect service discovery can mislead generic capabilities-driven clients, but the documented proxy usage is cache-only with a separate execution endpoint. Supported cache workflows remain available; no execution regression at the app, data loss or cross-group access is demonstrated. Independent review lowers medium to low for this limited impact.
Status: confirmed
Repro test target: //tests/app/capabilities:proxy_execution_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/4c9c5e0a-0206-4213-8424-0b9492e852b2

## Expected behavior

The [REAPI ExecutionCapabilities contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) defines `exec_enabled` as whether remote execution is enabled for the particular server/instance. An endpoint without an Execution service should omit execution capabilities or report `exec_enabled=false`, rather than forward the upstream app's execution advertisement unchanged.

## Actual behavior

The proxy returns the upstream app's `execution_capabilities` with `exec_enabled=true`, SHA256 and additional digest functions, and priority range -1000..1000. An Execute request to that proxy returns `UNIMPLEMENTED` with `unknown service build.bazel.remote.execution.v2.Execution`.

Two separate uncached reproductions failed identically:

- https://app.buildbuddy.io/invocation/4c9c5e0a-0206-4213-8424-0b9492e852b2
- https://app.buildbuddy.io/invocation/2caca354-b4cb-42ff-950e-e36152ea9d82

Both tests first probe the upstream app with the same authenticated Execute request and assert it does not return UNIMPLEMENTED. No executor is needed: a deliberately missing action probes RPC availability without scheduling. Fixture readiness and local API-key bootstrap succeed. All RPCs target the isolated fixture's loopback endpoints.

## Logs excerpt

```text
TestProxyExecutionAdvertisement
exec_enabled=true but Execute is unavailable:
rpc error: code = Unimplemented desc = unknown service build.bazel.remote.execution.v2.Execution
```

This is the relevant client RPC/test excerpt. Container stdout/stderr and inspect/config artifacts are preserved by the fixture in test undeclared outputs.

## Notes

Uses default harness configuration: local app with remote execution enabled, local Redis, SQLite and Pebble, and a customer cache proxy authenticated against that app. No image overrides or external services.

Released source explains the result: [CapabilitiesServerProxy.GetCapabilities](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/capabilities_server_proxy/capabilities_server_proxy.go) returns the upstream response without filtering; [registerGRPCServices](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/cmd/cache_proxy/cache_proxy.go) registers cache services but no Execution service. This rules out missing executor registration or harness routing as the cause. A mitigation is to use the app endpoint for execution and the proxy only for caching.

Run with `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:proxy_execution_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all`. Independent verification is recorded below. Findings remain in this repository; no upstream report.

## Verification

Independent review of PR #32 at `22f3bca` against BuildBuddy
`v2.314.0` (source commit `974929724a4c75f824ab18fbcb560e48bbf2803a`):
**confirmed**. The exact pinned app/proxy image digests are listed above and
were printed by both reviewer runs. Tests were run unchanged from the PR;
this verification branch copies only the findings because PR #32 is not merged.

Reviewer command (both targets, twice, with fresh test execution):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:batch_limit_bug_test //tests/app/capabilities:proxy_execution_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- Run 1: [test invocation](https://app.buildbuddy.io/invocation/f3779d64-4048-41c3-b6ee-de8f5478732c), [runner invocation](https://app.buildbuddy.io/invocation/2d28d947-5f63-46f1-a5c7-b25914ed7276).
- Run 2: [test invocation](https://app.buildbuddy.io/invocation/8ab24a09-29c2-4a52-89c2-01a0062b0414), [runner invocation](https://app.buildbuddy.io/invocation/7964102a-8885-40ec-9adb-997526fb53aa).

The decisive [REAPI GetCapabilities contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L636-L643)
says only capabilities of services supported by the queried endpoint are
returned, and explicitly says CAS + Action Cache endpoints should return
CacheCapabilities. [exec_enabled](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L2577-L2578)
also refers to the particular server/instance, not an upstream server.

The mundane explanation is intentional cache-only deployment. The
[pinned proxy documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-proxy.md#L81-L83)
indeed prescribes `--remote_cache` and `--executor.cache_target`, not using the
proxy as an execution endpoint. This explains why Execute is absent and limits
severity; it does not justify advertising an absent service. The defect is in
capabilities, not a requirement to add execution forwarding.
[Proxy forwarding](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/capabilities_server_proxy/capabilities_server_proxy.go#L42-L50)
returns the upstream response unchanged, while
[service registration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/cmd/cache_proxy/cache_proxy.go#L382-L388)
contains no Execution service.

In both runs, the authenticated upstream control passed, then the proxy returned
`UNIMPLEMENTED: unknown service build.bazel.remote.execution.v2.Execution`.
The empty instance is shared by capabilities and Execute. A missing action is a
valid service-availability probe: missing data/workers could yield precondition
or not-found failures but cannot explain an unknown gRPC service. The test does
not need an executor or assert successful scheduling. Fixture-local endpoints,
readiness and authentication succeeded, and static service registration explains
the repeatable result. Severity is reduced from medium to low because documented
cache-only usage remains functional and generic discovery is the demonstrated
impact.
