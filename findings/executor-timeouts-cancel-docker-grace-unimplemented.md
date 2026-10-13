# Docker termination grace period cannot deliver SIGTERM

Title: Docker actions accept termination-grace-period but cannot deliver the graceful signal
Components: executor
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Docker actions relying on a TERM handler to flush diagnostics or clean up cannot use the requested grace period. The deadline still force-stops the action; no isolation escape or unbounded execution was observed.
Status: unverified
Repro test target: //tests/executor/timeouts-cancel:docker_graceful_timeout_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/6ee6d2d6-ce9f-48c5-a2d6-9071f67583ae

## Expected behavior

With an action timeout of 1 second and `termination-grace-period=2s`, send SIGTERM at the timeout and allow the installed handler to print `terminated` before force termination. The final response should remain DeadlineExceeded. Alternatively, reject the unsupported combination explicitly rather than accepting a grace period that cannot be honored.

The generic [termination-grace-period platform property](https://github.com/buildbuddy-io/buildbuddy/blob/master/server/util/platform/platform.go) and [executor graceful-termination path](https://github.com/buildbuddy-io/buildbuddy/blob/master/enterprise/server/remote_execution/executor/executor.go) motivate this expectation. Source links describe implementation context, not additional execution evidence or a guaranteed support contract for every isolation provider.

## Actual behavior

Two separate runs returned DeadlineExceeded with exit -2 and stdout `started`, missing `terminated`:

- https://app.buildbuddy.io/invocation/6ee6d2d6-ce9f-48c5-a2d6-9071f67583ae
- https://app.buildbuddy.io/invocation/8eb10322-7b42-48c2-8cad-660852691a80

An enhanced repro with an independent self-TERM control reproduced twice more in https://app.buildbuddy.io/invocation/a1cc7a82-f590-47c6-a621-67693a18798b (both runs). The control returned OK, exit 0 and stdout `startedterminated`; the timed action still returned only `started`. Preserved executor logs identify the cause as an Unimplemented error when sending the graceful signal. Thus the shell supports the handler and the action ran successfully; an image-pull or scheduler failure does not explain these observations.

The final control-enhanced repro also failed with the same Unimplemented log in
https://app.buildbuddy.io/invocation/0d22ef3e-5d75-44bf-b4a0-0eca7ea1be83.

Status remains **unverified**, as requested for round 2. The observed implementation limitation is reproducible; whether Docker grace-period support is an intended product contract needs review. This is not classified as a confirmed product bug.

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
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:docker_graceful_timeout_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

The initial Docker Hub pull-rate-limit failure in https://app.buildbuddy.io/invocation/4c05c724-6ec5-4656-b9bd-a419976e1c90 is excluded from reproduction evidence. The Alpine action image is a tag, whereas both tested BuildBuddy component images are digest pinned. Firecracker and recycled Docker runners are outside this repro. A mitigation is to perform application-level cleanup before the deadline or use an isolation provider that supports graceful signaling.
