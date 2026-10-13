# Released BuildBuddy findings report

## Executive summary

As of 2026-10-13, 69 finding files from main and 12 open PR branches consolidate into **47 confirmed defects** (1 high, 25 medium, 21 low) and **6 rejected reports**. There are **no standalone unverified reports**; unverified duplicate reproducers remain identified within the confirmed entries. Counts below count each consolidated defect once per affected component, so component totals overlap.

| Affected component | High | Medium | Low | Confirmed total |
| --- | ---: | ---: | ---: | ---: |
| app | 0 | 18 | 15 | 33 |
| executor | 1 | 11 | 7 | 19 |
| cacheproxy | 0 | 5 | 4 | 9 |

The top defects are Docker ignoring `network=off`, bare-runner input mutation poisoning subsequent actions, descendants surviving timeout plus termination grace, incomplete Trees for overlapping directory outputs, and scheduler recovery failures after app replacement or Redis state loss. No host escape or cross-organization compromise is established.

Every confirmed defect tested across historical releases reproduces back to at least **2.258.0 or 2.260.0**. These are long-standing defects, not demonstrated recent regressions. Version matrices sample releases; they do not prove every intervening release was tested or establish the introduction version. Confirmed findings without a historical matrix are explicitly marked below.

Coverage with no distinct defect found includes these test areas (bounded scenarios, not exhaustive guarantees):

- App: `tests/app/auth-groups/`, `tests/app/digest-function-execution/`, `tests/app/rand-bes/`, `tests/app/storage-gc/`, `tests/app/cache-stats/`.
- Cache proxy: `tests/cacheproxy/action-cache/`, `tests/cacheproxy/auth-passthrough/`, `tests/cacheproxy/digest-functions/`, `tests/cacheproxy/failure-modes/`, `tests/cacheproxy/model-based/`, `tests/cacheproxy/write-through/`, `tests/cacheproxy/rand-config/`, `tests/cacheproxy/smoke/`.
- Executor: `tests/executor/caching-results/`, `tests/executor/file-cache/`, `tests/executor/lifecycle/`, `tests/executor/platform-props/`, `tests/executor/resources/`, `tests/executor/scheduling/`, `tests/executor/stdio-exit/`, `tests/executor/persistent-workers/`, `tests/executor/smoke/`.
- Crosscutting: `tests/crosscutting/bazel-download-modes/`, `tests/crosscutting/bazel-flags/`, `tests/crosscutting/bazel-matrix/`, `tests/crosscutting/bazel-test-rbe/`, `tests/crosscutting/e2e-proxy-exec/`, `tests/crosscutting/metrics-consistency/`, `tests/crosscutting/rand-trees/`, `tests/crosscutting/rand-workspaces/`, `tests/crosscutting/rand-multi-client/`, `tests/crosscutting/soak/`. Randomized execution and upgrade coverage also pass many controls, but those areas have confirmed findings.

Known gaps are Firecracker (no usable KVM on hosted runners), multi-identity authorization (self-auth uses one user owning multiple groups), and investigations cut short by tooling limits: gRPC/HTTP2 framing, request randomization near limits, and deeper instance-name and action-cache probing. Distributed/multi-node deployments, macOS/Windows execution, browser workflows, true persistent-worker multiplexing, and long-duration or power-loss durability remain outside the demonstrated coverage.

## Scope and evidence

This is an evidence reconciliation, not a new component-test run. Main snapshot: `016e4693a43d45dde453a8c585c430150e83d517`. Gathered every finding from `origin/main` and all open PR heads after `git fetch origin '+refs/heads/*:refs/remotes/origin/*'`. A copy containing a Verification section takes precedence over an unverified copy; among verified copies, recorded version history takes precedence, then main. README, TEMPLATE, INDEX and the previous REPORT are supporting documents, excluded from counts. Links pin the selected copies to exact commits, including findings on unmerged branches.

Duplicate reproducers are folded by shared contract and observed failure, including compressed read limits, past-EOF panics, unfinished-write responses, malformed-zstd status, overlapping output Trees, and Docker input/output variants. Docker variants are labeled “also reproduces under Docker isolation”; their original unverified status is preserved. Inclusion does not turn a duplicate into independent verification. Images, two-run evidence, source analysis, prerequisites and historical invocation links remain in each linked finding. Every confirmed entry concerns released enterprise-v2.314.0 images; exact default digests are in [docs/environment.md](../docs/environment.md), and historical digests are in the linked version matrices.

The app QueryWriteStatus finding is independently confirmed, while the proxy report is independently rejected as intended forwarding. Both verdicts are retained, with the disagreement called out below; the proxy rejection does not erase the app contract finding.

Commands below reproduce on hosted runners. Use the public-fetch checkout setup in “How to run”; if a target is not on main, use the test branch/commit recorded in its linked finding with current main's harness. This report does not import pending tests. Historical commands require the exact component digest overrides recorded in each finding.

## Confirmed defects

### High

#### Docker isolation ignores network=off and retains the configured bridge

**Components:** executor. **Severity:** high.

