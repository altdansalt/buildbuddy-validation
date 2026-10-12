# Executor resource regressions

Run on the hosted runner:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/resources/... --config=nobug
```

All actions address the fixture's local app and execute in its digest-pinned
enterprise-v2.314.0 executor. No shared component is queried. Each action bypasses
cache lookup and carries a unique test-name comment, including concurrent actions,
so execution merging cannot substitute for concurrency coverage.

Coverage:

- CPU cores, millicores, scientific notation; IEC and numeric memory/disk sizes.
- Tiny, zero, negative and malformed estimates execute without server errors.
- Thirty-two child processes complete and are reaped; four distinct actions run
  concurrently with small estimates.
- A bounded 8 MiB workspace write exceeds a one-byte disk estimate successfully.
- An action-local 64 MiB virtual-address limit causes an allocator failure; the
  failure is returned as a tool exit code, and a subsequent action succeeds.
- A 16 MiB workspace tmpfs causes ENOSPC during a bounded 32 MiB write; the tool
  error and stderr are preserved, and workspace cleanup allows a fresh 4 MiB write.

The test asserts protocol-visible results, not exact scheduler reservations or
CPU throttling. Memory exhaustion uses RLIMIT_AS, **not** an executor/cgroup OOM
kill. It does not claim to cover OCI or Firecracker resource enforcement. The
bare provider does not implement per-action cgroup constraints. The dedicated
tmpfs affects only executor root_directory; CAS and file cache retain separate
storage. No whole-runner disk fill or fork bomb is used.

Contract references at the released source tag:

- [Platform parsing](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go):
  `milliCPUProp`, `iecBytesProp`, `EstimatedFreeDiskBytes`. Malformed estimates
  currently use defaults, so rejection is not asserted.
- [Task sizing](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/tasksize/tasksize.go):
  `Requested`, `Override`, `ApplyLimits`, `GetCgroupSettings`. Positive estimates
  override defaults; nonpositive values fall back to defaults/minima.
- [Bare provider](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/bare/bare.go):
  executes directly with process statistics and no container-level isolation.
- [REAPI](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto):
  `ExecuteResponse.status` and `ActionResult.exit_code` distinguish service errors
  from unsuccessful commands. Platform properties are implementation-defined.
