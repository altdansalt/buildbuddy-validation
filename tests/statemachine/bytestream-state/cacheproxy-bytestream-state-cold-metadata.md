# Cold proxy metadata does not observe app publication until read-through

Title: Cold proxy metadata does not observe app publication until read-through
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: Metadata consumers must warm the replica first; no security boundary is violated.
Status: confirmed
Repro test target: //tests/statemachine/bytestream-state:go_default_test --test_arg=-test.run=^TestColdMetadataDivergence$ --test_arg=-test.v
BuildBuddy invocations: https://app.buildbuddy.io/invocation/456f3afc-6cb5-4eac-a1e2-307652ba4406 and https://app.buildbuddy.io/invocation/c19f093e-dfd9-4258-9cd9-a0767725dbfa
Both full targeted suites passed; isolated known-difference probes reproduce the state differences without failing normal CI.

## Expected behavior

Uniform publication would make metadata visible without a content read. This is the suite consistency goal, not a documented read-through metadata contract.

## Actual behavior

App Write and app metadata succeed. Proxy metadata remains NotFound throughout ten seconds; a successful proxy Read materializes metadata and three subsequent observations succeed. No time-only convergence was observed.

## Logs excerpt

```text
cold metadata divergence persisted 10.04994546s without read-through
read-triggered metadata became available after 6.298746ms
read-triggered metadata convergence stable after 309.231811ms
```

## Notes

Default harness configuration. Cache.GetMetadata reads the component-local backing store; proxy ByteStream reads can populate it. Confirmed state difference, not a confirmed product bug. Timestamps and stored sizes belong to each replica and are not required to match across replicas.

The isolated test asserts the known difference so normal CI stays green. It must
be updated if the released behavior changes. No errors are ignored and no
component calls target shared deployments. Harness artifacts include container
configs and actual image IDs. Findings stay in this owned directory per task.

Full expanded suite passed: https://app.buildbuddy.io/invocation/456f3afc-6cb5-4eac-a1e2-307652ba4406

Final pinned-image full-suite verification passed in 217.6 seconds, including exact HTTP absence responses, ZIP JSON fields, required CLI reads and stable-state controls. No shared changes are required.
