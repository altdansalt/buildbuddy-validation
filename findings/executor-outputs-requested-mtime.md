# Requested output mtime silently omitted

Title: Requested output mtime silently omitted
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Clients requesting timestamps cannot reproduce output metadata. File bytes remain intact; no cross-group impact is demonstrated.
Status: confirmed
Repro test target: //tests/executor/outputs:requested_mtime
BuildBuddy invocation: https://app.buildbuddy.io/invocation/85b0e0bc-475c-4e46-98fd-ff5a0b139f41

## Expected behavior

Command.output_node_properties=[mtime] requests timestamps for output files and directories. Unsupported properties should be explicitly rejected; this test permits INVALID_ARGUMENT rather than requiring all servers to support mtime.

[REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

## Actual behavior

The command creates file and tree/file and sets their mtimes and the tree directory mtime with touch -t 200001010000. The request succeeds but the top-level OutputFile has no NodeProperties or mtime. The test stops at this first missing property; it does not independently establish directory or nested-file metadata behavior.

The same assertion failed in [run 1](https://app.buildbuddy.io/invocation/85b0e0bc-475c-4e46-98fd-ff5a0b139f41) and [run 2](https://app.buildbuddy.io/invocation/eae098fa-3d5a-4da8-84e3-b4d17140b51c). Both reached successful fixture-local execution with the pinned images above. The first used the fixture-owned Docker daemon; the second used the runner's prestarted daemon. This rules out daemon setup as the cause. No custom component configuration or version overrides were used. A separate invocation failed before execution due to Docker Hub's Redis rate limit and is excluded from reproduction evidence.

## Logs excerpt

```text
TestRequestedMtime
status=<nil> result=output_files:{path:"file" digest:{...}} output_directories:{path:"tree" tree_digest:{...}}
Expected value not to be nil.
Messages: requested output file mtime omitted
```

## Notes

The requested timestamp is 2000-01-01T00:00:00Z (946684800 seconds). The assertion checks both seconds and nanoseconds if present. This is a metadata negotiation/conformance concern, not a content-digest failure. Independent verification of metadata negotiation is recorded below.

[Released implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go). Tests use an empty input root, baseline bare isolation, bounded RPC contexts, DoNotCache=true and SkipCacheLookup=true. Fixture-local app, Redis and executor containers are started inside each test on bb remote. No shared component endpoint or dispatch credentials are used. Container configs/logs/inspect metadata are preserved in undeclared test outputs. Independent verification is recorded below.

```sh
bb remote --runner_exec_properties=init-dockerd=true test //tests/executor/outputs:requested_mtime --test_tag_filters=bug --nocache_test_results --test_output=all
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

The [Command.output_node_properties contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L886)
lets clients request NodeProperties field names and says an unrecognized property
returns `INVALID_ARGUMENT`. `mtime` is a defined NodeProperties field. The test
accepts `INVALID_ARGUMENT`, so it does not require servers to implement optional
metadata support.

The strongest alternative explanation is unsupported metadata: the
[released capabilities server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go#L92)
does not advertise supported node properties. This explains why a client should
not assume support, but does not authorize silently accepting an unrecognized
request. A source search found no consumption of `OutputNodeProperties` in the
released implementation, and
[OutputFile construction](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L296)
sets only path, digest, and executable bit. The release's own proto repeats the
same rejection contract; no documented exception was found.

Both runs returned successful execution and a regular OutputFile without mtime,
failing at `mtime_test.go:24`. Missing metadata is independent of the shell's
timezone; the later exact timestamp assertion could require an explicit `TZ=UTC`
for portability, but was never reached and is not the observed failure. Likewise,
directory and nested-file metadata assertions were not reached. Severity remains
**low**, specifically for silent acceptance instead of returning requested
metadata or explicit rejection, rather than a claim that all clients must receive
mtime support.

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

All five releases failed at the finding's intended assertion: The action
returned OK but the requested top-level output file mtime was nil.
The oldest release was reproduced in two independent fresh fixtures.

**Earliest tested reproducing release: v2.258.0 (2026-04-09).** The defect
was present at or before that release; its actual introduction is unknown.
No earlier release or known-good boundary was tested, so this is an upper
bound on introduction, not evidence that v2.258.0 introduced it.

No release-specific configuration changes were needed for these five releases.

Command template, substituting the matching digests from the table below:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:requested_mtime \
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
