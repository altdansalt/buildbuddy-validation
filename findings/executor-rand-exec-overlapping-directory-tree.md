# Overlapping output directories produce an incomplete parent Tree

Title: Overlapping output directories produce an incomplete parent Tree
Components: executor / app
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: A successful action returns a parent output Tree that cannot be reconstructed by a conforming client. Support for nested directory declarations is required by REAPI; this breaks output materialization without an execution or upload error.
Status: confirmed
Repro test target: //tests/crosscutting/rand-exec:overlapping_directory_bug
BuildBuddy invocation: https://app.buildbuddy.io/invocation/2774dad2-4e11-4379-876e-86688cf1d9d7

## Expected behavior

[REAPI Tree.children](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) includes all directories referenced by the root and recursively by its children. Each returned OutputDirectory Tree must be independently reconstructible. Nested output directories are legal: Command.output_directories explicitly permits a directory to be the parent of another, and output_paths does not prohibit nesting.

The command is only:

```sh
mkdir -p tree/nested; printf payload > tree/nested/file
```

With sorted output_paths=[tree, tree/nested], the Tree for tree must include the Directory for nested as a child. The separate Tree for tree/nested does not replace the parent's required Tree.children entry.

## Actual behavior

Execution succeeds with exit code zero and returns both OutputDirectories. The Tree for tree has a DirectoryNode referencing nested, but lacks its corresponding Directory in Tree.children. The comparator verifies the Tree blob's SHA256 and size, hashes its child Directory messages, and cannot resolve the referenced child.

