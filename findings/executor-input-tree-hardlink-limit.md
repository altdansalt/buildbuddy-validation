# Valid repeated-digest input tree fails at the filesystem hardlink limit

Title: Valid repeated-digest input tree fails at the filesystem hardlink limit
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: A valid large action fails before command execution. An authenticated client can trigger this with repeated content; no executor crash or cross-tenant impact was shown.
Status: confirmed
Repro test target: //tests/executor/input-tree:input_tree_limits_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/275ae89a-6149-4737-a420-b2008de3d339

## Expected behavior

A canonical tree with 256 directories, each containing 256 sorted file nodes referencing the same uploaded 15-byte blob, should materialize successfully. Internal hardlink deduplication should fall back to copying when the filesystem link limit is reached. The [REAPI Directory contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) does not forbid repeated file digests.

## Actual behavior

The `65536-files-256-directories` subtest returns UNKNOWN (2) with `too many links` before running the shell command. It reproduces without the concurrent mutation test. The tree has unique sorted names and every referenced Directory and file blob was uploaded successfully; the existing valid-tree coverage passes using the same fixture/configuration.

Reproduced twice with uncached execution in independent fixture-local released-image containers:

1. https://app.buildbuddy.io/invocation/974c14bd-cad2-4b2c-bfc2-a4546b9b86a5
2. https://app.buildbuddy.io/invocation/275ae89a-6149-4737-a420-b2008de3d339

The original draft failure also occurred at https://app.buildbuddy.io/invocation/c69b123b-ac92-48a9-89de-049c3d3394c7. The untagged coverage suite, including the nonempty-directory metadata control and concurrent cold/warm actions, passes at https://app.buildbuddy.io/invocation/f0c39316-3e0c-46c1-8302-e628abc0cd24. Independent verification is recorded below.

## Logs excerpt

```text
code:2 message:"download inputs: could not fetch inputs: link /data/executor/<workspace>/d000/f000 /data/executor/<workspace>/d253/f231: too many links"
```

## Notes

Run `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/input-tree:input_tree_limits_test --test_tag_filters=bug --nocache_test_results --test_output=all`. This target contains two separate failing subtests; select the corresponding Go subtest with `--test_arg=-test.run=TestInputTreeLimits/<subtest>` if desired. Images use pinned defaults with no overrides. App, Redis and executor run inside the test on the hosted runner; API keys are created by the local app. No shared deployment is a component endpoint. Full logs and image inspect metadata are preserved in undeclared outputs.

The filesystem limit is an expected environment constraint; the defect is propagating that optimization failure for otherwise valid inputs. Released [fastcopy_linux.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/fastcopy/fastcopy_linux.go) calls os.Link and returns errors other than destination-exists without copy fallback. [dirtools.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go) uses this for repeated file content. This finding applies to the default bare Linux runner/filesystem configuration, not all filesystems or isolation providers. Mitigation: avoid exceeding the link count for identical inputs. Suggested fix: copy to a fresh inode on EMLINK.

Findings stay in this repository; no upstream report was filed.

## Verification

Verdict: **confirmed**. Independently reviewed on 2026-10-12/13 using PR #29
commit `5231a24db7394306187fb690ca32b52c1b583cdb`. Ran the bug test unchanged
in a separate checkout. Both runs printed the pinned app/executor image
digests above and used default fixture configuration, local credentials and
local container endpoints, without image overrides or component source builds.

First run:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/input-tree:input_tree_limits_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Second run used the same command plus
`--test_arg=-test.run=TestInputTreeLimits/65536-files-256-directories`, excluding
the metadata case.

