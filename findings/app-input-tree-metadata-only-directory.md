# CAS tree traversal reports an uploaded metadata-only directory as missing

Title: CAS tree traversal reports an uploaded metadata-only directory as missing
Components: app / executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Actions containing valid empty directories with metadata fail before execution. This affects authenticated clients using optional node properties; no data loss, crash or cross-tenant impact was shown.
Status: confirmed
Repro test target: //tests/executor/input-tree:input_tree_limits_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/275ae89a-6149-4737-a420-b2008de3d339

## Expected behavior

An empty child Directory carrying NodeProperties (mtime and unix_mode) is valid under the [REAPI Directory contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto). Optional properties may be ignored, but their presence should not cause existing CAS content to be reported missing.

## Actual behavior

The `optional-node-properties` subtest uploads an empty child Directory with metadata, then Execute returns FAILED_PRECONDITION (9), wrapping a GetTree NotFound error. The second reproduction explicitly reads the child blob back from fixture CAS and checks its nonzero length before Execute; these checks pass. Thus this is not an absent upload or authentication failure. The matching nonempty-directory metadata control is in the untagged suite, and ordinary empty directories are covered there as well.

Reproduced twice with uncached execution in independent fixture-local released-image containers:

1. https://app.buildbuddy.io/invocation/974c14bd-cad2-4b2c-bfc2-a4546b9b86a5
2. https://app.buildbuddy.io/invocation/275ae89a-6149-4737-a420-b2008de3d339

The original draft failure also occurred at https://app.buildbuddy.io/invocation/c69b123b-ac92-48a9-89de-049c3d3394c7. The untagged coverage suite, including the nonempty-directory metadata control and concurrent cold/warm actions, passes at https://app.buildbuddy.io/invocation/f0c39316-3e0c-46c1-8302-e628abc0cd24. Independent verification is recorded below.

## Logs excerpt

```text
code:9 message:"download inputs: rpc error: code = NotFound desc = could not get input tree: Found empty directory for digest e7c2fbe24b99b655682700e641b5002568c7a8194b1273e4af7feb71da404a49."
```

## Notes

Run `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/input-tree:input_tree_limits_test --test_tag_filters=bug --nocache_test_results --test_output=all`. This target contains two separate failing subtests; select the corresponding Go subtest with `--test_arg=-test.run=TestInputTreeLimits/<subtest>` if desired. Images use pinned defaults with no overrides. App, Redis and executor run inside the test on the hosted runner; API keys are created by the local app. No shared deployment is a component endpoint. Full logs and image inspect metadata are preserved in undeclared outputs.

Released [content_addressable_storage_server.go fetchDirectory](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L795) skips the canonical zero-byte Directory digest, but rejects any other decoded child with no files/directories/symlinks, ignoring NodeProperties. Metadata makes an empty Directory serialize to nonzero bytes, explaining this false missing-input error. No claim is made that optional Unix modes or timestamps must be applied. Mitigation: omit properties on empty directories. Suggested fix: accept valid metadata-only directories during GetTree traversal.

Findings stay in this repository; no upstream report was filed.

## Verification

Verdict: **confirmed**. Independently reviewed on 2026-10-12/13 using PR #29
commit `5231a24db7394306187fb690ca32b52c1b583cdb`. The bug test and helpers were
run unchanged in a separate checkout. Both runs printed the exact pinned
app/executor image digests above; they used default fixture configuration,
fixture-local credentials/endpoints, and no image overrides or source builds.

First run:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/input-tree:input_tree_limits_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Second run used the same command plus
`--test_arg=-test.run=TestInputTreeLimits/optional-node-properties`, excluding
the hardlink subtest entirely.

- [Verification run 1](https://app.buildbuddy.io/invocation/9be4c732-ce2d-441f-836a-54fca4994798), [runner](https://app.buildbuddy.io/invocation/8f90463c-da8f-4f8a-9098-6c7f2cb242fd).
- [Verification run 2, metadata only](https://app.buildbuddy.io/invocation/cbac5827-3235-49c9-b49d-4c9666fc92e8), [runner](https://app.buildbuddy.io/invocation/9664e789-0381-4550-bf64-03382ee13141).

Both uploads and the child's ByteStream readback/size checks succeeded. Both
executions then returned FAILED_PRECONDITION (9), wrapping exactly the same
`Found empty directory for digest e7c2fbe2...` error. The isolated second run
rules out contamination from the preceding hardlink-limit failure. Failure is
in input-tree retrieval, before commands or timestamp/mode assertions execute;
there is no timeout or cached-result explanation.

Also reran the unchanged nonempty-directory control:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/input-tree:go_default_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all --test_arg=-test.run=TestInputTreeRound2/optional-node-properties-nonempty-directory
```

[Control passed](https://app.buildbuddy.io/invocation/de92812c-2f0d-4f9b-9c1c-4d6ffefc3243)
([runner](https://app.buildbuddy.io/invocation/d7de3969-50a4-448d-916e-e11747e46fa7)).
It supplies the same file and directory properties, with a child file inside
the directory, and successfully executes two uncached actions.

[REAPI Directory/NodeProperties](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1049)
has no requirement that a Directory contain children. NodeProperties support
is explicitly server-defined; the released [capabilities server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go#L94)
does not advertise supported node properties. Therefore this report does not
establish a requirement to honor mtime or unix_mode, or accept every optional
property during execution. The narrower confirmed defect is in CAS traversal:
[GetTree](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L416)
should enumerate stored directories; unsupported execution metadata does not
make an existing CAS blob missing.

Released [fetchDirectory](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L795)
finds and unmarshals the child blob, then explicitly returns NotFound solely
because all three child lists are empty. It never checks whether the metadata
is supported. This explains both the successful readback and why adding a
child avoids the failure. Reviewed [RBE configuration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-rbe.md)
and [RBE setup](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-setup.md)
docs; found no documented CAS restriction requiring nonempty directories.

Severity remains **low**: the trigger is optional metadata on an empty child,
and omitting that metadata avoids it. No loss of stored content, crash or
cross-tenant impact was observed. Attribution is to the app's CAS traversal;
the executor exposes its error rather than independently rejecting properties.
