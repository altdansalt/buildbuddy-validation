# Input file/directory name collision returns UNKNOWN

Title: Malformed input tree with a file/directory name collision returns UNKNOWN instead of INVALID_ARGUMENT
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: An authenticated execution client can submit a malformed tree and receives an unclassified server error rather than a client-input error. This impairs error handling and diagnostics; no crash, successful command execution, or cross-tenant impact was demonstrated.
Status: rejected
Repro test target: //tests/executor/input-tree:file_directory_collision_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/a133dab2-f45f-40de-a3ab-f0f54eadcf56

## Original expected behavior (rejected in verification)

The [REAPI Directory contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) requires unique child names across all node kinds. The Execute RPC documents INVALID_ARGUMENT for invalid arguments. A root containing both FileNode(name="same") and DirectoryNode(name="same") should be rejected as INVALID_ARGUMENT before executing the command.

## Actual behavior

The completed ExecuteResponse has status UNKNOWN (2), with a raw filesystem error identifying an executor workspace path. The bug-tagged assertion expecting INVALID_ARGUMENT (3) fails.

Reproduced twice with uncached test execution on independent fixture-local containers:

1. https://app.buildbuddy.io/invocation/cae14c20-0ab4-4ef7-8fbb-e54425e1b813 (explicit TestFileDirectoryCollision filter).
2. https://app.buildbuddy.io/invocation/a133dab2-f45f-40de-a3ab-f0f54eadcf56 (standalone minimal target, no test filter).

Both fixtures bootstrapped local auth and reached Execute completion. All referenced blobs, including the empty Directory, were uploaded successfully to the local app CAS. A valid-tree suite using the same fixture/configuration passed at https://app.buildbuddy.io/invocation/3de2f8b7-6272-4313-baac-abb3e2088d12. No external component endpoint or malformed harness configuration is involved. Independent review rejected the required-status claim; see Verification below.

## Logs excerpt

```text
expected: int(3)
actual  : int32(2)
malformed input tree should return INVALID_ARGUMENT, got code:2
message:"download inputs: could not fetch inputs: open /data/executor/adc743e3-1901-47e0-b075-7caa329fd9fa/same: is a directory"
```

This is the status returned by the product and recorded in the test log; full container logs and inspect metadata are preserved by the fixture in undeclared outputs.

## Notes

Use `bb remote test //tests/executor/input-tree:file_directory_collision_test --test_tag_filters=bug --nocache_test_results --test_output=all` with the repository's public fetch remote setup. No image overrides or config changes are needed. The baseline runs bare Linux actions (`workload-isolation-type=none`) inside the released executor container; no privileged mode or nested virtualization.

Source inspection of [v2.314.0 dirtools](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go) shows DownloadTree queues file downloads and creates directories before fetching the queued files. The colliding directory therefore exists when the file fetch opens its destination. [Workspace](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go) and [executor](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/executor/executor.go) wrap this error as "could not fetch inputs" / "download inputs". This explains the observed error, rather than establishing any broader security consequence.

Mitigation: clients must construct canonical directories with unique child names. Suggested server fix: validate names across node kinds and classify malformed input as INVALID_ARGUMENT. Findings remain in this repository; no upstream report was filed.

## Verification

Independent review on 2026-10-12: **rejected** as a demonstrated protocol defect.
The filesystem failure is real, but the test's mandatory INVALID_ARGUMENT
expectation is not established by REAPI. Low is retained as the original
severity of this diagnostic-quality concern, not an active confirmed defect.

### Spec and source review

The pinned [REAPI Execute contract, lines 55–62](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L55)
explicitly says the server MAY enforce canonicalization requirements and MAY
proceed without verifying some or all of them. The [Directory requirements,
lines 980–999](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L980)
include unique child names in those canonicalization restrictions. The test
violates that client obligation. Execute lists INVALID_ARGUMENT among possible
errors; it does not require this particular code for every noncanonical tree.
Consequently, the Expected behavior section above overstates the contract.
A clearer diagnostic would be useful, but this assertion alone does not prove
a product defect or justify mandatory validation before execution.

The released [tree fetcher](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L1650)
queues files, creates directories, then fetches files. Its [writeFile](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L657)
opens the already-created directory as a file. [Workspace](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go#L381)
and [executor](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/executor/executor.go#L398)
wrap that error; [WrapError](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/status/status.go#L275)
preserves its underlying gRPC classification, yielding UNKNOWN for the raw
filesystem error. This explains the response without a Docker, auth, or CAS
failure. The pinned [platform docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-platforms.md#L243)
support `none` isolation for self-hosted executors, as configured here. These
docs and the [RBE troubleshooting docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/troubleshooting-rbe.md)
provide no additional promise of collision validation or INVALID_ARGUMENT.

### Independent reproductions

Reviewed and ran the merged PR #2 test from main commit
`da2b710d4825ab31c08f569302e5aa75ec295e45`, without changing tests or harness:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/input-tree:file_directory_collision_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

The empty tag filter overrides `nobug`; uncached execution forces fresh fixtures.
Both runs use the exact pinned app and executor digests listed above. Uploads,
auth bootstrap and Execute completion succeed, then the assertion observes
UNKNOWN (2), with `same: is a directory`, rather than INVALID_ARGUMENT (3).
This is repeatable behavior, not test flakiness. All component traffic uses the
fixture's published loopback endpoints and local disposable API key.

- Run 1: [c18c5e84](https://app.buildbuddy.io/invocation/c18c5e84-d73c-4c8d-9499-63e9355ca138).
- Run 2: [51754d5d](https://app.buildbuddy.io/invocation/51754d5d-d728-4d98-8e9c-46c9c53ea802).

This review changes finding files only. Removing/reframing the bug-tagged assertion is a
follow-up for the test owner; a diagnostic-improvement request must not claim a
mandatory REAPI validation rule.
