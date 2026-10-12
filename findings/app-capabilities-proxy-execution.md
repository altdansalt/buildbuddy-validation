# Cache proxy advertises an Execution service it does not implement

Title: Cache proxy reports exec_enabled=true but Execute returns unknown service
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: medium
Severity justification: An authenticated client using the proxy endpoint for capabilities-driven remote execution receives a false positive and cannot execute. Cache operations remain available; no data loss or cross-group access is implicated.
Status: unverified
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

Run with `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:proxy_execution_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all`. Status remains unverified for maintainer review, as requested. Findings remain in this repository; no upstream report.
