# Docker actions retain networking with network=off

Title: Docker isolation ignores network=off and retains the configured bridge
Components: executor
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8
Severity: high
Severity justification: An explicit network isolation restriction is silently ignored. When the configured Docker network permits connectivity, an action intended to run without networking retains that connectivity, potentially exposing reachable services or allowing data exfiltration. Requires Docker isolation and a network-enabled default; this test verifies the attached bridge and route, not external exfiltration.
Status: confirmed
Repro test target: //tests/executor/isolation-docker:network_off_bug
BuildBuddy invocation: https://app.buildbuddy.io/invocation/100da40a-c8be-460a-aa8f-1c0e700e65d1

## Expected behavior

The released [platform property contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go#L160)
defines `network=off` as no network access and says it takes precedence over
`dockerNetwork`. Docker actions should use network mode `none`, exposing only
loopback and no bridge route, both with `dockerNetwork` absent and when the
command also specifies `dockerNetwork=host`.

The [REAPI Command platform](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)
is an implementation-defined execution environment contract. This finding concerns
BuildBuddy's documented extension rather than a REAPI-mandated networking mode.

## Actual behavior

Both cases complete with OK status and exit code 0, but `ls /sys/class/net`
reports `eth0` and `lo`, and `/proc/net/route` includes a default route on `eth0`.
The configured `action-network` bridge remains attached.

Two independent, uncached explicit reproductions fail on the interface assertion:

- [Run 1](https://app.buildbuddy.io/invocation/100da40a-c8be-460a-aa8f-1c0e700e65d1)
- [Run 2](https://app.buildbuddy.io/invocation/d8eb319f-9336-47b0-a74d-f45d978d9b07)

The same fixture's [passing regression suite](https://app.buildbuddy.io/invocation/73dbab49-1d53-4a7f-b508-63f8fc9b6179)
shows that `dockerNetwork=off` correctly removes `eth0`, invalid network values
return INVALID_ARGUMENT, and workdir/input/output mounts behave correctly.
Each fixture owns its daemon, socket, bridge, app, Redis, and released executor.
No shared deployment or external network probe is involved. An initial workspace
path configuration mistake was corrected before these two explicit reproductions;
it is not the source of this finding.

Source inspection explains the difference:
[GetEffectiveDockerNetwork](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go#L921)
converts `network=off` to `"none"`, while
[Docker hostConfig](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/docker/docker.go#L364)
handles only `"off"` and `"bridge"`. `"none"` falls through to the configured default.

## Logs excerpt

```text
--- FAIL: TestNetworkOff/dockerNetwork=
status=<nil> exit=0
network=off container interfaces and routes:
    eth0
    lo
    Iface Destination Gateway Flags ...
    eth0  00000000    01E8340A 0003  ...
    eth0  00E8340A    00000000 0001  ...
"...eth0..." should not contain "eth0"
Messages: network=off must disable the configured bridge
```

The second run reproduces the same output with a different randomly allocated subnet.
Full fixture logs, configs, inspect metadata, and the area-owned daemon log are
preserved in Bazel undeclared outputs.

## Notes

Reproduce with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-docker:network_off_bug --test_tag_filters=bug --nocache_test_results --test_output=all
```

The local app is also pinned to enterprise-v2.314.0:
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
Actions use `docker://alpine:3.21.3`, `workload-isolation-type=docker`, Linux/amd64,
and fresh, non-recycled runners. The executor uses its private socket,
`executor.docker_network=action-network`, and explicitly aligned host/build-root
bind mounts. The private daemon has no default bridge and disables iptables and
IP masquerading; the observed configured bridge has its own random private pool.
This limits the test's connectivity without changing Docker's `none` semantics.

Workaround: use `dockerNetwork=off` without `network=off`. A likely fix is to
handle the translated `"none"` value in Docker's network-mode switch. Podman and
OCI behavior have not been evaluated. Independent verification below confirms
the defect; findings remain in this repository.

## Verification

Verdict: **confirmed** (independent review, 2026-10-13). Reviewed PR #31 at
`58df56cac5523babb4c474ed9149fd02de20dddb`, including the bug target, fixture,
request construction, and passing controls. Only this finding is copied into the
verification branch; the reproducer remains in PR #31 until that PR is merged.

Two fresh remote executions, with test-result caching disabled, reproduced both
subcases: `network=off` alone and combined with `dockerNetwork=host`. All four
actions returned OK and exit code 0, but exposed `eth0` plus a default route.
The independent fixtures allocated different subnets (`10.162.135.0/28` and
`10.225.105.0/28`), so this is not a stale action result or a fixed subnet artifact.
Both runs logged the exact pinned app and executor digests recorded above.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-docker:network_off_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Verification run 1, failing assertions](https://app.buildbuddy.io/invocation/5fc29151-c9d3-45ed-a982-d9d1a7997fec)
  ([remote runner](https://app.buildbuddy.io/invocation/8fa241af-a91d-4126-9b53-bf1b8099e396)).
- [Verification run 2, failing assertions](https://app.buildbuddy.io/invocation/182c2b67-ab36-4048-9b42-d6d8d7183c7b)
  ([remote runner](https://app.buildbuddy.io/invocation/81154a27-6520-479f-9b59-226d4bf1a963)).

### Attempts to disprove

- **Intended behavior:** the pinned [platform documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-platforms.md#L361-L375)
  promises that `network=off` has only localhost access. Its
  [legacy property documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-platforms.md#L393-L400)
  explicitly says `dockerNetwork` has no effect when `network` is specified.
  A configured bridge is therefore not an intended override of the restriction.
- **REAPI interpretation:** [Platform](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L913-L968)
  leaves property meanings to the server; this is a BuildBuddy contract defect,
  not a universal REAPI networking requirement. Properties are sorted correctly.
  Although `Command.platform` is deprecated, the pinned
  [GetProto fallback](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go#L833-L841)
  explicitly supports it when `Action.platform` is absent. The test exercises
  this supported path, rather than supplying an ignored field.
- **Source explanation:** at release commit
  `974929724a4c75f824ab18fbcb560e48bbf2803a`,
  [GetEffectiveDockerNetwork](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go#L921-L940)
  returns `none` for `network=off`, regardless of the legacy value.
  [Provider.New](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/docker/docker.go#L119-L137)
  passes this value into DockerOptions. The
  [hostConfig switch](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/docker/docker.go#L364-L379)
  does not handle `none`, so it retains `executor.docker_network=action-network`.
  This deterministic mismatch explains both failures.
- **Harness/config/flakiness:** the fixture owns its daemon, mounts its socket into
  the executor, explicitly selects Docker isolation, and aligns workspace paths.
  Successful commands rule out startup or mount failures. Disabling daemon
  iptables and masquerading does not make a bridge equivalent to Docker's `none`:
  the latter removes the non-loopback interface. The assertion is appropriate for
  this controlled Alpine Docker environment; it does not claim to prove internet
  connectivity. Actions disable both cache lookup and result caching, and do not
  request recycled runners. Both independent runs failed at the intended assertion.

Severity remains **high**: silently retaining a configured network defeats an
explicit isolation restriction and can expose whatever that network can reach.
This is conditional on the executor's default network connectivity. Neither the
reproducer nor this review establishes external exfiltration, shared deployment
exposure, or effects on OCI, Podman, or Firecracker.

Independent negative/positive controls also passed:
[control invocation](https://app.buildbuddy.io/invocation/5b2e1e2c-346b-4902-9c7d-dc75f715b404)
([remote runner](https://app.buildbuddy.io/invocation/76f7ccc2-96c3-4b2d-8a14-9be242c9d902)).
The same fixture exposes `eth0` with networking enabled, removes it with
`dockerNetwork=off`, and returns INVALID_ARGUMENT for `network=invalid`.
This independently validates the isolation oracle and property parsing.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-docker:go_default_test --config=nobug --test_filter='TestDockerIsolation/(network_on|dockerNetwork_off|invalid_network)$' --nocache_test_results --test_output=all
```
