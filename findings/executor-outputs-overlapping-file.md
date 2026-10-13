# Explicit nested output file omitted from OutputFiles

Title: Explicit nested output file omitted from OutputFiles
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Clients enumerating an explicitly requested file through OutputFiles cannot find it. Its bytes remain available inside the directory Tree, reducing severity.
Status: confirmed
Repro test target: //tests/executor/outputs:overlapping_file
BuildBuddy invocation: https://app.buildbuddy.io/invocation/85b0e0bc-475c-4e46-98fd-ff5a0b139f41

## Expected behavior

Command.output_paths says a resulting file is returned in an OutputFile field. A separately requested directory Tree does not remove that explicitly listed file path.

[REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

## Actual behavior

The command writes tree/file and tree/nested/file and requests sorted output_paths=[tree, tree/nested, tree/nested/file]. Execution succeeds with two OutputDirectories and zero OutputFiles; the explicitly requested tree/nested/file is omitted.

The same assertion failed in [run 1](https://app.buildbuddy.io/invocation/85b0e0bc-475c-4e46-98fd-ff5a0b139f41) and [run 2](https://app.buildbuddy.io/invocation/eae098fa-3d5a-4da8-84e3-b4d17140b51c). Both reached successful fixture-local execution with the pinned images above. The first used the fixture-owned Docker daemon; the second used the runner's prestarted daemon. This rules out daemon setup as the cause. No custom component configuration or version overrides were used. A separate invocation failed before execution due to Docker Hub's Redis rate limit and is excluded from reproduction evidence.

## Logs excerpt

```text
TestOverlappingDirectoryOutputs
status=<nil> result=output_directories:{path:"tree" ...} output_directories:{path:"tree/nested" ...}
Error: "[]" should have 1 item(s), but has 0
Test: TestOverlappingDirectoryOutputs
```

## Notes

Released dirtools.UploadTree.visitFile only adds a top-level OutputFile when FindParentOutputPath finds no parent output path. A file explicitly listed beneath another output is therefore suppressed. Mitigation: retrieve the file through a directory Tree or avoid redundant declarations. The separate overlapping-directory finding describes malformed parent Trees.

[Released implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go). Tests use an empty input root, baseline bare isolation, bounded RPC contexts, DoNotCache=true and SkipCacheLookup=true. Fixture-local app, Redis and executor containers are started inside each test on bb remote. No shared component endpoint or dispatch credentials are used. Container configs/logs/inspect metadata are preserved in undeclared test outputs. Independent verification is recorded below.

```sh
bb remote --runner_exec_properties=init-dockerd=true test //tests/executor/outputs:overlapping_file --test_tag_filters=bug --nocache_test_results --test_output=all
```

Passing regression and round 2 coverage: [nobug suite](https://app.buildbuddy.io/invocation/fbec580c-729e-4ae5-897d-fbddbc9e7574), run with `bb remote test //tests/executor/outputs/... --config=nobug --nocache_test_results --test_output=all`.


## Verification

Independent review on 2026-10-13: **confirmed**. Reviewed PR #60 at
`f953aa418f749a1b102df6c1b61f85a458fabeb4`, including the bug-tagged target,
`helpers_test.go`, fixture defaults, and the released v2.314.0 source and docs.
The tests use sorted, unique, relative output paths, an empty input root,
fixture-local authenticated RPCs, bounded contexts, bare Linux isolation, and
both cache bypass flags. No component configuration or image override was used.
The runner logs show the exact pinned app/executor digests listed above and
successful action completion before the assertion failure; neither run failed
on readiness, authentication, Docker setup, or command execution.

Ran all three individual targets together twice, with fresh test execution:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test \
  //tests/executor/outputs:overlapping_directories \
  //tests/executor/outputs:overlapping_file \
  //tests/executor/outputs:requested_mtime \
  --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

[Verification run 1](https://app.buildbuddy.io/invocation/4cc5e14d-bf05-4ca0-bfe4-f82497ccbbd7)
and [verification run 2](https://app.buildbuddy.io/invocation/222d9fb5-ce82-4ee0-b571-f621ef4656b0)
executed all three targets and failed at the same reported assertions. The empty
filter overrides the `nobug` exclusion; these were actual executions, not cached
failures. Runner wrapper links:
[run 1](https://app.buildbuddy.io/invocation/0c9f44d9-2b60-4bbe-9399-ea7d4dc8d413),
[run 2](https://app.buildbuddy.io/invocation/75ba234c-9541-4b47-81ff-bf7872f30650).

The [ActionResult.output_files contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1253)
requires a single entry for each requested file that exists after execution.
The [Command.output_paths contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L843)
contains no prohibition on requesting a file beneath a requested directory.
Returning that file only inside a Tree does not satisfy the explicit OutputFile
requirement.

Both runs completed successfully with two OutputDirectories and zero
OutputFiles, failing at `overlap_test.go:17`. In
[released visitFile](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L471),
`FindParentOutputPath` suppresses an OutputFile whenever an ancestor is an output
path, without checking whether the file itself is explicitly requested. This
explains the omission deterministically. No documented exception was found.
The test's later Tree assertions are unreachable at this failure and are not
independent evidence here. Severity remains **low**: the missing result entry is
a contract violation, but the file can be retrieved from the separately returned
nested Tree; the malformed parent Tree is tracked separately.