- [Verification run 1](https://app.buildbuddy.io/invocation/9be4c732-ce2d-441f-836a-54fca4994798), [runner](https://app.buildbuddy.io/invocation/8f90463c-da8f-4f8a-9098-6c7f2cb242fd).
- [Verification run 2, hardlink case only](https://app.buildbuddy.io/invocation/3c7fe194-a192-49f4-9046-234777a50fd0), [runner](https://app.buildbuddy.io/invocation/98c67c70-32c9-4402-8e24-7ef33cf52418).

Both returned UNKNOWN (2) with `download inputs: could not fetch inputs:
link .../d000/f000 .../d253/f231: too many links`. Materialization failed in
under a second, before the shell command, so the 30-second command timeout,
process-spawning costs, shell utilities and content assertions cannot explain
it. Action caching and Bazel test-result caching were disabled. The second
run contains no other subtest or concurrent mutation workload, and uses a
fresh fixture; the identical destination is evidence of a deterministic link
limit rather than intermittent networking or scheduling.

The [REAPI Directory canonicalization rules](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L978)
require unique sorted child names, not unique content digests. The test uploads
one 15-byte blob and a canonical 256-file Directory, then references that
Directory under 256 different sorted names. This is a valid compact Merkle
DAG, representing 65,536 files. Reusing directory/file digests is legal, and
all required uploads succeeded. The requirement to copy on EMLINK is a
proposed implementation remedy, not an explicit REAPI MUST: the protocol
also does not promise unlimited action sizes or resource availability.

The filesystem's finite link count is an ordinary environment constraint.
The product defect is exposing it as an opaque input-download failure for a
modest approximately 960 KiB of file contents, because an internal
deduplication optimization shares one inode across all paths. Released
[dirtools batch fetch](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L909)
writes the first file, then uses `copyFile` for the rest;
[copyFile](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L692)
calls [FastCopy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/fastcopy/fastcopy_linux.go#L18),
which calls `os.Link` and propagates EMLINK with no byte-copy fallback. This
matches the exact observed error; it is not evidence of disk exhaustion.
Reviewed release [RBE configuration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-rbe.md),
[RBE setup](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-setup.md)
and [capabilities](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go);
found no documented per-digest input multiplicity limit or required filesystem
configuration that excludes this setup.

Severity remains **low**: this is a bounded, filesystem-dependent reliability
failure requiring tens of thousands of identical inputs. Copying to a fresh
inode on EMLINK would avoid the optimization limit. The assertion is suitable
for the validated Linux runner, but is not portable evidence that every
filesystem/provider must fail at 65,536 files. No executor crash or
cross-tenant impact was observed.

## Version history

Tested on 2026-10-13 using the unchanged bug-tagged reproducer from
`origin/area/executor-input-tree-2` at `5231a24db7394306187fb690ca32b52c1b583cdb`.
App, Redis and executor were started inside each test on hosted `bb remote`
runners, with fixture-local endpoints and disposable local credentials.
No BuildBuddy component was built from source.

| Enterprise release (release date, UTC) | Result | BuildBuddy invocation |
| --- | --- | --- |
| v2.314.0 (2026-10-08) | Reproduces | [run](https://app.buildbuddy.io/invocation/db9c923e-c16e-46df-b371-1ac6912e9a90) |
| v2.313.0 (2026-10-06) | Reproduces | [run](https://app.buildbuddy.io/invocation/015c6466-49d6-4314-a656-ebcc7fabef40) |
| v2.290.0 (2026-07-29) | Reproduces | [run](https://app.buildbuddy.io/invocation/f6fd9b35-e5f0-4ea5-8016-9727668aede7) |
| v2.275.0 (2026-06-04) | Reproduces | [run](https://app.buildbuddy.io/invocation/b35640f2-c0b1-4cfb-b943-b896d94a92a0) |
| v2.258.0 (2026-04-09) | Reproduces | [run](https://app.buildbuddy.io/invocation/e895cc27-5ee1-404c-a3c8-f49a0d0f5758); [independent repeat](https://app.buildbuddy.io/invocation/e5719312-c8a5-44fa-9c48-12e3fef65e92) |

All five releases failed at the finding's intended assertion: UNKNOWN (2) with
"too many links" during materialization, before command execution.
The oldest release was reproduced in two independent fresh fixtures.

**Earliest tested reproducing release: v2.258.0 (2026-04-09).** The defect
was present at or before that release; its actual introduction is unknown.
No earlier release or known-good boundary was tested, so this is an upper
bound on introduction, not evidence that v2.258.0 introduced it.

No release-specific configuration changes were needed for these five releases.

Command template, substituting the matching digests from the table below:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/input-tree:input_tree_limits_test \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image=gcr.io/flame-public/buildbuddy-app-enterprise@<app-digest> \
  --test_arg=-harness.executor_image=gcr.io/flame-public/buildbuddy-executor-enterprise@<executor-digest> \
  --test_arg=-test.run=TestInputTreeLimits/65536-files-256-directories
```

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
