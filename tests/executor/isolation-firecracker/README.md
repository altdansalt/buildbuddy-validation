# Firecracker isolation and bare-runner fallback

The baseline hosted runner has no `/dev/kvm`, as recorded in
[environment.md](../../../docs/environment.md#verified-docker-runner-properties).
Privileged Docker alone cannot supply nested virtualization. Firecracker
execution therefore requires a different runner exposing KVM, executor device
passthrough, and an enabled Firecracker provider. No Firecracker startup failure
on this runner is classified as a product defect. The test logs its own runner's
KVM device availability; existence alone would not establish usability.

These tests exercise the supported `workload-isolation-type=none` fallback,
using only app/executor containers started by the harness:

* Non-recycled actions: input restoration, distinct workspaces and TMPDIRs,
  asynchronous cleanup of both directories, and no command environment leakage.
* Recycled actions with preservation disabled: actual workspace reuse is required;
  mutated inputs, undeclared nested files, and symlinks must not survive.

The tests deliberately do not expect isolation of arbitrary absolute paths or
host processes: the released bare provider explicitly provides no container
isolation. All filesystem probes run inside the disposable executor container.
No runner credentials are passed into it.

Contracts consulted:

* [Pinned bare provider](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/bare/bare.go):
  generated TMPDIR and removal on non-recycled container teardown.
* [Pinned runner](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/runner/runner.go)
  and [workspace cleanup](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go):
  recycling invokes workspace cleanup; without preservation all children are removed.
* [REAPI Command](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto):
  command environment and input-root-relative working directory. Recycling is a
  BuildBuddy extension, so its cleanup expectations come from the pinned source.

Run with `bb remote test //tests/executor/isolation-firecracker/... --config=nobug`.
