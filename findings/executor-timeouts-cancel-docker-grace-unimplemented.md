# Docker termination grace period cannot deliver SIGTERM

Title: Docker actions accept termination-grace-period but cannot deliver the graceful signal
Components: executor
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Docker actions relying on a TERM handler to flush diagnostics or clean up cannot use the requested grace period. The deadline still force-stops the action; no isolation escape or unbounded execution was observed.
Status: confirmed
Repro test target: //tests/executor/timeouts-cancel:docker_graceful_timeout_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/6ee6d2d6-ce9f-48c5-a2d6-9071f67583ae

## Expected behavior

With an action timeout of 1 second and `termination-grace-period=2s`, send SIGTERM at the timeout and allow the installed handler to print `terminated` before force termination. The final response should remain DeadlineExceeded. Alternatively, reject the unsupported combination explicitly rather than accepting a grace period that cannot be honored.

The generic [termination-grace-period platform property](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/server/util/platform/platform.go) and [executor graceful-termination path](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/enterprise/server/remote_execution/executor/executor.go) motivate this expectation. Source links describe implementation context, not additional execution evidence or a guaranteed support contract for every isolation provider.

## Actual behavior

Two separate runs returned DeadlineExceeded with exit -2 and stdout `started`, missing `terminated`:

- https://app.buildbuddy.io/invocation/6ee6d2d6-ce9f-48c5-a2d6-9071f67583ae
- https://app.buildbuddy.io/invocation/8eb10322-7b42-48c2-8cad-660852691a80

An enhanced repro with an independent self-TERM control reproduced twice more in https://app.buildbuddy.io/invocation/a1cc7a82-f590-47c6-a621-67693a18798b (both runs). The control returned OK, exit 0 and stdout `startedterminated`; the timed action still returned only `started`. Preserved executor logs identify the cause as an Unimplemented error when sending the graceful signal. Thus the shell supports the handler and the action ran successfully; an image-pull or scheduler failure does not explain these observations.

The final control-enhanced repro also failed with the same Unimplemented log in
https://app.buildbuddy.io/invocation/0d22ef3e-5d75-44bf-b4a0-0eca7ea1be83.

Independent verification confirms a defect in the documented BuildBuddy grace-period behavior; see Verification below.

## Logs excerpt

```text
control: status=<nil> exit=0; stdout=startedterminated
Sending graceful termination signal
Failed to send graceful termination signal: rpc error: code = Unimplemented desc = not implemented
Command execution returned error: rpc error: code = DeadlineExceeded desc = deadline exceeded
operation: status=code:4 message:"deadline exceeded" exit=-2
expected stdout: "startedterminated"
actual stdout:   "started"
```

## Notes

The test starts released app/executor containers and local Redis on a private Docker daemon inside the hosted test runner. Executor configuration sets `docker_socket=/var/run/docker.sock`, matching host/container workspace paths, and `docker_network=action-network`. Action properties select `workload-isolation-type=docker`, `container-image=docker://mirror.gcr.io/library/alpine:3.21.3`, and `termination-grace-period=2s`. No release overrides or shared service calls are used.

Run:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:docker_graceful_timeout_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

The initial Docker Hub pull-rate-limit failure in https://app.buildbuddy.io/invocation/4c05c724-6ec5-4656-b9bd-a419976e1c90 is excluded from reproduction evidence. The Alpine action image is a tag, whereas both tested BuildBuddy component images are digest pinned. Firecracker and recycled Docker runners are outside this repro. A mitigation is to perform application-level cleanup before the deadline or use an isolation provider that supports graceful signaling.

## Verification

Verdict: **confirmed** (independent review, 2026-10-13). Severity remains **low**:
this prevents timeout handlers from flushing diagnostics or cleaning up, but the
action is still forcibly stopped. No broader availability or isolation impact is
established by this reproducer.

### Contract and implementation

The [v2.314.0 platform documentation](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/docs/rbe-platforms.md#execution-timeout-properties)
defines `termination-grace-period` as the time between **sending a graceful
termination signal** and forcibly stopping the action. It does not restrict this
property to particular isolation providers. The same document explicitly lists
Docker as a supported self-hosted isolation option. Thus this is a documented
BuildBuddy extension, not an assumption that every REAPI server must send TERM.

[REAPI Action.timeout](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L691-L715)
specifies an execution timeout, excluding queue/setup overhead; it does not
require SIGTERM, a grace period, or handler output. DeadlineExceeded is expected
here. The defect is the accepted BuildBuddy property failing to deliver its
promised signal, not the final REAPI status.

At the exact release source commit `974929724a4c75f824ab18fbcb560e48bbf2803a`:

- [Property parsing](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/server/util/platform/platform.go#L495-L498) accepts the duration.
- [Timeout validation](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/enterprise/server/remote_execution/executor/executor.go#L120-L155) validates its range without excluding Docker, and adds it to the force-kill deadline.
- [The termination path](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/enterprise/server/remote_execution/executor/executor.go#L419-L436) calls `GracefulTerminate` at the action deadline and only logs signaling errors.
- [The runner](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/enterprise/server/remote_execution/runner/runner.go#L575-L577) delegates to `Container.Signal(SIGTERM)`.
- [Docker Signal](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/enterprise/server/remote_execution/containers/docker/docker.go#L687-L689) unconditionally returns `Unimplemented`. This is an explicit implementation gap, not evidence of an intended documented Docker exemption.

### Attempts to disprove and independent reproduction

Reviewed PR #92 at commit `44e3fdf6b573cf9e8ad07b24e257308ce5caa579`, including
`docker_grace_test.go`, `docker_helpers_test.go`, and `helpers_test.go`. No test
code was changed. Both runs used the exact app/executor digests recorded above,
confirmed in test output. Each fixture owns its Docker daemon and action network;
the socket and workspace mounts agree with executor configuration. All component
RPCs address the disposable local app, with fixture-created authentication.

The self-TERM action first verifies a working shell handler and stdout retrieval.
The timed action uses the same image and handler; its `started` output establishes
that execution began. The executor then logs `Sending graceful termination
signal` and `Failed to send graceful termination signal: ... Unimplemented`,
followed two seconds later by DeadlineExceeded. Thus image pulling, scheduling,
authentication, Docker PID 1 signal handling, and shell trap syntax do not explain
the missing signal: the provider never attempts to send it. The 2-minute RPC
context outlasts the 1-second action timeout plus 2-second grace period.

The one-second timeout could be sensitive to unusually slow container startup,
and the Alpine action image is tag-selected rather than digest-pinned. Neither
invalidated these runs: the control passed, the timed action printed `started`,
and both produced the implementation-specific signaling error. Pinning the
Alpine digest is a worthwhile follow-up in the originating test PR, but is not
necessary to explain this deterministic provider failure. This verification PR
only changes the finding, as requested.

Independent uncached runs used:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:docker_graceful_timeout_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

The empty final tag filter overrides `--config=nobug` so the bug target actually
runs. Both executions fail at the expected stdout assertion, with the self-TERM
control passing, status code 4, exit -2, stdout `started`, and the same
`Unimplemented` signal error:

- Run 1: https://app.buildbuddy.io/invocation/7bb4b134-dc54-46db-b564-468e4d9be86b
- Run 2: https://app.buildbuddy.io/invocation/2106d18b-f4f2-475e-baf4-56117780865b
