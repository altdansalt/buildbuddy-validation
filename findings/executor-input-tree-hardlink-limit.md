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
