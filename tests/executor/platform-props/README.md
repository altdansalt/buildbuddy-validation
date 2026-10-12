# Executor platform properties

These tests schedule real REAPI actions against the fixture-local released app
and bare executor. Each action bypasses the action cache, verifies stdout on
success, and checks status codes on rejection. Properties are sorted by name
and value as REAPI requires. The suite runs a successful action after invalid
requests to check that the service remains usable.

The behavior oracle is the pinned [v2.314.0 platform parser](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go)
(`ParseProperties`, `GetProto`) and [executor overrides](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/executorplatform/executorplatform.go)
(`ApplyOverrides`), together with the [REAPI Action and Platform contracts](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto).

BuildBuddy deliberately normalizes property names and OS/architecture values,
ignores unknown extensions, and uses the last duplicate value in its parser.
REAPI permits server-defined value and duplicate semantics; unknown-property
rejection and action-platform preference are SHOULD requirements. The unknown
extension test records released compatibility behavior, rather than treating
that SHOULD deviation as a confirmed correctness defect.

Coverage includes command fallback, action-only properties, conflicting action
and command properties, a present empty action platform, mixed case names and
OS/architecture values, trimmed values, duplicate images, empty/none/real/malformed
image values with bare isolation, invalid and case-mismatched isolation, invalid
duration, invalid VFS prefetch mode, and invalid base64 environment overrides.

The baseline enables only bare isolation. Image tests assert that a real image
is rejected instead of silently running on the host; they do not exercise Docker
pulls or OCI-reference validation in a Docker provider. Unsupported OS/architecture
scheduling and resource-property parsing remain follow-up work.
