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

## Version history

Tested on 2026-10-13 using the unchanged bug-tagged reproducer from
`origin/area/executor-isolation-docker` at `58df56cac5523babb4c474ed9149fd02de20dddb`.
App, Redis, executor and action containers were started inside the test on
hosted `bb remote` runners, using fixture-local endpoints and credentials.
No BuildBuddy component was built from source.

| Enterprise release (release date, UTC) | Result | BuildBuddy invocation |
| --- | --- | --- |
| v2.314.0 (2026-10-08) | Reproduces in both subcases | [runner log, VERSION 2.314.0](https://app.buildbuddy.io/invocation/ad4713a2-76ec-49c0-b1ad-1e8df67bedd2) |
| v2.313.0 (2026-10-06) | Reproduces in both subcases | [runner log, VERSION 2.313.0](https://app.buildbuddy.io/invocation/ad4713a2-76ec-49c0-b1ad-1e8df67bedd2) |
| v2.290.0 (2026-07-29) | Reproduces in both subcases | [runner log, VERSION 2.290.0](https://app.buildbuddy.io/invocation/ad4713a2-76ec-49c0-b1ad-1e8df67bedd2) |
| v2.275.0 (2026-06-04) | Reproduces in both subcases | [runner log, VERSION 2.275.0](https://app.buildbuddy.io/invocation/ad4713a2-76ec-49c0-b1ad-1e8df67bedd2) |
| v2.258.0 (2026-04-09) | Reproduces in both subcases | [runner log, VERSION 2.258.0](https://app.buildbuddy.io/invocation/ad4713a2-76ec-49c0-b1ad-1e8df67bedd2); [independent repeat](https://app.buildbuddy.io/invocation/4d77dfd1-de3f-471f-9f05-dcfcd0815b8a) |

Each release returned OK and exit 0 for both `network=off` alone and combined
with `dockerNetwork=host`, but exposed `eth0` and a default route. Both subcases
failed at `network_bug_test.go:23`, the intended network-isolation assertion.
The April release reproduced in two independent fresh fixtures. The linked
runner log contains separate `VERSION` markers and full Bazel test output for
each release. The script continues after expected test failures, so its outer
exit code is not a passing-test verdict.

**Earliest tested reproducing release: v2.258.0 (2026-04-09).** The defect
was present at or before that release; its actual introduction is unknown.
No earlier release or known-good boundary was tested. The
[v2.258.0 platform documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.258.0/docs/rbe-platforms.md#L361)
already documents `network=off` as localhost-only and says `dockerNetwork` has
no effect when `network` is supplied. This is not an unsupported new property
on the older release.

### Runner configuration and excluded attempts

No release-specific component configuration changes were needed. This area's
private dockerd disables iptables setup; reused runners can retain forwarding
rules from earlier daemons. Initial attempts stopped before actions ran:

- v2.314.0: local app readiness failed because it could not reach fixture-local Redis; [excluded invocation](https://app.buildbuddy.io/invocation/dafc0a44-2a95-4531-a721-7f0f54a15c58).
- v2.313.0: Docker Hub rate limit pulling the fixture Redis image; [excluded invocation](https://app.buildbuddy.io/invocation/b7ac0c34-f0fc-4cb3-ab5b-1ed5083ba61c).
- v2.290.0: local app readiness failed because it could not reach fixture-local Redis; [excluded invocation](https://app.buildbuddy.io/invocation/fee68726-90fc-455c-8f67-a2bfa1e497ea).
- v2.275.0: local app readiness failed because it could not reach fixture-local Redis; [excluded invocation](https://app.buildbuddy.io/invocation/e89a7146-174c-4c03-8fe8-29d2344e6779).
- v2.258.0: local app readiness failed because it could not reach fixture-local Redis; [excluded invocation](https://app.buildbuddy.io/invocation/391b4571-a800-4951-9e9f-163e7caae95d).
- Fresh-runner v2.314.0 retry: Redis pull rate limit; [excluded invocation](https://app.buildbuddy.io/invocation/6a1960e7-96cc-4fc6-9c4a-566f7123ce87).

These are fixture/environment failures, not reproductions or fixed-version
results. To obtain the matrix above without editing tests or harness, a fresh
disposable runner was configured with Google's Docker Hub mirror and bridge
forwarding enabled. The configuration applies to all five releases:

```sh
sudo mkdir -p /etc/docker
printf '%s\n' '{"registry-mirrors":["https://mirror.gcr.io"]}' | sudo tee /etc/docker/daemon.json
sudo iptables -P FORWARD ACCEPT
```

This setup and the following Bazel command template ran inside
`GIT_CONFIG_GLOBAL=/dev/null bb remote --runner_exec_properties=recycle-runner=false --script=...`,
never on the worker VM. The existing Docker Hub image references resolve via
the mirror; BuildBuddy app/executor references remain pinned to GCR digests.
No environment override or dispatch credential was forwarded into the tests.

```sh
bazel test //tests/executor/isolation-docker:network_off_bug \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image=gcr.io/flame-public/buildbuddy-app-enterprise@<app-digest> \
  --test_arg=-harness.executor_image=gcr.io/flame-public/buildbuddy-executor-enterprise@<executor-digest>
```

Allowing forwarding does not create `eth0` in a correctly isolated Docker
`none` network. The failure remains the presence of the configured bridge
interface and route, not a claim about internet connectivity. The existing
finding's independent controls and current-release verification remain intact.

### Exact release pins

Both components were overridden together. These linux/amd64 manifest digests
were resolved from the GCR registry API, and the fixture logs identify the
requested digest references. App repository:
`gcr.io/flame-public/buildbuddy-app-enterprise`; executor repository:
`gcr.io/flame-public/buildbuddy-executor-enterprise`.

| Enterprise release | App digest | Executor digest |
| --- | --- | --- |
| v2.314.0 | `sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` | `sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8` |
| v2.313.0 | `sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6` | `sha256:8f7c2662f5fcd0ab888113f6d36d31cde8b8ea254434a8177cf6996b01155afe` |
| v2.290.0 | `sha256:f3b285107f28da495e9496dd848857ce0cf6cddbf3fac8743b259c2a7a8678b6` | `sha256:2c65048231e21ecf0c60623bc5f8ccaf494e82828077c5064aa994909e15607a` |
| v2.275.0 | `sha256:75ffd43cf6dc14ccf5a652b760d7e4d7390dfa7870524c57e7f354f271e96db1` | `sha256:d2dbe25032467449c80e9eb18002d56b57bcf5247cba267ab886022415b30956` |
| v2.258.0 | `sha256:f7b499d817581e58c1fe826c6441a7773519488d4500dc6ffafad387d2c990c4` | `sha256:500703561ba144c64b12a36f0d3350422f09efe693b8e24592414958ea6753cd` |
