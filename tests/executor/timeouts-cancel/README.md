# Executor timeouts and cancellation

Tests run shell actions on the digest-pinned released executor, with the
harness's local app/Redis. Bare actions run inside the executor; Docker actions
use a test-owned daemon, socket, network and matching workspace mounts. Docker
controls address only that daemon. No shared deployment is used. Timeout duration checks use executor metadata, so
fixture startup and scheduler queue time do not enter the assertion.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel/... --config=nobug
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:graceful_timeout_child_test --test_tag_filters=bug --nocache_test_results
```

The regression target covers explicit/default timeouts, maximum-timeout
rejection, partial stdout, normal nonzero exit status, self-signaled actions,
graceful TERM handlers that exit zero, forced termination when TERM is ignored,
ordinary descendant cleanup, Execute context cancellation, connection closure,
and retrieval with WaitExecution after either disconnection. It also checks
preserved app/executor logs for Go panics and fatal runtime errors.

Canceling an Execute subscription is distinct from canceling the underlying
operation. The REAPI makes Operations.CancelOperation optional. These tests
exercise subscription cancellation and disconnect recovery. Round 2 also sends
`CancelExecutions` through the fixture-local UI API for three distinct actions
sharing REAPI `RequestMetadata.tool_invocation_id`, in both bare and Docker
isolation. A minimal BES started event with nonempty options first provisions
the invocation row; the send stream closes before collecting its acknowledgment.
The tests verify all three responses are Canceled, an unrelated action completes,
canceled processes/containers stop, and subsequent execution succeeds. Physical
cleanup can lag the canceled response until the default 10-second task lease
renews; the probes allow up to 25 seconds for cleanup. The private daemon uses
the GCR registry mirror for the harness's digest-pinned Redis sidecar as well.

The bug target isolates a graceful-timeout parent that exits zero on TERM while
its child ignores TERM and later writes a marker. Both the marker and the probe
are inside the same test-local executor. Child stdio is redirected so inherited
pipes do not keep the parent command's output collection open. The subsequent
probe waits beyond the child's planned write, then reads/removes any marker.
See `findings/executor-timeouts-cancel-graceful-orphan.md`.

Contract references:

- [REAPI Execute and Action.timeout](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
- [Released executor timeout and error handling](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/executor/executor.go)
- [Released Unix process tree cleanup](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil_unix.go)
- [Released termination-grace-period property](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go)

Round 2 Docker coverage checks ordinary and TERM-ignoring deadlines, preserved
partial stdout, and signal/nonzero-exit reporting. The OOM case starts a single
allocation action, applies a 64 MiB cgroup limit through its private daemon, and
requires a Docker OOM event naming that action container. It never constrains the
executor or runner. The signal checks kill a child shell and propagate its status, avoiding Linux
PID-1 self-signal protection. Docker reports these SIGTERM actions as exit 143;
SIGKILL, real OOM and an
explicit `exit 137` all report exit 137 with OK RPC status. These are failed action
results, not successful builds. A numeric exit code alone does not distinguish
OOM from SIGKILL or a deliberate nonzero exit. Bare self-signals retain the
existing non-OK, negative-exit assertions.

The additional bug target checks `termination-grace-period=2s` with a 1-second
Docker action timeout. A self-TERM control verifies the image's handler works;
the deadline case fails because Docker signaling returns Unimplemented. See
`findings/executor-timeouts-cancel-docker-grace-unimplemented.md` (unverified).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:docker_graceful_timeout_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

Limits: Linux bare and non-recycled Docker runners; Firecracker, executor restart,
and recycled-runner signal classification remain outside this area coverage.
Docker action images use `mirror.gcr.io/library/alpine:3.21.3` to avoid Docker Hub
pull rate limits. The OOM test applies its memory limit after the container starts;
it validates reporting, not enforcement of an executor platform memory property.

Round 3 adds `TestSeededProcessTreeTermination`: eight generated process trees
per isolation mode, alternating action deadlines and invocation cancellations.
The default seed is 314003; replay another corpus with
`--test_arg=-tree_seed=123 --nocache_test_results`. Each case logs its seed,
index, isolation, delay and exact shell script. Trees have 3–7 shell nodes,
random parent edges, background children, guaranteed grandchildren and a
session-detached TERM ignorer, plus randomized TERM disposition and detachment.
All nodes continuously append to a declared `partial.txt` output. Deadlines and
cancellation delays vary from 1.2–2.499 seconds. Cancellation first independently
observes the action's unique inherited environment token in live processes.

The property requires DeadlineExceeded or Canceled as appropriate; partial
stdout/files cannot turn an interrupted action into success. Returned partial
files are fetched and inspected. Bare cleanup scans `/proc/*/environ` inside
only the fixture executor, including reparented and detached descendants;
zombies have no executable state/environment and are excluded. Docker cleanup
requires no running action containers on the fixture-owned daemon, which also
removes their process namespaces. Both checks are repeated after a short delay.
Cancellation can publish its response before lease renewal, so physical cleanup
has the same 25-second bound as round 2. This checks eventual cleanup, not an
instantaneous guarantee at terminal-operation delivery.

Known exceptions are kept in their separate bug targets: randomized cases omit
nonzero termination grace periods, avoiding the graceful-parent-orphan defect
and Docker's unimplemented graceful signaling. The strict hard-stop cleanup
invariant still applies to every generated tree. No graceful-orphan survivor is
silently accepted by the randomized oracle.
