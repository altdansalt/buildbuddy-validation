# Overlapping directory outputs produce an incomplete parent Tree

Title: Overlapping directory outputs produce an incomplete parent Tree
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: A successful action returns a parent Tree missing the Directory messages needed to materialize its descendants. Clients relying on that self-contained Tree can fail to restore outputs. This requires overlapping directory declarations; no cross-group impact is demonstrated.
Status: confirmed
Repro test target: //tests/executor/outputs:overlapping_directories
BuildBuddy invocation: https://app.buildbuddy.io/invocation/eae098fa-3d5a-4da8-84e3-b4d17140b51c

## Expected behavior

[REAPI](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) allows an output directory to be a parent of another output directory. A Tree includes its root Directory and all descendant Directory messages in children. Each returned output directory's Tree must be independently complete.

## Actual behavior

The command writes tree/nested/file containing inner, and requests sorted output_paths=[tree, tree/nested]. Execution succeeds and returns both directories. Decoding the tree output's CAS Tree gives zero children instead of the required nested Directory message. Its 82-byte Tree references the nested directory but omits its contents. The separately returned tree/nested Tree is 80 bytes.

The identical zero-children assertion failed in [run 1](https://app.buildbuddy.io/invocation/eae098fa-3d5a-4da8-84e3-b4d17140b51c) (initially in the round2 binary) and [run 2](https://app.buildbuddy.io/invocation/a96b13df-921e-4ef9-8762-aaee8b546d79) (isolated overlapping_directories target). Both used the runner's prestarted daemon and reached successful fixture-local execution. Existing single-directory nested-tree regression coverage passes, isolating the difference to overlapping declarations. CAS readback validates the Tree's digest before decoding, so the missing children are in the server-produced message.

## Logs excerpt

```text
--- FAIL: TestOverlappingDirectories
status=<nil> result=output_directories:{path:"tree" tree_digest:{hash:"1be1f2e19e959631aca7a52f4a3e82d6ec4543dcb5f13db2a94b6bfd6bdb098f" size_bytes:82}}
                   output_directories:{path:"tree/nested" tree_digest:{hash:"4721db558263f3c40220f86f7e09662cfb0967f8e96f583c930adc0381f7414c" size_bytes:80}}
Error: "[]" should have 1 item(s), but has 0
Test: TestOverlappingDirectories
```

## Notes

[Released dirtools.UploadTree](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go) assigns each visited Directory to either its own output Tree root or one ancestor Tree's children using mutually exclusive branches. A separately declared child directory therefore does not enter the parent's children. Mitigation: request only the outermost directory, or reconstruct from other outputs/Directory CAS messages when available; the test does not establish whether all clients can use that workaround.

Baseline harness.Start(App, Executor), empty input root, bounded RPC context, no custom component config, no image overrides, workload-isolation-type=none, DoNotCache=true and SkipCacheLookup=true. Components start inside each remote test and all RPCs address the local fixture. No shared component endpoint or dispatch credential is used. Harness artifacts retain container configs/logs/inspect data. Independent verification is recorded below.

```sh
bb remote --runner_exec_properties=init-dockerd=true test //tests/executor/outputs:overlapping_directories --test_tag_filters=bug --nocache_test_results --test_output=all
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

The [REAPI Command contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L843)
does not prohibit overlapping `output_paths`; the legacy directory field also
explicitly allows parent/child declarations. The
[Tree definition](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1450)
requires all referenced descendant Directory protos in `children`. A separate
OutputDirectory for the child does not waive that requirement.

Both runs returned the same 82-byte parent Tree digest
`1be1f2e19e959631aca7a52f4a3e82d6ec4543dcb5f13db2a94b6bfd6bdb098f`
with zero children. The test validates CAS bytes against the advertised digest
before decoding. In
[released UploadTree](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L569),
the `if ... else if` assigns the nested directory to its own Tree root and
excludes it from the parent's children. This deterministic construction explains
the failure without a race or harness error. No exception was found in release
docs. Severity remains **medium**: the parent Tree is incomplete and cannot be
materialized independently, although redundant output declarations are required.

## Version history

Tested on 2026-10-13 using the unchanged bug-tagged reproducer from
`origin/area/executor-outputs-2` at `f953aa418f749a1b102df6c1b61f85a458fabeb4`.
App, Redis and executor were started inside each test on hosted `bb remote`
runners, with fixture-local endpoints and disposable local credentials.
No BuildBuddy component was built from source.

| Enterprise release (release date, UTC) | Result | BuildBuddy invocation |
| --- | --- | --- |
| v2.314.0 (2026-10-08) | Reproduces | [run](https://app.buildbuddy.io/invocation/2e96ab0a-8cb9-4f4d-919b-fd6e34541405) |
| v2.313.0 (2026-10-06) | Reproduces | [run](https://app.buildbuddy.io/invocation/71022919-3179-42ac-8801-1bdb50ee0ebb) |
| v2.290.0 (2026-07-29) | Reproduces | [run](https://app.buildbuddy.io/invocation/aac11647-ec5c-43ca-8494-49ee4d857081) |
| v2.275.0 (2026-06-04) | Reproduces | [run](https://app.buildbuddy.io/invocation/2b8a9774-b8ad-4198-862b-700bd17c5072) |
| v2.258.0 (2026-04-09) | Reproduces | [run](https://app.buildbuddy.io/invocation/23c416f3-5e4f-493c-be8c-941325281908); [independent repeat](https://app.buildbuddy.io/invocation/4702a650-1fe0-4095-86a1-68796c425ba7) |

All five releases failed at the finding's intended assertion: The enclosing Tree
omitted its nested Directory from Children (zero entries rather than one).
The oldest release was reproduced in two independent fresh fixtures.

**Earliest tested reproducing release: v2.258.0 (2026-04-09).** The defect
was present at or before that release; its actual introduction is unknown.
No earlier release or known-good boundary was tested, so this is an upper
bound on introduction, not evidence that v2.258.0 introduced it.

No release-specific configuration changes were needed for these five releases.

Command template, substituting the matching digests from the table below:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:overlapping_directories \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image=gcr.io/flame-public/buildbuddy-app-enterprise@<app-digest> \
  --test_arg=-harness.executor_image=gcr.io/flame-public/buildbuddy-executor-enterprise@<executor-digest>
```

The invocation batches the five output bug targets; `output_paths_precedence`
contains separate symlink-loss and directory-precreation tests. The result
above was checked against this finding's individual Go test, not just the
aggregate Bazel failure.

### Exact release pins

Both components were overridden together. These linux/amd64 manifest digests
were resolved from the GCR registry API, and the fixture logs identify the
requested digest references. App repository:
`gcr.io/flame-public/buildbuddy-app-enterprise`; executor repository:
`gcr.io/flame-public/buildbuddy-executor-enterprise`.

| Enterprise release | App digest | Executor digest |
| --- | --- | --- |
| v2.314.0 | `sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` | `sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8` |
| v2.313.0 | `sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6` | `sha256:8f7c2662f5fcd0ab888113f6d36d31cde8b8ea254434a8177cf6996b01155afe` |
| v2.290.0 | `sha256:f3b285107f28da495e9496dd848857ce0cf6cddbf3fac8743b259c2a7a8678b6` | `sha256:2c65048231e21ecf0c60623bc5f8ccaf494e82828077c5064aa994909e15607a` |
| v2.275.0 | `sha256:75ffd43cf6dc14ccf5a652b760d7e4d7390dfa7870524c57e7f354f271e96db1` | `sha256:d2dbe25032467449c80e9eb18002d56b57bcf5247cba267ab886022415b30956` |
| v2.258.0 | `sha256:f7b499d817581e58c1fe826c6441a7773519488d4500dc6ffafad387d2c990c4` | `sha256:500703561ba144c64b12a36f0d3350422f09efe693b8e24592414958ea6753cd` |
