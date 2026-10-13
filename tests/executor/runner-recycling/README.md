# Runner recycling

These tests run released v2.314.0 app/executor containers with authenticated,
fixture-local Remote Execution RPCs and bare isolation. No executor host paths
outside those containers are touched. Each action disables action caching.

`regression` covers observed workspace reuse, removal of undeclared files,
nested directories and symlinks, command environment reset, platform and instance
key separation, nonzero exit cleanup, and timeout workspace/process cleanup.
`tmp_leak` checks per-task TMPDIR cleanup on an observed recycled runner.

The one-second pause after each completed operation allows asynchronous runner
pool finalization; equal workspace paths prove that a same-key check actually
exercised recycling, rather than passing on a fresh runner.

Contract references:

- [v2.314.0 runner keys and recycling](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/runner/runner.go)
- [workspace cleanup](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go)
- [bare runner per-task TMPDIR](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/bare/bare.go)
- [REAPI Command environment and Action timeout](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)

Bare runners explicitly provide no isolation between containers. These tests do
not assert cross-tenant protection for arbitrary absolute `/tmp` paths. The TMPDIR
check specifically tests the provider's documented task-scoped directory.
