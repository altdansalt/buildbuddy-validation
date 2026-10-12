# Executor lifecycle

Tests run released containers exclusively in fixture-local networks:

- registration health (query and header probes), graceful idle shutdown, restart and fresh execution;
- SIGTERM during a command confirmed running by a container-local marker, readiness withdrawal, liveness during lameduck, successful result delivery and graceful exit;
- adjacent-release app/executor skew in both directions (v2.313.0 against pinned v2.314.0);
- invalid build-root startup failure with a useful diagnostic, followed by recovery with the original configuration.

Behavior references at the pinned release:
[executor initialization](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/cmd/executor/executor.go),
[scheduler registration and shutdown](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/scheduling/scheduler_client/scheduler_client.go),
[health checks](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/healthcheck/healthcheck.go), and
[REAPI Execute](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto).
Version skew coverage records observed compatibility of this release pair; REAPI does not promise arbitrary internal scheduler protocol compatibility.

The Docker helper selects only a daemon containing the fixture's exact executor name, supporting both prestarted Docker and the harness's owned daemon. All mutations address that container. The test does not create additional containers or access a shared deployment.