The minimal test reproduces in [run 1](https://app.buildbuddy.io/invocation/2774dad2-4e11-4379-876e-86688cf1d9d7) and [run 2](https://app.buildbuddy.io/invocation/da25d06f-1124-4fe0-983f-81eb421828a1). These author runs are corroborated by the independent verification below.

A preceding [seeded concurrent run](https://app.buildbuddy.io/invocation/05b1571e-ea6a-4768-8180-11c12badb6a4) reproduced the same missing child in every case under both bare and Docker isolation. Reduction removed concurrency, randomness, symlinks, environment variables, and the extra file declaration. The remaining two output declarations and one ordinary file still fail.

## Logs excerpt

```text
ActionResult:
  output_directories: path:"tree"
    tree_digest: hash:"35db764176fc2683a228c8adecef5d8d8e22ffafaa19cd6d7070f66757518ac8" size_bytes:82
  output_directories: path:"tree/nested"
    tree_digest: hash:"5a58e61c6644731365580044d9897e790ac84717a5a054a3c04532797e08fe58" size_bytes:80
--- FAIL: TestOverlappingDirectoryMinimal
Error: Expected value not to be nil.
Messages: missing Tree child at tree/nested
```

## Notes

Run explicitly (default nobug suite excludes this target):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-exec:overlapping_directory_bug --test_tag_filters=bug --nocache_test_results
```

The fixture starts released app/executor containers and disposable Redis on a private network inside the hosted runner. It uses local SQLite/CAS, provisioned local auth, baseline workload-isolation-type=none, a 20-second Action timeout, and SkipCacheLookup=true. No version overrides or shared component deployments are used. Configs, container logs/inspect metadata, and private-daemon logs are retained as undeclared outputs.

The private daemon uses Docker's normal iptables handling with the harness startup lock and mirror.gcr.io for Docker Hub pulls. Earlier pull-rate-limit and Redis-readiness failures occurred before execution and are excluded from reproduction evidence. Passing non-overlapping output Tree coverage uses the same fixture and SHA256/protobuf comparator, ruling out a general CAS, authentication, or Tree decoding failure. The direct symlink reduction passed and is not reported as a defect. Both reproduction runs use the same release digests above.

Mitigation: omit redundant nested directory declarations and request only the parent. No shared harness/dependency changes are needed. Findings remain in this repository; no upstream report was sent.

Passing regression coverage: [final ordinary suite](https://app.buildbuddy.io/invocation/25d55482-26e3-4a35-bbdf-67e1c0c5d98e), including concurrent generated actions, symlinks, parent-directory/file overlap, and Action digest-field separation.

## Verification

Verdict: **confirmed** (independent review, 2026-10-13). Severity remains
**medium**: a valid successful action produces a malformed output artifact that
breaks normal Tree materialization. The trigger requires overlapping declarations
and has a simple workaround; this test does not establish security impact or
loss of the underlying file bytes.

### Spec and implementation review

- [REAPI Command and Tree specification](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L841):
  `output_paths` requires sorted, deduplicated relative paths and promises the
  entire directory structure for directory outputs. `tree` and `tree/nested`
  satisfy these constraints. The legacy `output_directories` explicitly allows
  parent/child declarations. `Tree.children` requires every recursively referenced
  child Directory; a separate OutputDirectory does not waive this requirement.
- [Pinned executor UploadTree implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L570):
  each visited Directory is assigned either to its own Tree root or, via an
  `else if`, to one parent Tree's children. Consequently `tree/nested` becomes
  its own root and is omitted from `tree`'s children even though the parent
  Directory contains its DirectoryNode. `FindParentOutputPath` also returns only
  the nearest declared ancestor. This explains the deterministic malformed Tree.
- [Workspace.UploadOutputs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go#L553)
  calls this uploader for normal execution. The pinned
  [RBE overview](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/remote-build-execution.md)
  and [enterprise setup documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-rbe.md)
  document no exception for overlapping output declarations. Source inspection
  shows an implementation defect rather than an intentional alternate format.

### Test and environment review

Reviewed PR #126's `TestOverlappingDirectoryMinimal`, `minimalOutputCase`,
`normalized`, `bareFixture`, and daemon setup. The test uses fixture-local
app/executor RPCs, locally provisioned auth, default pinned images, sorted
OutputPaths, an ordinary shell command, a bounded context, and cache lookup
bypass. It requests neither Docker isolation nor Firecracker. The private daemon
only hosts the fixture and cannot explain a successfully uploaded but incomplete
Tree. CAS reads validate SHA256 and size before decoding. Child Directory hashes
use deterministic protobuf serialization; this minimal Directory contains only
ordinary known fields, so no unknown-field or serialization ambiguity explains
the missing child. The assertion fails before comparing the reference snapshot.
Although the nested Directory is separately uploaded to CAS, requiring a client
to fetch it outside the Tree would violate the Tree contract.

### Independent reproductions

Both runs used the PR branch test without modifications and this command:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-exec:overlapping_directory_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Run 1](https://app.buildbuddy.io/invocation/07718a5f-40ff-4075-960f-be13a6e86174)
  ([runner](https://app.buildbuddy.io/invocation/da91f287-ca29-40f7-931a-8c6586752da5)):
  execution succeeded; test failed with `missing Tree child at tree/nested`.
- [Run 2](https://app.buildbuddy.io/invocation/5036c733-6f57-4cf8-852e-949e97cb3033)
  ([runner](https://app.buildbuddy.io/invocation/0f17a80a-60fe-424c-be2d-e02317b5e2cb)):
  execution succeeded; test failed with the same missing-child assertion.

Both logs show the exact app/executor digests listed above and identical Tree
hashes/sizes (`tree`: `35db764176fc2683a228c8adecef5d8d8e22ffafaa19cd6d7070f66757518ac8`,
82 bytes; `tree/nested`: `5a58e61c6644731365580044d9897e790ac84717a5a054a3c04532797e08fe58`,
80 bytes). OutputDirectory order differs between runs, which is harmless because
the comparator does not rely on order. Neither run failed in readiness, auth,
image pulling, RPC transport, or cleanup. This is reliable reproduction of the
reported artifact defect, not fixture flakiness.

PR #126 is still open at review time. This verification branch copies only this
finding from its head; the reproducer remains in that PR and requires its branch
until merged. No shared harness or dependency changes are requested.