The released [platform property contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go#L160) defines `network=off` as no network access and says it takes precedence over `dockerNetwork`. Docker actions should use network mode `none`, exposing only loopback and no bridge route, both with `dockerNetwork` absent and when the command also specifies `dockerNetwork=host`. Both cases complete with OK status and exit code 0, but `ls /sys/class/net` reports `eth0` and `lo`, and `/proc/net/route` includes a default route on `eth0`. The configured `action-network` bridge remains attached.

**Spec/source reference:** [platform documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-platforms.md#L361-L375); [legacy property documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-platforms.md#L393-L400).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-docker:network_off_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [executor-isolation-docker-network-off-ignored.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-isolation-docker-network-off-ignored.md) — confirmed.

### Medium

#### App and cache proxy advertise unlimited batches but reject a 4,000,001-byte batch read

**Components:** app / cacheproxy. **Severity:** medium.

The [REAPI CacheCapabilities contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) defines `max_batch_total_size_bytes=0` as no limit set, apart from actual protocol message-size restrictions. BatchReadBlobs says a server's combined blob-size limit can be obtained through GetCapabilities. Both app and proxy return `max_batch_total_size_bytes=0`. Both accept a 4,000,001-byte blob in BatchUpdateBlobs, and ByteStream reads return the exact data. Both reject the tiny BatchReadBlobs request naming that same digest with `INVALID_ARGUMENT: BatchReadBlobs request exceeds server limit of 4000000 bytes`. The fixed application limit is not advertised and the 4,000,001-byte payload is below the standard 4 MiB message limit.

**Spec/source reference:** [REAPI batch-read contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L392-L408); [CacheCapabilities](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L2427-L2431).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:batch_limit_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-capabilities-batch-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-capabilities-batch-read-limit.md) — confirmed.

#### Bare action input mutation poisons subsequent action inputs

**Components:** executor. **Severity:** medium.

Each action's input tree should be materialized with the bytes named by its `InputRootDigest` and the contained `FileNode.digest`. See the [REAPI Action and FileNode definitions](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto). A later action naming the original input digest should read `original\n`, even if an earlier action modified its workspace copy. If input modification is unsupported, preventing it would also avoid corrupting later materialization. The first action runs `cat input; printf 'poisoned\n' > input` in its workspace. The second action has a distinct command/action digest, no runner recycling, no workspace preservation, and reads the same input digest. It succeeds but reads `poisoned\n` rather than `original\n`. An intervening authenticated read of that digest from the fixture-local app CAS returns the original bytes.

**Spec/source reference:** [REAPI definitions at commit 6def1c5](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto); [dirtools.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-firecracker:input_mutation_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [executor-isolation-firecracker-input-cache-poisoning.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-isolation-firecracker-input-cache-poisoning.md) — confirmed.

#### Bare executor ignores the action's PATH when resolving an unqualified executable

**Components:** executor. **Severity:** medium.

[REAPI Command.arguments](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) permits an unqualified executable resolved through the OS equivalent of PATH and states that an explicitly supplied PATH **SHOULD** be respected. This is a SHOULD recommendation, not a MUST requirement. Relative executables are resolved from the command working directory. The input executable `tools/envargs-tool`, working_directory=`tools` and PATH=`.` should yield `input-tool:path` when invoked as `envargs-tool path`; an explicit `./envargs-tool` control succeeds. The unqualified action returns `ExecuteResponse.status.code=5` (NOT_FOUND), exit code -2, and `exec: "envargs-tool": executable file not found in $PATH`.

**Spec/source reference:** [REAPI Command.arguments at 6def1c5d27a5](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L760); [capabilities server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go#L61).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/env-args:path_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [executor-env-args-command-path.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-env-args-command-path.md) — confirmed.

#### ByteStream Read past EOF panics instead of returning OUT_OF_RANGE

**Components:** app / cacheproxy. **Severity:** medium.

The [ByteStream read_offset contract](https://github.com/googleapis/googleapis/blob/f5f34cc12374c1b3c11240e3d733b34ad0f63f49/google/bytestream/bytestream.proto#L99) requires OUT_OF_RANGE beyond the resource size. A read one byte beyond a small inline Pebble blob instead returns INTERNAL: A panic occurred, with a slice-bounds stack in filestore.InlineReader. The direct app and cold/warm proxy reproducers exercise the same unchecked-offset defect; the cold proxy forwards the error and the warm proxy can panic in its own inline reader. The panic is recovered, normal readback remains sound, and process termination or wider denial of service is not established.

**Spec/source reference:** [ByteStream read_offset contract](https://github.com/googleapis/googleapis/blob/f5f34cc12374c1b3c11240e3d733b34ad0f63f49/google/bytestream/bytestream.proto#L99); [Read preconditions](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go#L107).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:read_past_end_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional duplicate reproducers (verification status is listed with each evidence file):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/bytestream:read_beyond_eof --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.260.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [app-bytestream-read-past-end-panic.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-bytestream-read-past-end-panic.md) — confirmed.
- [cacheproxy-bytestream-beyond-eof.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/cacheproxy-bytestream-beyond-eof.md) — confirmed.

#### CDC-uploaded blobs return NotFound for bounded ByteStream reads

**Components:** cacheproxy (with app fallback). **Severity:** medium.

**Expected:** An identity ByteStream Read of an existing CAS resource with `read_offset=12345` and `read_limit=17` returns bytes `[12345:12362]` with successful completion. [ByteStream ReadRequest](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto) permits positive limits. Upload interception and internal chunk storage should preserve that resource's externally visible semantics.

**Actual:** A seeded 1 MiB incompressible payload is uploaded successfully through a CDC-enabled writer proxy. Full reads return exactly the original payload from all three endpoints: app, writer proxy, and a separate cold reader proxy. The reader proxy then returns `NotFound` for the same resource with offset 12345 and limit 17. Both explicit, uncached repro runs failed at that assertion; fixture startup, upload, all full-read checks, and cleanup succeeded.

**Spec/source reference:** [REAPI CAS download rules](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L290); [ByteStream ReadRequest contract](https://github.com/googleapis/googleapis/blob/f5f34cc12374c1b3c11240e3d733b34ad0f63f49/google/bytestream/bytestream.proto#L95).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/read-through:cdc_bounded_read_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [cacheproxy-read-through-cdc-bounded-read.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/cacheproxy-read-through-cdc-bounded-read.md) — confirmed.

#### Closing an unfinished ByteStream Write omits the response message

**Components:** app / cacheproxy. **Severity:** medium.

The [ByteStream Write contract](https://github.com/googleapis/googleapis/blob/f5f34cc12374c1b3c11240e3d733b34ad0f63f49/google/bytestream/bytestream.proto#L66) requires a WriteResponse when the client closes the request channel; finish_write controls finalization separately. Orderly half-close of a fresh partial upload instead produces an INTERNAL cardinality error because the server sends no response. Zero committed bytes would be acceptable: retaining partial progress is not assumed. The seeded app/proxy report observes the same behavior, with no context cancellation or broken network required.

**Spec/source reference:** [ByteStream Write contract](https://github.com/googleapis/googleapis/blob/f5f34cc12374c1b3c11240e3d733b34ad0f63f49/google/bytestream/bytestream.proto#L66); [Write loop](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go#L802).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:unfinished_response_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional duplicate reproducers (verification status is listed with each evidence file):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-cas:unfinished_write_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-bytestream-unfinished-write-response.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-bytestream-unfinished-write-response.md) — confirmed.
- [crosscutting-rand-cas-unfinished-write-response.md](https://github.com/altdansalt/buildbuddy-validation/blob/9b99c09250c6dfd61806460f2122496f04f6b747/findings/crosscutting-rand-cas-unfinished-write-response.md) — unverified duplicate reproducer.

#### Execute reports the action digest as missing when only the command is absent

**Components:** app. **Severity:** medium.

The [REAPI Execution contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) requires FAILED_PRECONDITION for missing inputs/commands and recommends a PreconditionFailure with MISSING violations naming each absent blob. Once the server emits this detail, its subject must identify the absent command: `blobs/7ebbf16769265803a4ce811ff293c60b82f61e061dade999843cf9299dda5745/33`. Execute returns FAILED_PRECONDITION with a MISSING violation naming the uploaded Action: `blobs/2b69dd99eb501d936e61aeed1c8ef18f94d46051cc831a15c623b2c6b86fb0b1/138`. The human-readable error also incorrectly says this action is not found.

**Spec/source reference:** [REAPI Execute contract, lines 87–111](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L87); [v2.314.0 fetchCommand, lines 1997–2008](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go#L1997).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/execution-api:missing_command_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.260.0–2.314.0 (2.260.0, 2.280.0, 2.300.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [app-execution-api-missing-command-digest.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-execution-api-missing-command-digest.md) — confirmed.

#### Executor accepts legacy output type mismatches

**Components:** executor. **Severity:** medium.

[REAPI ActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) requires FAILED_PRECONDITION (9) when an output_files path is a directory, or a legacy output symlink points to the wrong type. The corresponding output_directories contract likewise requires directory outputs. These commands use only legacy fields, so modern type inference does not apply. All four cases return status OK (0), exit code 0: directory declared as file is returned in output_directories; file declared as directory is returned in output_files; a directory symlink declared as file is returned in output_file_symlinks; a file symlink declared as directory is returned in output_directory_symlinks.

**Spec/source reference:** [REAPI ActionResult contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1253-L1384); [NewDirHelper](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L127-L153).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:legacy_type_mismatch --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional duplicate reproducers (verification status is listed with each evidence file):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs-docker:legacy_type_mismatch --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [executor-outputs-legacy-type-mismatch.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-legacy-type-mismatch.md) — confirmed.
- [executor-outputs-docker-legacy-type-mismatch.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-docker-legacy-type-mismatch.md) — also reproduces under Docker isolation (author report; unverified duplicate).

#### Executor cannot download large inputs from an identity-only cache even with compression disabled

**Components:** executor / app. **Severity:** medium.

**Expected:** With `cache.zstd_transcoding_enabled=false` on the app and both `cache.client.enable_download_compression=false` and `cache.client.enable_upload_compression=false` on the executor, the executor should download identity data and execute successfully. The download flag's release-source description says it controls compression of cache downloads.

**Actual:** A single 2,097,153-byte input file (one byte above the release's 2 MiB BatchReadBlobs threshold) prevents execution. Input upload uses identity; the executor returns ExecuteResponse status 12 (`UNIMPLEMENTED`): `download inputs: could not fetch inputs: Unsupported compressor ZSTD`. The shell action never runs. The bug test asserts successful execution and byte-identical output, so it fails until the behavior is corrected.

**Spec/source reference:** [cache configuration documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md); [flag definition](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/config/config.go#L7).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/compression:identity_large_input_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [executor-compression-identity-large-input.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-compression-identity-large-input.md) — confirmed.

#### Executor output uploader uses zstd without consulting cache capabilities

**Components:** executor / app. **Severity:** medium.

**Expected:** A cache that does not advertise zstd should receive identity output uploads. Enabling the executor's upload compression option should use compression only when supported by the cache. Execution and output bytes should match those with upload compression disabled.

**Actual:** The app runs with `cache.zstd_transcoding_enabled=false`. The executor runs with `cache.client.enable_upload_compression=true` (the release default) and `cache.client.enable_download_compression=false`. One 100-byte input is uploaded using identity. The command transforms it into a distinct 100-byte output. The output size is exactly the cache client's minimum compression threshold. Execution returns status 14 (`UNAVAILABLE`) wrapping an `UNIMPLEMENTED` output upload error. The output uploader chooses zstd despite the app's identity-only capabilities. Inputs fit within BatchReadBlobs, excluding the separate forced large-download defect. Disabling upload compression succeeds in the passing identity-only boundary coverage.

**Spec/source reference:** [cache configuration documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md); [flag definition](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/config/config.go#L7).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/compression:identity_small_output_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [executor-compression-unadvertised-output-zstd.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-compression-unadvertised-output-zstd.md) — confirmed.

#### FetchBlob maps origin timeout, access denial, and checksum mismatch to NOT_FOUND

**Components:** app. **Severity:** medium.

The [pinned Remote Asset API](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto) explicitly requires inline `DEADLINE_EXCEEDED` when `FetchBlobRequest.timeout` expires retrieving origin content. Its response status documentation identifies `PERMISSION_DENIED` for rejection by the remote server and `ABORTED` for a failed consistency check. All three requests return gRPC OK with inline status 5 (`NOT_FOUND`). The messages retain evidence of deadline expiry, HTTP 403, or rejected digest validation, but the structured code loses those distinctions.

**Spec/source reference:** [Remote Asset spec at the repository's pinned revision](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto#L162-L175); [FetchBlobResponse.status](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto#L213-L225).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/remote-asset:origin_status_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.260.0–2.314.0 (2.260.0, 2.280.0, 2.300.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [app-remote-asset-origin-error-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-remote-asset-origin-error-status.md) — confirmed.

#### GetActionResult skips availability checks for stdout_digest and stderr_digest

**Components:** app. **Severity:** medium.

[REAPI ActionCache.GetActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) says implementations SHOULD ensure any referenced CAS blobs are available when returning an ActionResult. This includes stdout_digest and stderr_digest. A missing referenced blob should cause a cache miss (`NOT_FOUND`), matching this release's handling of missing output files and Trees. The test uploads a valid Command, input root and Action, then updates the result with only a stdout_digest or stderr_digest whose nonempty CAS blob has never been uploaded. A direct ByteStream read verifies `NOT_FOUND` for that digest. GetActionResult nonetheless succeeds and returns the dangling digest in both subtests.

**Spec/source reference:** [REAPI GetActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L158-L174); [Pinned source, ValidateActionResult and fetchActionResult](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go#L153-L252).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/action-cache:missing_streams_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.260.0–2.314.0 (2.260.0, 2.280.0, 2.300.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [app-action-cache-missing-stream-blobs.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-action-cache-missing-stream-blobs.md) — confirmed.

#### GetTree aborts partial trees and returns the wrong missing-root status

**Components:** app / cacheproxy. **Severity:** medium.

[REAPI GetTree](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) explicitly requires: "If part of the tree is missing from the CAS, the server will return the portion present and omit the rest." A stored root referencing one absent child must therefore return the root and omit that child. If the root itself is absent, the documented status is `NOT_FOUND`. Both app and proxy return `FailedPrecondition` for a missing child and emit no available root Directory. A missing root also returns `FailedPrecondition` instead of `NotFound`.

**Spec/source reference:** [released proxy implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go#L501); [cache configuration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:bugs_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-cas-gettree-missing-directories.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-cas-gettree-missing-directories.md) — confirmed.

#### Graceful action timeout leaves a TERM-ignoring descendant running after the grace period

**Components:** executor. **Severity:** medium.

[REAPI Action.timeout](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) specifies a timeout after which execution should be killed. BuildBuddy additionally supports the [`termination-grace-period` platform property](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go): the executor sends graceful termination at the action timeout and sets a force-shutdown deadline at timeout plus grace period. A descendant that ignores TERM should be stopped by that final deadline, including when its parent handles TERM and exits zero. A one-second action with a two-second termination grace period starts a background child that ignores TERM, sleeps five seconds, and writes `survived` to a unique `/tmp` marker. The parent handles TERM by printing `terminated` and exiting zero. Child stdin/stdout/stderr are redirected away from the action's pipes. The parent returns DEADLINE_EXCEEDED with exit -2 and `startedterminated`; a later action waits six seconds and finds the unique child's `survived` marker. The descendant therefore ran beyond the three-second force-shutdown deadline, inside the bare executor container.

**Spec/source reference:** [REAPI Action.timeout](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L691); [platform docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-platforms.md#L311).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:graceful_timeout_child_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [executor-timeouts-cancel-graceful-orphan.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-timeouts-cancel-graceful-orphan.md) — confirmed.

#### Graceful app shutdown unregisters the executor before re-enqueuing its leased task

**Components:** app / executor. **Severity:** medium.

**Expected:** With scheduler/task records retained in test-local Redis, the original operation should recover (possibly retry execution) after a graceful app replacement and return the shell action's successful output through `WaitExecution`. This is a scheduler recovery expectation, not a claim that REAPI promises uninterrupted streams or exactly-once execution during server downtime.

**Actual:** Both v2.313.0 -> v2.314.0 upgrade and v2.314.0 -> v2.314.0 replacement return a terminal `ExecuteResponse.status` with code 14 (`Unavailable`) and message `No registered executors in pool "" with os "linux" with arch "amd64".` The RPC itself succeeds; this is the stored terminal outcome of the original operation, not a transient client connection failure.

**Spec/source reference:** [Scheduler v2.314.0](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/enterprise/server/scheduling/scheduler_server/scheduler_server.go#L3084); [Executor task leaser](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/enterprise/server/scheduling/task_leaser/task_leaser.go#L24).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/upgrade-compat:scheduler_shutdown_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded. Both 2.313.0 → 2.314.0 upgrade and 2.314.0 → 2.314.0 replacement reproduce; this does not establish an introduction boundary.

**Evidence files:**

- [app-upgrade-compat-running-task-shutdown.md](https://github.com/altdansalt/buildbuddy-validation/blob/d8add1f9450c14200fa262bd644a1c929ed0fd5c/findings/app-upgrade-compat-running-task-shutdown.md) — confirmed.

#### Legacy fields suppress symlinks requested through output_paths

**Components:** executor. **Severity:** medium.

[REAPI Command.output_paths](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) says output_files and output_directories are ignored when output_paths is used. ActionResult.output_symlinks must contain each requested output path that exists and is a symlink. The command creates target containing data and link -> target, requests output_paths=[link], and also sets output_files=[ignored]. Execution succeeds but returns no output files, directories, or symlinks. The requested link is lost.

**Spec/source reference:** [Command.output_paths](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L843-L871); [output_symlinks contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1286-L1300).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:output_paths_precedence --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional duplicate reproducers (verification status is listed with each evidence file):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs-docker:output_paths_precedence --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [executor-outputs-output-paths-symlink-loss.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-output-paths-symlink-loss.md) — confirmed.
- [executor-outputs-docker-output-paths-symlink-loss.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-docker-output-paths-symlink-loss.md) — also reproduces under Docker isolation (author report; unverified duplicate).

#### Negative OutputFile digest size bypasses ActionResult validation

**Components:** app. **Severity:** medium.

[REAPI Digest](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) defines size_bytes as the size of the blob in bytes; a negative value cannot describe a blob. UpdateActionResult documents `INVALID_ARGUMENT` for invalid arguments. The app should reject an ActionResult containing a negative OutputFile digest size with `INVALID_ARGUMENT` and should not serve it as a successful cache hit. After uploading a valid Command, input root and Action, UpdateActionResult accepts an OutputFile with a valid SHA256 hash and size_bytes=-1. GetActionResult returns the same malformed file digest successfully. No output blob was uploaded or could have the stated size.

**Spec/source reference:** [REAPI Digest](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1146-L1184); [UpdateActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L175-L194).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/action-cache:malformed_result_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.260.0–2.314.0 (2.260.0, 2.280.0, 2.300.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [app-action-cache-negative-output-size.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-action-cache-negative-output-size.md) — confirmed.

#### Overlapping directory outputs produce an incomplete parent Tree

**Components:** executor. **Severity:** medium.

[REAPI](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) allows an output directory to be a parent of another output directory. A Tree includes its root Directory and all descendant Directory messages in children. Each returned output directory's Tree must be independently complete. The command writes tree/nested/file containing inner, and requests sorted output_paths=[tree, tree/nested]. Execution succeeds and returns both directories. Decoding the tree output's CAS Tree gives zero children instead of the required nested Directory message. Its 82-byte Tree references the nested directory but omits its contents. The separately returned tree/nested Tree is 80 bytes.

**Spec/source reference:** [REAPI Command contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L843); [Tree definition](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1450).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:overlapping_directories --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional duplicate reproducers (verification status is listed with each evidence file):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-exec:overlapping_directory_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs-docker:overlapping_directories --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [executor-outputs-overlapping-directory-tree.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-overlapping-directory-tree.md) — confirmed.
- [executor-rand-exec-overlapping-directory-tree.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-rand-exec-overlapping-directory-tree.md) — confirmed.
- [executor-outputs-docker-overlapping-directory-tree.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-docker-overlapping-directory-tree.md) — also reproduces under Docker isolation (author report; unverified duplicate).

#### QueryWriteStatus is unimplemented for unknown and finalized uploads

**Components:** app. **Severity:** medium.

**Expected:** The [ByteStream contract](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto) requires `NOT_FOUND` for a nonexistent upload, and a `QueryWriteStatusResponse` for an existing resource. After a successfully finalized uncompressed upload, `complete` should be true and `committed_size` should equal the uploaded size. The returned size is the offset clients use to resume an interrupted write; retaining zero bytes of partial progress is allowed, but does not justify returning `UNIMPLEMENTED` for completed resources.

**Actual:** Both querying a new upload name and querying that same name after successful `Write(finish_write=true)` return `UNIMPLEMENTED: not implemented`. The test checks the successful write's committed size and reads back identical bytes before testing completed status, ruling out missing data and auth/config errors.

**Spec/source reference:** [ByteStream QueryWriteStatus contract](https://github.com/googleapis/googleapis/blob/f5f34cc12374c1b3c11240e3d733b34ad0f63f49/google/bytestream/bytestream.proto#L76); [pinned handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go#L867).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:query_status_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-bytestream-query-write-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-bytestream-query-write-status.md) — confirmed.

The rejected proxy report below covers the same unconditional app response. Reviewers disagree on whether intentional lack of implementation excuses the ByteStream contract; retain this bounded app conformance defect and do not count a separate proxy defect.

#### Queued Execute and WaitExecution do not report retriable failure after Redis restart

**Components:** app / executor. **Severity:** medium.

**Expected:** After Redis recovers, each admitted action should either complete with its actual execution result or receive a clear retriable failure. A lost scheduler task should not leave clients waiting for an execution that can no longer run. [REAPI Execute / WaitExecution](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) allows WaitExecution to report NOT_FOUND when an operation no longer exists due to a transient condition, instructing the client to retry Execute. Errors while running an action belong in ExecuteResponse.status, not Operation.error. The suite additionally checks bounded operational recovery; REAPI does not prescribe a particular recovery time bound.

**Actual:** A five-second action occupies the executor's sole execution slot. A second, zero-delay action is admitted and queued; its command marker does not exist. The test opens WaitExecution for that second operation and restarts only the fixture's Redis sidecar. Redis responds PONG after restart. The executor later attempts to lease the queued task and the app logs `task does not exist`. Neither the queued Execute stream nor WaitExecution returns completion or a retriable status before the client's 45-second deadline. A fresh action then successfully executes and its CAS-backed output is fetched.

**Spec/source reference:** [REAPI Execution](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L65); [monitoring flag](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go#L93).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/redis-dependency:queued_restart_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [crosscutting-redis-dependency-queued-stream-stall.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/crosscutting-redis-dependency-queued-stream-stall.md) — confirmed.

#### Recycled bare runner retains task-scoped TMPDIR files across actions

**Components:** executor. **Severity:** medium.

The released [bare provider](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/bare/bare.go) describes `executor.bare.enable_tmpdir` as provisioning a temporary directory for each task and removing it when the task completes. A subsequent action should not find a previous action's file in the executor-provided TMPDIR. [REAPI Command](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) allows worker-provided default environment variables; this finding concerns the lifetime contract of that supplied directory, not a spec requirement to isolate arbitrary absolute paths. Action one writes `private-first-action-data` to `$TMPDIR/recycling-secret`. Action two, with the same recycling key and no inputs, reads that file and exits 42. Equal `pwd` values confirm actual runner reuse. Both ExecuteResponses have OK status; the test fails on the command exit code.

**Spec/source reference:** [REAPI Command.environment_variables](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L777); [recycling docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-platforms.md#L258).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/runner-recycling:tmp_leak --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [executor-runner-recycling-tmpdir-leak.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-runner-recycling-tmpdir-leak.md) — confirmed.

#### SIGHUP resets CLI-overridden CSP flag to its default

**Components:** app. **Severity:** medium.

**Expected:** The explicit `--app.strict_csp_enabled=true` override should remain true after SIGHUP, even when YAML contains `app.strict_csp_enabled: false`. The pinned [reload contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/config/config.go) says it resets defaults, re-parses flags, and loads the config file. The [YAML loader](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/flagutil/yaml/yaml.go) explicitly preserves command-line precedence.

**Actual:** Startup returns Content-Security-Policy on GET `/`. After an accepted reload, the same handler returns no CSP header. Independent uncached runs confirm this behavior.

**Spec/source reference:** [Enterprise main](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/cmd/server/main.go#L164-L179); [Reload and SIGHUP contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/config/config.go#L136-L184).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/config-validation:cli_reload_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [crosscutting-config-validation-cli-override-reload.md](https://github.com/altdansalt/buildbuddy-validation/blob/039b70ddc93b69e99de7349d7996ddd48bf7ad0a/findings/crosscutting-config-validation-cli-override-reload.md) — confirmed.

#### SpliceBlob succeeds but ByteStream cannot read the resulting small blob

**Components:** app. **Severity:** medium.

**Expected:** After uploading all chunks and successfully splicing their digests, a client should be able to read the resulting blob. There is no advertised minimum splice size. An unsupported size should be rejected rather than accepted as a successful upload. [REAPI chunk upload contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

**Actual:** Upload `abc` and `def`, then splice them into the digest of `abcdef`. Splice succeeds, and SplitBlob returns the two correct chunk digests, but ByteStream Read for the six-byte result returns NOT_FOUND. Independent uncached runs confirm this behavior; invocation links are in the finding.

**Spec/source reference:** [REAPI v2.12 SpliceBlob contract](https://github.com/bazelbuild/remote-apis/blob/v2.12.0/build/bazel/remote/execution/v2/remote_execution.proto#L503-L559); [ByteStream read semantics](https://github.com/bazelbuild/remote-apis/blob/v2.12.0/build/bazel/remote/execution/v2/remote_execution.proto#L295).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:small_splice_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-capabilities-small-splice-unreadable.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-capabilities-small-splice-unreadable.md) — confirmed.

#### Valid unknown WaitExecution operation names wait until the client deadline

**Components:** app. **Severity:** medium.

**Expected:** [REAPI WaitExecution](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) specifies NOT_FOUND when the operation no longer exists. A syntactically valid but never-created name on a fresh app should promptly return NOT_FOUND, allowing the client to recover.

**Actual:** Two independent fresh fixtures returned no operation and no server error during a five-second bounded wait. The client eventually saw DeadlineExceeded.

**Spec/source reference:** [REAPI contract at 6def1c5d27a5](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L122-L138); [execution server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go#L1324-L1389).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/execution-api:unknown_operation_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-execution-api-unknown-operation-hangs.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-execution-api-unknown-operation-hangs.md) — confirmed.

#### v1 GetLog starts at the last log chunk instead of the first

**Components:** app. **Severity:** medium.

The public [GetLog request and response](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/api/v1/log.proto) describe `page_token` as the `next_page_token` returned by a previous request, and `next_page_token` as empty when there are no more logs. An initial request with no previous token, followed by every returned token, should expose the complete stored build log in order. Upload 2,250,021 bytes in small BES Progress events, starting with `FIRST-LINE` and ending with `LAST-LINE`. After the server acknowledges the entire stream and completes it, call `/api/v1/GetLog` with an invocation selector and empty page token. Following every returned token yields only 270,217 bytes: the final nonempty chunk followed by an empty response. The first line and approximately 1.98 MB of earlier output are missing.

**Spec/source reference:** [v2.314.0 API documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-api.md#GetLog); [public log proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/api/v1/log.proto).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/invocations-api:log_pagination_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.260.0–2.314.0 (2.260.0, 2.280.0, 2.300.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [app-invocations-api-log-pagination.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-invocations-api-log-pagination.md) — confirmed.

### Low

#### Artifact download reports missing invocation as HTTP 500

**Components:** app. **Severity:** low.

A nonexistent invocation should return HTTP 404, not a server error. The [released serveArtifact handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/buildbuddy_server/buildbuddy_server.go) explicitly has a NotFound-to-404 branch for invocation lookup. See [RFC 9110 section 15.5.5](https://www.rfc-editor.org/rfc/rfc9110.html#section-15.5.5). Cookie-authenticated `GET /file/download?artifact=build_log&invocation_id=00000000-0000-0000-0000-000000000000` returns HTTP 500 and `rpc error: code = Internal desc = Internal server error`. Both isolated reproductions fail on the expected 404 assertion.

**Spec/source reference:** [ServeHTTP and serveArtifact](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/buildbuddy_server/buildbuddy_server.go#L2611); [LookupInvocation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/backends/invocationdb/invocationdb.go#L179).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/http-endpoints:missing_invocation_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.260.0–2.314.0 (2.260.0, 2.280.0, 2.300.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [app-http-endpoints-missing-invocation-500.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-http-endpoints-missing-invocation-500.md) — confirmed.

#### BES accepts a changed stream identity and ACKs the wrong invocation

**Components:** app. **Severity:** low.

[PublishBuildToolEventStream](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto#L72-L75) publishes events belonging to the same stream. Each OrderedBuildEvent identifies its stream, and each ACK identifies the stream containing the committed event. Reject a change of stream identity within an RPC. In particular, do not ACK an event for invocation B as an event for invocation A. One RPC sends BuildStarted for invocation A at sequence 1, then BuildFinished for a distinct invocation B at sequence 2. Both invocation_id and build_id change. The server completes successfully and emits ACKs for sequences 1 and 2, both with A's StreamId. No event for A at sequence 2 was sent.

**Spec/source reference:** [server receive loop](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_server/build_event_server.go#L188-L209); [postProcessStream](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_server/build_event_server.go#L229-L266).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:changed_identity --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.260.0–2.314.0 (2.260.0, 2.280.0, 2.300.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [app-bes-changed-stream-id.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-bes-changed-stream-id.md) — confirmed.

#### BatchUpdateBlobs accepts nonempty data for the empty digest

**Components:** app. **Severity:** low.

BatchUpdateBlobs must report per-item INVALID_ARGUMENT when the digest does not match the supplied data. The SHA-256 empty digest with size zero does not describe the supplied bytes `not empty`. A valid peer should still succeed. The RPC succeeds and returns per-item OK for the empty digest despite nonempty data. The test submits this alongside a valid upload. Both fresh runs return expected code 3 versus actual code 0. Reference: [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto).

**Spec/source reference:** [released enterprise cache documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-cache-setup.md); [cache configuration documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-batch:empty_digest_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-cas-batch-empty-digest-validation.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-cas-batch-empty-digest-validation.md) — confirmed.

#### BatchUpdateBlobs reports INTERNAL for malformed client zstd data

**Components:** app. **Severity:** low.

Malformed compressed client data should return per-entry INVALID_ARGUMENT, while valid sibling requests succeed. The [REAPI CAS contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) describes BatchUpdateBlobs as equivalent to ByteStream uploads and specifies INVALID_ARGUMENT when the digest does not match the provided data. Its compressed-upload contract requires validating the uncompressed hash and size. A string that is not a zstd frame cannot represent the declared uncompressed blob. [gRPC status guidance](https://grpc.io/docs/guides/status-codes/) classifies invalid client arguments as INVALID_ARGUMENT and reserves INTERNAL for broken server invariants. Uploading `not a zstd frame` with compressor ZSTD returns per-entry code 13 (INTERNAL), with `Failed to decompress zstd-compressed blob: invalid input: magic number mismatch`. The overall batch RPC succeeds. The valid identity-compressed sibling is stored and readable; FindMissingBlobs confirms the corrupt entry is absent.

**Spec/source reference:** [REAPI BatchUpdateBlobs](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L372-L387); [compressed upload rules](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L246-L260).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/compression:corrupt_batch_zstd_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional duplicate reproducers (verification status is listed with each evidence file):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-batch:malformed_zstd_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-compression-corrupt-batch-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-compression-corrupt-batch-status.md) — confirmed.
- [app-cas-batch-malformed-zstd-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-cas-batch-malformed-zstd-status.md) — unverified duplicate reproducer.

#### CAS tree traversal reports an uploaded metadata-only directory as missing

**Components:** app / executor. **Severity:** low.

An empty child Directory carrying NodeProperties (mtime and unix_mode) is valid under the [REAPI Directory contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto). Optional properties may be ignored, but their presence should not cause existing CAS content to be reported missing. The `optional-node-properties` subtest uploads an empty child Directory with metadata, then Execute returns FAILED_PRECONDITION (9), wrapping a GetTree NotFound error. The second reproduction explicitly reads the child blob back from fixture CAS and checks its nonzero length before Execute; these checks pass. Thus this is not an absent upload or authentication failure. The matching nonempty-directory metadata control is in the untagged suite, and ordinary empty directories are covered there as well.

**Spec/source reference:** [REAPI Directory/NodeProperties](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1049); [capabilities server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go#L94).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/input-tree:input_tree_limits_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional duplicate reproducers (verification status is listed with each evidence file):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/inputs-docker:input_tree_limits_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-input-tree-metadata-only-directory.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-input-tree-metadata-only-directory.md) — confirmed.
- [executor-inputs-docker-metadata-only-directory.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-inputs-docker-metadata-only-directory.md) — also reproduces under Docker isolation (author report; unverified duplicate).

#### Cache proxy reports exec_enabled=true but Execute returns unknown service

**Components:** app / cacheproxy. **Severity:** low.

The [REAPI ExecutionCapabilities contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) defines `exec_enabled` as whether remote execution is enabled for the particular server/instance. An endpoint without an Execution service should omit execution capabilities or report `exec_enabled=false`, rather than forward the upstream app's execution advertisement unchanged. The proxy returns the upstream app's `execution_capabilities` with `exec_enabled=true`, SHA256 and additional digest functions, and priority range -1000..1000. An Execute request to that proxy returns `UNIMPLEMENTED` with `unknown service build.bazel.remote.execution.v2.Execution`.

**Spec/source reference:** [REAPI GetCapabilities contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L636-L643); [exec_enabled](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L2577-L2578).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:proxy_execution_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-capabilities-proxy-execution.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-capabilities-proxy-execution.md) — confirmed.

#### Checksum cache hits bypass oldest_content_accepted

**Components:** app. **Severity:** low.

The [pinned Remote Asset API](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto) permits reuse of cached fetched content subject to `oldest_content_accepted`. That field is the oldest accepted time measured when content was pushed or its underlying origin retrieval started. The repro fetches a SHA256-qualified asset from a test-owned origin, waits until after completion and sets oldest_content_accepted to a new current timestamp. A second origin retrieval should satisfy that fresh lower bound. The initial fetch returns OK and produces exactly one origin request. The second fetch also returns OK, but there is still exactly one origin request: the older checksum cache entry was returned without consulting the origin.

**Spec/source reference:** [Remote Asset Fetch contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto#L90-L103); [the field definition](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto#L177-L185).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/remote-asset:freshness_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.260.0–2.314.0 (2.260.0, 2.280.0, 2.300.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [app-remote-asset-oldest-content-accepted.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-remote-asset-oldest-content-accepted.md) — confirmed.

#### Compressed ByteStream Read accepts nonzero read_limit instead of INVALID_ARGUMENT

**Components:** app / cacheproxy. **Severity:** low.

[REAPI compressed download rules](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L313-L317) require INVALID_ARGUMENT for every nonzero compressed Read read_limit. App and proxy instead return OK and compressed partial content. Unlimited controls return correct full bytes. This is invalid-request conformance and interoperability impact, with no demonstrated persistent corruption; app, proxy, large-blob and seeded-property reports describe the same defect.

**Spec/source reference:** [REAPI requirement](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L313-L317); [pinned app implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go#L106-L119).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/compression:compressed_read_limit_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional duplicate reproducers (verification status is listed with each evidence file):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:compressed_limit_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/compression:compressed_read_limit_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/large-blobs:compressed_read_limit_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-cas:compressed_read_limit_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.260.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [app-compression-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-compression-read-limit.md) — confirmed.
- [app-bytestream-compressed-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-bytestream-compressed-read-limit.md) — unverified duplicate reproducer.
- [cacheproxy-compression-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/cacheproxy-compression-read-limit.md) — confirmed.
- [cacheproxy-large-blobs-compressed-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/cacheproxy-large-blobs-compressed-read-limit.md) — unverified duplicate reproducer.
- [crosscutting-rand-cas-compressed-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/crosscutting-rand-cas-compressed-read-limit.md) — unverified duplicate reproducer.

#### Docker actions accept termination-grace-period but cannot deliver the graceful signal

**Components:** executor. **Severity:** low.

**Expected:** With an action timeout of 1 second and `termination-grace-period=2s`, send SIGTERM at the timeout and allow the installed handler to print `terminated` before force termination. The final response should remain DeadlineExceeded. Alternatively, reject the unsupported combination explicitly rather than accepting a grace period that cannot be honored.

**Actual:** Two separate runs returned DeadlineExceeded with exit -2 and stdout `started`, missing `terminated`:

**Spec/source reference:** [v2.314.0 platform documentation](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/docs/rbe-platforms.md#execution-timeout-properties); [REAPI Action.timeout](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L691-L715).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:docker_graceful_timeout_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [executor-timeouts-cancel-docker-grace-unimplemented.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-timeouts-cancel-docker-grace-unimplemented.md) — confirmed.

#### Explicit nested output file omitted from OutputFiles

**Components:** executor. **Severity:** low.

Command.output_paths says a resulting file is returned in an OutputFile field. A separately requested directory Tree does not remove that explicitly listed file path. The command writes tree/file and tree/nested/file and requests sorted output_paths=[tree, tree/nested, tree/nested/file]. Execution succeeds with two OutputDirectories and zero OutputFiles; the explicitly requested tree/nested/file is omitted. Reference: [REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

**Spec/source reference:** [ActionResult.output_files contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1253); [Command.output_paths contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L843).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:overlapping_file --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional duplicate reproducers (verification status is listed with each evidence file):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs-docker:overlapping_file --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [executor-outputs-overlapping-file.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-overlapping-file.md) — confirmed.
- [executor-outputs-docker-overlapping-file.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-docker-overlapping-file.md) — also reproduces under Docker isolation (author report; unverified duplicate).

#### Fresh BES streams starting above one are ACKed without persistence

**Components:** app. **Severity:** low.

The released [OrderedBuildEvent contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto#L150-L159) says sequence numbers are consecutive natural numbers starting at one. The response identifies an event that has been committed. A brand-new UUID with no earlier events cannot be a suffix retry of an already committed invocation. Reject the invalid initial sequence without ACKing it, or actually retain it with an explicit supported recovery policy. Sending one valid Bazel BuildStarted payload at sequence 2 for a fresh UUID returns an ACK for sequence 2 and successful stream completion. The second reproduction also queries GetInvocation immediately after the ACK: the app returns NotFound.

**Spec/source reference:** [handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_handler/build_event_handler.go#L999-L1017); [FinalizeInvocation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_handler/build_event_handler.go#L852-L856).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:fresh_offset --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.260.0–2.314.0 (2.260.0, 2.280.0, 2.300.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [app-bes-fresh-offset.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-bes-fresh-offset.md) — confirmed.

#### GetCapabilities accepts invalid UTF-8 in instance_name

**Components:** app. **Severity:** low.

Proto3 `string` fields must contain UTF-8 or ASCII text, according to the [protobuf scalar type contract](https://protobuf.dev/programming-guides/proto3/#scalar). The released [Remote Execution API proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/remote_execution.proto) declares `GetCapabilitiesRequest.instance_name` as proto3 `string`, field 1; the [upstream REAPI](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) agrees. The request payload `0a 01 ff` is therefore malformed. The [gRPC status mapping](https://github.com/grpc/grpc/blob/master/doc/statuscodes.md) specifies INTERNAL for an error parsing the request proto. The app returns gRPC OK with populated cache capabilities for `0a 01 ff`. The reproducer sends the bytes with a raw client codec named `proto`, bypassing client-side validation while retaining the ordinary gRPC protobuf content type. The standard Go protobuf decoder rejects these exact bytes in a local control inside the test. A valid `0a 01 61` request succeeds against the same connection, method, credentials, and raw codec before the malformed request is sent.

**Spec/source reference:** [released proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/remote_execution.proto); [upstream REAPI](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/grpc-edges:invalid_utf8_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-grpc-edges-invalid-utf8.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-grpc-edges-invalid-utf8.md) — confirmed.

#### GetExecution ignores ExecutionLookup.target_label on the primary database path

**Components:** app. **Severity:** low.

**Expected:** The released [ExecutionLookup.target_label contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/execution_stats.proto) says: “Only executions for this target will be returned (exact match).” Execute two distinct actions with RequestMetadata.target_id set to `//history:case_0` and `//history:case_1`. A GetExecution lookup for `//history:case_0` should return exactly the first execution.

**Actual:** The lookup returns both executions. Reproduced on two independent fixture-local app/executor pairs:

**Spec/source reference:** [ExecutionLookup contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/execution_stats.proto#L282-L297); [REAPI RequestMetadata definition used by the test](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L2610-L2645).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/execution-history:target_label_filter_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-execution-history-target-label-filter.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-execution-history-target-label-filter.md) — confirmed.

#### GetTree ignores page_size and returns an oversized page

**Components:** app / cacheproxy. **Severity:** low.

GetTreeRequest.page_size is a maximum number of items per page. A two-directory tree requested with page_size=1 must not place both directories in one GetTreeResponse. Traversal order may vary for fresh calls; issued tokens must resume the corresponding suffix. See the [REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto). Both app and proxy return the complete two-directory tree in a single response despite page_size=1. Its next_page_token is empty. Both hosted runs show the same failure. Content is otherwise correct, ruling out malformed input. Reproduction links: [run 1](https://app.buildbuddy.io/invocation/ea7980d9-4bad-42f1-9db4-5d3463f2e3ac) and [run 2](https://app.buildbuddy.io/invocation/41a50bf1-c29b-4d87-905c-f17abc3c7f3d). The linked finding records independent verification.

**Spec/source reference:** [released proxy implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go#L501); [cache configuration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:bugs_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-cas-gettree-page-size.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-cas-gettree-page-size.md) — confirmed.

#### GetTree omits empty root and child Directory messages

**Components:** app / cacheproxy. **Severity:** low.

GetTree must return the Directory messages reachable from its root, including an empty root or child. An empty Directory has a valid zero-byte protobuf encoding and the SHA256 empty-blob digest. Uploading that encoding succeeds. See the [REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto). Both app and proxy return no Directory for an empty root (expected one), and only the parent for a parent containing one empty child (expected two). Both cases reproduce in independent hosted runs. Reproduction links: [run 1](https://app.buildbuddy.io/invocation/ea7980d9-4bad-42f1-9db4-5d3463f2e3ac) and [run 2](https://app.buildbuddy.io/invocation/41a50bf1-c29b-4d87-905c-f17abc3c7f3d). The linked finding records independent verification.

**Spec/source reference:** [released proxy implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go#L501); [cache configuration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:bugs_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-cas-gettree-empty-directories.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-cas-gettree-empty-directories.md) — confirmed.

#### Ignored legacy declaration adds an unrequested top-level symlink

**Components:** executor. **Severity:** low.

**Expected:** [REAPI Command.output_paths](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L843) supersedes output_files and output_directories: both legacy fields must be ignored. A requested directory returns its descendants inside its Tree. The optional legacy compatibility symlink fields may mirror explicitly requested modern symlinks; they do not authorize top-level entries for unrequested descendant paths.

**Actual:** The result contains the correct directory Tree and also OutputFileSymlink(path=tree/link, target=../target), even though tree/link is not an output_paths entry. The ignored legacy declaration affects output selection.

**Spec/source reference:** [pinned REAPI Command specification](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L843-L871); [ActionResult compatibility provision](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1266-L1300).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:ignored_legacy_symlink --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [executor-outputs-ignored-legacy-symlink.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-ignored-legacy-symlink.md) — confirmed.

#### Ignored legacy output directories alter modern action execution

**Components:** executor. **Severity:** low.

[REAPI Command.output_paths](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) requires legacy output_files/output_directories to be ignored when modern output_paths is present. With output_paths=[out] and output_directories=[ignored], the executor still precreates ignored; the command observes it and produces unexpected rather than expected output. The legacy-only precreation probe is an intentional compatibility workaround and is not the confirmed defect. The actionable mixed-field reproducer is output_paths_precedence.

**Spec/source reference:** [NewDirHelper lines 156–169](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L156-L169); [REAPI parent-only setup contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L811-L871).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:output_paths_precedence --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional duplicate reproducers (verification status is listed with each evidence file):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs-docker:legacy_directory_precreation --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [executor-outputs-legacy-directory-precreation.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-legacy-directory-precreation.md) — confirmed.
- [executor-outputs-docker-legacy-directory-precreation.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-docker-legacy-directory-precreation.md) — also reproduces under Docker isolation (author report; unverified duplicate).

#### JSON HTTP RPC responses omit Content-Type

**Components:** app. **Severity:** low.

A successful JSON protolet response should declare `Content-Type: application/json`. The [released WriteProtoToResponse implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/http/protolet/protolet.go) explicitly sets that media type. See also [RFC 9110 section 8.3](https://www.rfc-editor.org/rfc/rfc9110.html#section-8.3). Cookie-authenticated `POST /rpc/BuildBuddyService/GetUser` with Content-Type `application/json` and body `{}` returns HTTP 200 with valid user JSON but no Content-Type header. Both isolated reproductions fail on that header assertion. The same endpoint is used successfully by the harness to provision the user and API key, ruling out failed login, missing service configuration, or invalid JSON.

**Spec/source reference:** [WriteProtoToResponse lines 110–119](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/http/protolet/protolet.go#L110); [gzip middleware](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/http/interceptors/interceptors.go#L230).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/http-endpoints:protolet_content_type_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.260.0–2.314.0 (2.260.0, 2.280.0, 2.300.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [app-http-endpoints-protolet-content-type.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-http-endpoints-protolet-content-type.md) — confirmed.

#### Malformed digest aborts valid peers in CAS batch RPCs

**Components:** app. **Severity:** low.

BatchUpdateBlobs requests may succeed or fail independently, with digest mismatches reported as individual INVALID_ARGUMENT. BatchReadBlobs explicitly states that every individual read error is returned in the corresponding digest status. A malformed hash should produce an item error while the valid peer returns OK and its data. Both BatchUpdateBlobs and BatchReadBlobs return whole-RPC INVALID_ARGUMENT and no per-item responses when a 12-character malformed hash precedes a valid digest. The valid peer was successfully uploaded independently before both subtests. Both fresh explicit-SHA256 runs fail identically. Reference: [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto).

**Spec/source reference:** [released enterprise cache documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-cache-setup.md); [cache configuration documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-batch:malformed_digest_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** confirmed on 2.314.0; no historical defect matrix recorded.

**Evidence files:**

- [app-cas-batch-malformed-digest-aborts-batch.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-cas-batch-malformed-digest-aborts-batch.md) — confirmed.

#### Requested output mtime silently omitted

**Components:** executor. **Severity:** low.

Command.output_node_properties=[mtime] requests timestamps for output files and directories. Unsupported properties should be explicitly rejected; this test permits INVALID_ARGUMENT rather than requiring all servers to support mtime. The command creates file and tree/file and sets their mtimes and the tree directory mtime with touch -t 200001010000. The request succeeds but the top-level OutputFile has no NodeProperties or mtime. The test stops at this first missing property; it does not independently establish directory or nested-file metadata behavior. Reference: [REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

**Spec/source reference:** [Command.output_node_properties contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L886); [released capabilities server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go#L92).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:requested_mtime --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional duplicate reproducers (verification status is listed with each evidence file):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs-docker:requested_mtime --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [executor-outputs-requested-mtime.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-requested-mtime.md) — confirmed.
- [executor-outputs-docker-requested-mtime.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-outputs-docker-requested-mtime.md) — also reproduces under Docker isolation (author report; unverified duplicate).

#### Valid repeated-digest input tree fails at the filesystem hardlink limit

**Components:** executor. **Severity:** low.

A canonical tree with 256 directories, each containing 256 sorted file nodes referencing the same uploaded 15-byte blob, should materialize successfully. Internal hardlink deduplication should fall back to copying when the filesystem link limit is reached. The [REAPI Directory contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) does not forbid repeated file digests. The `65536-files-256-directories` subtest returns UNKNOWN (2) with `too many links` before running the shell command. It reproduces without the concurrent mutation test. The tree has unique sorted names and every referenced Directory and file blob was uploaded successfully; the existing valid-tree coverage passes using the same fixture/configuration.

**Spec/source reference:** [REAPI Directory canonicalization rules](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L978); [dirtools batch fetch](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L909).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/input-tree:input_tree_limits_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional duplicate reproducers (verification status is listed with each evidence file):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/inputs-docker:input_tree_limits_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:invalid_sequence_code --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:unknown_digest_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/bytestream:completed_write_status --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/config-validation:unknown_key_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/input-tree:file_directory_collision_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-docker:registry_credentials_cache_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Version history:** reproduces on sampled releases 2.258.0–2.314.0 (2.258.0, 2.275.0, 2.290.0, 2.313.0, 2.314.0); introduction unknown. See linked matrices for exact image digests and invocation links.

**Evidence files:**

- [executor-input-tree-hardlink-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-input-tree-hardlink-limit.md) — confirmed.
- [executor-inputs-docker-hardlink-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-inputs-docker-hardlink-limit.md) — also reproduces under Docker isolation (author report; unverified duplicate).

## Rejected reports

- [app-bes-invalid-sequence-code.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-bes-invalid-sequence-code.md) — BES does not mandate INVALID_ARGUMENT for these sequence failures; the asserted status-code oracle is unsupported.

- [app-capabilities-unknown-digest-accepted.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/app-capabilities-unknown-digest-accepted.md) — FindMissingBlobs does not mandate rejection of enum 999; a successful absence lookup does not establish storage support.

- [cacheproxy-bytestream-completed-write-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/cacheproxy-bytestream-completed-write-status.md) — The proxy correctly forwards the app’s intentionally UNIMPLEMENTED QueryWriteStatus; no distinct proxy defect is established. The confirmed app contract report above reaches a different verdict on the underlying omission.

- [crosscutting-config-validation-unknown-yaml-key.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/crosscutting-config-validation-unknown-yaml-key.md) — Unknown YAML keys are deliberately warned about and filtered; strict startup rejection is not promised.

- [executor-input-tree-name-collision-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-input-tree-name-collision-status.md) — REAPI permits skipping canonicalization checks and does not mandate INVALID_ARGUMENT for the malformed tree.

- [executor-isolation-docker-private-image-credentials-cache.md](https://github.com/altdansalt/buildbuddy-validation/blob/016e4693a43d45dde453a8c585c430150e83d517/findings/executor-isolation-docker-private-image-credentials-cache.md) — Same-group cached image authorization intentionally survives changed short-lived registry credentials until its token expires.

## Unverified

No standalone unverified defect remains after consolidation. The 13 unverified duplicate files are linked under their confirmed defect; they retain their original status and are not additional independently confirmed defects.

## How to run

The [harness](../harness/README.md) starts released app, executor and cache-proxy images inside each test, with fixture-local peers and disposable API keys. Tests run only on `bb remote` hosted runners; no component is built from source and no shared deployment receives component calls. Logs, generated configs and container image metadata are preserved in undeclared test outputs. The dispatch `BUILDBUDDY_API_KEY` belongs only to `bb remote`; never forward it to tests.

On exe.dev, configure public GitHub for runner checkout while retaining internal `origin` for pushes:

```sh
git remote get-url public >/dev/null 2>&1 || git remote add public https://github.com/altdansalt/buildbuddy-validation.git
git config buildbuddy.remote-bazel-remote-name public
GIT_CONFIG_GLOBAL=/dev/null bb remote test //... --config=nobug
```

`GIT_CONFIG_GLOBAL=/dev/null` bypasses the worker URL rewrite for this command only; do not change global Git settings. No checkout token is required. Use `GH_HOST=github.int.exe.xyz gh ...` for repository operations.

Failing defect reproducers carry the `bug` tag. The default suite and `--config=nobug` exclude them. Each confirmed command above places `--test_tag_filters=` after `--config=nobug` to clear the exclusion, and disables cached test results. Run a specific target: its assertion failure is expected until the defect is fixed. Alternatively use `--test_tag_filters=bug` for an explicit bug-only selection. Passing coverage targets have no `bug` tag. Optional `bb remote --runner_exec_properties=init-dockerd=true test ...` uses the runner’s prestarted Docker daemon; otherwise the fixture can start an isolated owned daemon.

For historical reproduction, add the exact `--test_arg=-harness.<component>_image=gcr.io/flame-public/<repository>@sha256:<digest>` overrides from the linked finding. Override every component specified by that matrix; do not infer historical image digests from tags.
