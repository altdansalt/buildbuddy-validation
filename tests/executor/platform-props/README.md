# Executor platform properties

These tests schedule real REAPI actions against the fixture-local released app
and bare executor. Baseline actions bypass the action cache, verify stdout on
success, and check status codes on rejection. Round 2 also checks cache replay. Properties are sorted by name
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
duration, invalid VFS prefetch mode, and invalid base64 environment overrides, and unavailable OS/architecture pools.
Unsupported isolation and absent machine pools return scheduler UNAVAILABLE,
while executor/parser validation failures return INVALID_ARGUMENT.

The baseline enables only bare isolation. Round 2 starts a separate private
Docker daemon and enables Docker isolation with shared workspace paths and a
mounted socket; it requires no privileged executor. Image-reference coverage
checks tags, digests, tag-plus-digest references (including a nonexistent tag
with a valid digest), malformed references, and a fixture-local denying registry
with an explicit port. Registry denial is a parsing/reachability oracle, not a
successful-pull oracle. Alpine 3.21.3 is the action image; its resolved digest is
obtained from the test's own Docker daemon.

Resource estimates cover fractional, scientific, negative, nonfinite, malformed,
millicore, and IEC-like values. The released parser deliberately defaults on
parse errors. A 1e3 CPU estimate must be rejected with the parsed 1,000,000
milliCPU quantity because it cannot fit this runner. Other cases assert
execution remains usable; they do not assert
resource enforcement or infer scheduling quantities from execution success.

A seeded permutation (`20261012`) varies `env-overrides` while preserving the
command and other properties. For both Action.Platform and Command.Platform,
each distinct value must execute freshly and produce its own output; a replay
must hit the cache. Docker tests also require independent cache entries for
image-reference changes and `dockerUser` changes, checking the resulting UID.
Eight concurrent uncached actions check that differing
property values do not bleed between requests. All RPCs have bounded contexts.
