# Input file/directory name collision returns UNKNOWN

Title: Malformed input tree with a file/directory name collision returns UNKNOWN instead of INVALID_ARGUMENT
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: An authenticated execution client can submit a malformed tree and receives an unclassified server error rather than a client-input error. This impairs error handling and diagnostics; no crash, successful command execution, or cross-tenant impact was demonstrated.
Status: unverified
Repro test target: //tests/executor/input-tree:file_directory_collision_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/a133dab2-f45f-40de-a3ab-f0f54eadcf56

## Expected behavior

The [REAPI Directory contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) requires unique child names across all node kinds. The Execute RPC documents INVALID_ARGUMENT for invalid arguments. A root containing both FileNode(name="same") and DirectoryNode(name="same") should be rejected as INVALID_ARGUMENT before executing the command.

## Actual behavior

The completed ExecuteResponse has status UNKNOWN (2), with a raw filesystem error identifying an executor workspace path. The bug-tagged assertion expecting INVALID_ARGUMENT (3) fails.

Reproduced twice with uncached test execution on independent fixture-local containers:

1. https://app.buildbuddy.io/invocation/cae14c20-0ab4-4ef7-8fbb-e54425e1b813 (explicit TestFileDirectoryCollision filter).
2. https://app.buildbuddy.io/invocation/a133dab2-f45f-40de-a3ab-f0f54eadcf56 (standalone minimal target, no test filter).

Both fixtures bootstrapped local auth and reached Execute completion. All referenced blobs, including the empty Directory, were uploaded successfully to the local app CAS. A valid-tree suite using the same fixture/configuration passed at https://app.buildbuddy.io/invocation/3de2f8b7-6272-4313-baac-abb3e2088d12. No external component endpoint or malformed harness configuration is involved. Status remains unverified for maintainer review as requested.

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
