# Released BuildBuddy findings report

## Executive summary

As of 2026-10-13, 51 finding files from main and all 66 open PR branches consolidate into **35 confirmed defects** (1 high, 16 medium, 18 low), **2 rejected reports**, and **6 unverified/pending reports**. Duplicate reproducers are counted once. All defect evidence concerns the released, digest-pinned enterprise-v2.314.0 images; no finding records a reproduced fix in a later release.

The highest priorities are Docker's ignored `network=off` isolation setting (high), bare-runner input mutation poisoning subsequent actions (medium), descendants surviving an action's timeout plus termination grace period (medium), and incomplete parent output Trees for overlapping directory outputs (medium). These demonstrate an isolation-setting failure, incorrect future execution inputs, execution beyond the configured bound, and unusable output metadata respectively. No host escape, cross-organization compromise, or unrecovered service crash is established.

Coverage with no distinct defect reported includes app API-key group boundaries, randomized BES readback/replay, advertised-digest execution and disk-cache storage/GC; proxy authorization passthrough, read/write-through, digest functions, AC revalidation, outage recovery and model-based consistency; executor file-cache eviction/restarts, lifecycle, platform properties, scheduling, resource diagnostics and stdio/exit handling; and real Bazel version/flag/download-mode matrices, end-to-end proxy execution, seeded execution/tree properties, metrics, ten-minute soak and adjacent-release upgrades. These are bounded tested scenarios from area READMEs, not guarantees that the whole areas are defect-free.

Known gaps include Firecracker execution (hosted runners lack usable KVM), distributed-cache and multi-node database deployments, independent-user/SSO authorization boundaries, strict scheduling fairness and retry exhaustion, arbitrary multi-release upgrades/downgrades, host-power-loss durability, long-duration/capacity stress, and platform/isolation combinations beyond the documented Linux bare/Docker scenarios. No dedicated coverage directory was found for persistent workers, macOS/Windows execution, or web UI workflows.

| Affected component | High | Medium | Low | Confirmed total |
| --- | ---: | ---: | ---: | ---: |
| app | 0 | 9 | 14 | 23 |
| executor | 1 | 7 | 5 | 13 |
| cacheproxy | 0 | 4 | 4 | 8 |

Multi-component rows overlap; use the distinct-defect totals above for the overall count. Components identify affected released services, including proxy-forwarded behavior, rather than assigning separate root causes.

## Scope, reconciliation and reproduction

This is a documentation snapshot, not a new test run. Gathered every `findings/*.md` from `origin/main` and every open PR head after `git fetch origin '+refs/heads/*:refs/remotes/origin/*'`; README, TEMPLATE and generated INDEX are supporting documents, not findings. The main snapshot is `d1cece6a7439536fb2cd55bb001fded5a5dd1524`. Across differing copies of a file, use the latest recorded verification/revision, retaining independent-review verdicts rather than stale original statuses. Source links below are pinned to the gathered commits, so open-branch findings remain readable before merge. No source finding or test was changed by this report.

Duplicate entries combine explicit “Duplicate of” records and matching contracts, symptoms and source paths. Pending duplicate reproducers add coverage to an already confirmed defect; they do not become independently verified by association. The newer rejected QueryWriteStatus review supersedes the earlier app confirmation for this report; that disagreement is documented under rejected reports.

All reproduction commands use hosted runners and start released components inside the test. On exe.dev, configure public GitHub for **runner checkout** while retaining internal `origin` for pushes and `GH_HOST=github.int.exe.xyz gh ...`:

```sh
git remote get-url public >/dev/null 2>&1 || git remote add public https://github.com/altdansalt/buildbuddy-validation.git
git config buildbuddy.remote-bazel-remote-name public
```

Run each command below with `GIT_CONFIG_GLOBAL=/dev/null` as shown; this bypasses the worker's GitHub URL rewrite for that command only. Do not alter global Git settings. No tokens are required for checkout. The empty `--test_tag_filters=` **after** `--config=nobug` overrides bug exclusion, and `--nocache_test_results` forces execution. Assertion failures are expected until the defects are fixed.

The report branch does not import pending tests. Before reproducing, select the listed test PR branch/commit, or apply its area-only tests to current main in a disposable branch. Some older PR heads predate main's mirrored Redis-image correction; use current main's harness with those unchanged area tests, as the independent GetTree verification did. Never use an old checkout to pull Docker Hub images. For reports already on main, the separately listed test PR may still be required. Tests use bounded RPC contexts, fixture-local containers and disposable local API keys; never pass the dispatch `BUILDBUDDY_API_KEY` to tests or address a shared BuildBuddy deployment.

Image overrides and config prerequisites are in the linked findings. Exact tested image references are repeated per confirmed entry. The recorded verification history is against v2.314.0; repeated runs and severity/status revisions are review history, not evidence of cross-version regressions. Adjacent-release v2.313.0↔v2.314.0 compatibility is passing coverage, not a defect version history.

## Confirmed defects

### High

#### Docker isolation ignores network=off and retains the configured bridge

**Components:** executor. **Severity:** high.

The released [platform property contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go#L160) defines `network=off` as no network access and says it takes precedence over `dockerNetwork`. Docker actions should use network mode `none`, exposing only loopback and no bridge route, both with `dockerNetwork` absent and when the command also specifies `dockerNetwork=host`. Both cases complete with OK status and exit code 0, but `ls /sys/class/net` reports `eth0` and `lo`, and `/proc/net/route` includes a default route on `eth0`. The configured `action-network` bridge remains attached.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-docker:network_off_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8.

**Evidence and PRs:**

- [executor-isolation-docker-network-off-ignored.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-isolation-docker-network-off-ignored.md) — finding on main; reproducer [PR #31](https://github.com/altdansalt/buildbuddy-validation/pull/31) (`area/executor-isolation-docker`).

### Medium

#### App and cache proxy advertise unlimited batches but reject a 4,000,001-byte batch read

**Components:** app, cacheproxy. **Severity:** medium.

The [REAPI CacheCapabilities contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) defines `max_batch_total_size_bytes=0` as no limit set, apart from actual protocol message-size restrictions. BatchReadBlobs says a server's combined blob-size limit can be obtained through GetCapabilities. Both app and proxy return `max_batch_total_size_bytes=0`. Both accept a 4,000,001-byte blob in BatchUpdateBlobs, and ByteStream reads return the exact data. Both reject the tiny BatchReadBlobs request naming that same digest with `INVALID_ARGUMENT: BatchReadBlobs request exceeds server limit of 4000000 bytes`. The fixed application limit is not advertised and the 4,000,001-byte payload is below the standard 4 MiB message limit.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:batch_limit_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906.

**Evidence and PRs:**

- [app-capabilities-batch-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-capabilities-batch-read-limit.md) — finding on main; reproducer [PR #32](https://github.com/altdansalt/buildbuddy-validation/pull/32) (`area/app-capabilities`).

#### Bare action input mutation poisons subsequent action inputs

**Components:** executor. **Severity:** medium.

Each action's input tree should be materialized with the bytes named by its `InputRootDigest` and the contained `FileNode.digest`. See the [REAPI Action and FileNode definitions](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto). A later action naming the original input digest should read `original\n`, even if an earlier action modified its workspace copy. If input modification is unsupported, preventing it would also avoid corrupting later materialization. The first action runs `cat input; printf 'poisoned\n' > input` in its workspace. The second action has a distinct command/action digest, no runner recycling, no workspace preservation, and reads the same input digest. It succeeds but reads `poisoned\n` rather than `original\n`. An intervening authenticated read of that digest from the fixture-local app CAS returns the original bytes.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-firecracker:input_mutation_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [executor-isolation-firecracker-input-cache-poisoning.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-isolation-firecracker-input-cache-poisoning.md) — finding on main; reproducer [PR #20](https://github.com/altdansalt/buildbuddy-validation/pull/20) (`area/executor-isolation-firecracker`).

#### Bare executor ignores the action's PATH when resolving an unqualified executable

**Components:** executor. **Severity:** medium.

[REAPI Command.arguments](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) permits an unqualified executable resolved through the OS equivalent of PATH and states that an explicitly supplied PATH **SHOULD** be respected. This is a SHOULD recommendation, not a MUST requirement. Relative executables are resolved from the command working directory. The input executable `tools/envargs-tool`, working_directory=`tools` and PATH=`.` should yield `input-tool:path` when invoked as `envargs-tool path`; an explicit `./envargs-tool` control succeeds. The unqualified action returns `ExecuteResponse.status.code=5` (NOT_FOUND), exit code -2, and `exec: "envargs-tool": executable file not found in $PATH`.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/env-args:path_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8.

**Evidence and PRs:**

- [executor-env-args-command-path.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-env-args-command-path.md) — finding on main; reproducer [PR #30](https://github.com/altdansalt/buildbuddy-validation/pull/30) (`area/executor-env-args`).

#### ByteStream Read past EOF panics instead of returning OUT_OF_RANGE

**Components:** app, cacheproxy. **Severity:** medium.

The [ByteStream read_offset contract](https://github.com/googleapis/googleapis/blob/f5f34cc12374c1b3c11240e3d733b34ad0f63f49/google/bytestream/bytestream.proto#L99) requires OUT_OF_RANGE beyond the resource size. A read one byte beyond a small inline Pebble blob instead returns INTERNAL: A panic occurred, with a slice-bounds stack in filestore.InlineReader. The direct app and cold/warm proxy reproducers exercise the same unchecked-offset defect; the cold proxy forwards the error and the warm proxy can panic in its own inline reader. The panic is recovered, normal readback remains sound, and process termination or wider denial of service is not established.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:read_past_end_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional confirmed repro: `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/bytestream:read_beyond_eof --config=nobug --test_tag_filters= --nocache_test_results --test_output=all`.

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843 | enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906.

**Evidence and PRs:**

- [app-bytestream-read-past-end-panic.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-bytestream-read-past-end-panic.md) — finding on main; reproducer [PR #37](https://github.com/altdansalt/buildbuddy-validation/pull/37) (`area/app-bytestream`).
- [cacheproxy-bytestream-beyond-eof.md](https://github.com/altdansalt/buildbuddy-validation/blob/da9ba045c0c9c09d5a379b2e7b1bee8103fa6669/findings/cacheproxy-bytestream-beyond-eof.md) — finding revision from `origin/verify/cacheproxy-bytestream` ([finding PR #88](https://github.com/altdansalt/buildbuddy-validation/pull/88)); reproducer [PR #72](https://github.com/altdansalt/buildbuddy-validation/pull/72) (`area/cacheproxy-bytestream-2`).

#### Closing an unfinished ByteStream Write omits the response message

**Components:** app, cacheproxy. **Severity:** medium.

The [ByteStream Write contract](https://github.com/googleapis/googleapis/blob/f5f34cc12374c1b3c11240e3d733b34ad0f63f49/google/bytestream/bytestream.proto#L66) requires a WriteResponse when the client closes the request channel; finish_write controls finalization separately. Orderly half-close of a fresh partial upload instead produces an INTERNAL cardinality error because the server sends no response. Zero committed bytes would be acceptable: retaining partial progress is not assumed. The seeded app/proxy report observes the same behavior, with no context cancellation or broken network required.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:unfinished_response_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-bytestream-unfinished-write-response.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-bytestream-unfinished-write-response.md) — finding on main; reproducer [PR #37](https://github.com/altdansalt/buildbuddy-validation/pull/37) (`area/app-bytestream`).
- [crosscutting-rand-cas-unfinished-write-response.md](https://github.com/altdansalt/buildbuddy-validation/blob/340cbfe981fbcf8a67d1c0e1736a0105bd5af0b1/findings/crosscutting-rand-cas-unfinished-write-response.md) — finding revision from `origin/area/crosscutting-rand-cas-2` ([finding PR #99](https://github.com/altdansalt/buildbuddy-validation/pull/99)); reproducer [PR #99](https://github.com/altdansalt/buildbuddy-validation/pull/99) (`area/crosscutting-rand-cas-2`).

#### Execute reports the action digest as missing when only the command is absent

**Components:** app. **Severity:** medium.

The [REAPI Execution contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) requires FAILED_PRECONDITION for missing inputs/commands and recommends a PreconditionFailure with MISSING violations naming each absent blob. Once the server emits this detail, its subject must identify the absent command: `blobs/7ebbf16769265803a4ce811ff293c60b82f61e061dade999843cf9299dda5745/33`. Execute returns FAILED_PRECONDITION with a MISSING violation naming the uploaded Action: `blobs/2b69dd99eb501d936e61aeed1c8ef18f94d46051cc831a15c623b2c6b86fb0b1/138`. The human-readable error also incorrectly says this action is not found.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/execution-api:missing_command_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-execution-api-missing-command-digest.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-execution-api-missing-command-digest.md) — finding on main; reproducer [PR #8](https://github.com/altdansalt/buildbuddy-validation/pull/8) (`area/app-execution-api`).

#### Executor accepts legacy output type mismatches

**Components:** executor. **Severity:** medium.

[REAPI ActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) requires FAILED_PRECONDITION (9) when an output_files path is a directory, or a legacy output symlink points to the wrong type. The corresponding output_directories contract likewise requires directory outputs. These commands use only legacy fields, so modern type inference does not apply. All four cases return status OK (0), exit code 0: directory declared as file is returned in output_directories; file declared as directory is returned in output_files; a directory symlink declared as file is returned in output_file_symlinks; a file symlink declared as directory is returned in output_directory_symlinks.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:legacy_type_mismatch --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [executor-outputs-legacy-type-mismatch.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-outputs-legacy-type-mismatch.md) — finding on main; reproducer [PR #23](https://github.com/altdansalt/buildbuddy-validation/pull/23) (`area/executor-outputs`).

#### FetchBlob maps origin timeout, access denial, and checksum mismatch to NOT_FOUND

**Components:** app. **Severity:** medium.

The [pinned Remote Asset API](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto) explicitly requires inline `DEADLINE_EXCEEDED` when `FetchBlobRequest.timeout` expires retrieving origin content. Its response status documentation identifies `PERMISSION_DENIED` for rejection by the remote server and `ABORTED` for a failed consistency check. All three requests return gRPC OK with inline status 5 (`NOT_FOUND`). The messages retain evidence of deadline expiry, HTTP 403, or rejected digest validation, but the structured code loses those distinctions.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/remote-asset:origin_status_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-remote-asset-origin-error-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-remote-asset-origin-error-status.md) — finding on main; reproducer [PR #24](https://github.com/altdansalt/buildbuddy-validation/pull/24) (`area/app-remote-asset`).

#### GetActionResult skips availability checks for stdout_digest and stderr_digest

**Components:** app. **Severity:** medium.

[REAPI ActionCache.GetActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) says implementations SHOULD ensure any referenced CAS blobs are available when returning an ActionResult. This includes stdout_digest and stderr_digest. A missing referenced blob should cause a cache miss (`NOT_FOUND`), matching this release's handling of missing output files and Trees. The test uploads a valid Command, input root and Action, then updates the result with only a stdout_digest or stderr_digest whose nonempty CAS blob has never been uploaded. A direct ByteStream read verifies `NOT_FOUND` for that digest. GetActionResult nonetheless succeeds and returns the dangling digest in both subtests.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/action-cache:missing_streams_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-action-cache-missing-stream-blobs.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-action-cache-missing-stream-blobs.md) — finding on main; reproducer [PR #12](https://github.com/altdansalt/buildbuddy-validation/pull/12) (`area/app-action-cache`).

#### GetTree aborts partial trees and returns the wrong missing-root status

**Components:** app, cacheproxy. **Severity:** medium.

[REAPI GetTree](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) explicitly requires: "If part of the tree is missing from the CAS, the server will return the portion present and omit the rest." A stored root referencing one absent child must therefore return the root and omit that child. If the root itself is absent, the documented status is `NOT_FOUND`. Both app and proxy return `FailedPrecondition` for a missing child and emit no available root Directory. A missing root also returns `FailedPrecondition` instead of `NotFound`.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:bugs_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cache proxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906.

**Evidence and PRs:**

- [app-cas-gettree-missing-directories.md](https://github.com/altdansalt/buildbuddy-validation/blob/6dee03e6747a7961f2db56153360e6e89aa6e41a/findings/app-cas-gettree-missing-directories.md) — finding revision from `origin/verify/cas-gettree` ([finding PR #95](https://github.com/altdansalt/buildbuddy-validation/pull/95)); reproducer [PR #75](https://github.com/altdansalt/buildbuddy-validation/pull/75) (`area/app-cas-gettree`).

#### Graceful action timeout leaves a TERM-ignoring descendant running after the grace period

**Components:** executor. **Severity:** medium.

[REAPI Action.timeout](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) specifies a timeout after which execution should be killed. BuildBuddy additionally supports the [`termination-grace-period` platform property](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go): the executor sends graceful termination at the action timeout and sets a force-shutdown deadline at timeout plus grace period. A descendant that ignores TERM should be stopped by that final deadline, including when its parent handles TERM and exits zero. A one-second action with a two-second termination grace period starts a background child that ignores TERM, sleeps five seconds, and writes `survived` to a unique `/tmp` marker. The parent handles TERM by printing `terminated` and exiting zero. Child stdin/stdout/stderr are redirected away from the action's pipes. The parent returns DEADLINE_EXCEEDED with exit -2 and `startedterminated`; a later action waits six seconds and finds the unique child's `survived` marker. The descendant therefore ran beyond the three-second force-shutdown deadline, inside the bare executor container.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:graceful_timeout_child_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app: gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [executor-timeouts-cancel-graceful-orphan.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-timeouts-cancel-graceful-orphan.md) — finding on main; reproducer [PR #16](https://github.com/altdansalt/buildbuddy-validation/pull/16) (`area/executor-timeouts-cancel`).

#### Legacy fields suppress symlinks requested through output_paths

**Components:** executor. **Severity:** medium.

[REAPI Command.output_paths](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) says output_files and output_directories are ignored when output_paths is used. ActionResult.output_symlinks must contain each requested output path that exists and is a symlink. The command creates target containing data and link -> target, requests output_paths=[link], and also sets output_files=[ignored]. Execution succeeds but returns no output files, directories, or symlinks. The requested link is lost.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:output_paths_precedence --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [executor-outputs-output-paths-symlink-loss.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-outputs-output-paths-symlink-loss.md) — finding on main; reproducer [PR #23](https://github.com/altdansalt/buildbuddy-validation/pull/23) (`area/executor-outputs`).

#### Negative OutputFile digest size bypasses ActionResult validation

**Components:** app. **Severity:** medium.

[REAPI Digest](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) defines size_bytes as the size of the blob in bytes; a negative value cannot describe a blob. UpdateActionResult documents `INVALID_ARGUMENT` for invalid arguments. The app should reject an ActionResult containing a negative OutputFile digest size with `INVALID_ARGUMENT` and should not serve it as a successful cache hit. After uploading a valid Command, input root and Action, UpdateActionResult accepts an OutputFile with a valid SHA256 hash and size_bytes=-1. GetActionResult returns the same malformed file digest successfully. No output blob was uploaded or could have the stated size.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/action-cache:malformed_result_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-action-cache-negative-output-size.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-action-cache-negative-output-size.md) — finding on main; reproducer [PR #12](https://github.com/altdansalt/buildbuddy-validation/pull/12) (`area/app-action-cache`).

#### Overlapping directory outputs produce an incomplete parent Tree

**Components:** executor. **Severity:** medium.

[REAPI](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) allows an output directory to be a parent of another output directory. A Tree includes its root Directory and all descendant Directory messages in children. Each returned output directory's Tree must be independently complete. The command writes tree/nested/file containing inner, and requests sorted output_paths=[tree, tree/nested]. Execution succeeds and returns both directories. Decoding the tree output's CAS Tree gives zero children instead of the required nested Directory message. Its 82-byte Tree references the nested directory but omits its contents. The separately returned tree/nested Tree is 80 bytes.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:overlapping_directories --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [executor-outputs-overlapping-directory-tree.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-outputs-overlapping-directory-tree.md) — finding on main; reproducer [PR #60](https://github.com/altdansalt/buildbuddy-validation/pull/60) (`area/executor-outputs-2`).

#### Recycled bare runner retains task-scoped TMPDIR files across actions

**Components:** executor. **Severity:** medium.

The released [bare provider](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/bare/bare.go) describes `executor.bare.enable_tmpdir` as provisioning a temporary directory for each task and removing it when the task completes. A subsequent action should not find a previous action's file in the executor-provided TMPDIR. [REAPI Command](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) allows worker-provided default environment variables; this finding concerns the lifetime contract of that supplied directory, not a spec requirement to isolate arbitrary absolute paths. Action one writes `private-first-action-data` to `$TMPDIR/recycling-secret`. Action two, with the same recycling key and no inputs, reads that file and exits 42. Equal `pwd` values confirm actual runner reuse. Both ExecuteResponses have OK status; the test fails on the command exit code.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/runner-recycling:tmp_leak --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8.

**Evidence and PRs:**

- [executor-runner-recycling-tmpdir-leak.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-runner-recycling-tmpdir-leak.md) — finding on main; reproducer [PR #28](https://github.com/altdansalt/buildbuddy-validation/pull/28) (`area/executor-runner-recycling`).

#### v1 GetLog starts at the last log chunk instead of the first

**Components:** app. **Severity:** medium.

The public [GetLog request and response](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/api/v1/log.proto) describe `page_token` as the `next_page_token` returned by a previous request, and `next_page_token` as empty when there are no more logs. An initial request with no previous token, followed by every returned token, should expose the complete stored build log in order. Upload 2,250,021 bytes in small BES Progress events, starting with `FIRST-LINE` and ending with `LAST-LINE`. After the server acknowledges the entire stream and completes it, call `/api/v1/GetLog` with an invocation selector and empty page token. Following every returned token yields only 270,217 bytes: the final nonempty chunk followed by an empty response. The first line and approximately 1.98 MB of earlier output are missing.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/invocations-api:log_pagination_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-invocations-api-log-pagination.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-invocations-api-log-pagination.md) — finding on main; reproducer [PR #25](https://github.com/altdansalt/buildbuddy-validation/pull/25) (`area/app-invocations-api`).

### Low

#### Artifact download reports missing invocation as HTTP 500

**Components:** app. **Severity:** low.

A nonexistent invocation should return HTTP 404, not a server error. The [released serveArtifact handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/buildbuddy_server/buildbuddy_server.go) explicitly has a NotFound-to-404 branch for invocation lookup. See [RFC 9110 section 15.5.5](https://www.rfc-editor.org/rfc/rfc9110.html#section-15.5.5). Cookie-authenticated `GET /file/download?artifact=build_log&invocation_id=00000000-0000-0000-0000-000000000000` returns HTTP 500 and `rpc error: code = Internal desc = Internal server error`. Both isolated reproductions fail on the expected 404 assertion.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/http-endpoints:missing_invocation_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-http-endpoints-missing-invocation-500.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-http-endpoints-missing-invocation-500.md) — finding on main; reproducer [PR #54](https://github.com/altdansalt/buildbuddy-validation/pull/54) (`area/app-http-endpoints`).

#### BES accepts a changed stream identity and ACKs the wrong invocation

**Components:** app. **Severity:** low.

[PublishBuildToolEventStream](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto#L72-L75) publishes events belonging to the same stream. Each OrderedBuildEvent identifies its stream, and each ACK identifies the stream containing the committed event. Reject a change of stream identity within an RPC. In particular, do not ACK an event for invocation B as an event for invocation A. One RPC sends BuildStarted for invocation A at sequence 1, then BuildFinished for a distinct invocation B at sequence 2. Both invocation_id and build_id change. The server completes successfully and emits ACKs for sequences 1 and 2, both with A's StreamId. No event for A at sequence 2 was sent.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:changed_identity --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-bes-changed-stream-id.md](https://github.com/altdansalt/buildbuddy-validation/blob/5e61ba80376f446685e9b365e902d1670ca26de6/findings/app-bes-changed-stream-id.md) — finding on main; reproducer [PR #22](https://github.com/altdansalt/buildbuddy-validation/pull/22) (`area/app-bes`).

**Review history:** medium → low on independent review; malformed-client ACK integrity is established, ordinary-client or cross-organization effects are not.

#### BatchUpdateBlobs accepts nonempty data for the empty digest

**Components:** app. **Severity:** low.

BatchUpdateBlobs must report per-item INVALID_ARGUMENT when the digest does not match the supplied data. The SHA-256 empty digest with size zero does not describe the supplied bytes `not empty`. A valid peer should still succeed. The RPC succeeds and returns per-item OK for the empty digest despite nonempty data. The test submits this alongside a valid upload. Both fresh runs return expected code 3 versus actual code 0. Reference: [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-batch:empty_digest_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-cas-batch-empty-digest-validation.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-cas-batch-empty-digest-validation.md) — finding on main; reproducer [PR #11](https://github.com/altdansalt/buildbuddy-validation/pull/11) (`area/app-cas-batch`).

#### BatchUpdateBlobs reports INTERNAL for malformed client zstd data

**Components:** app. **Severity:** low.

Malformed compressed client data should return per-entry INVALID_ARGUMENT, while valid sibling requests succeed. The [REAPI CAS contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) describes BatchUpdateBlobs as equivalent to ByteStream uploads and specifies INVALID_ARGUMENT when the digest does not match the provided data. Its compressed-upload contract requires validating the uncompressed hash and size. A string that is not a zstd frame cannot represent the declared uncompressed blob. [gRPC status guidance](https://grpc.io/docs/guides/status-codes/) classifies invalid client arguments as INVALID_ARGUMENT and reserves INTERNAL for broken server invariants. Uploading `not a zstd frame` with compressor ZSTD returns per-entry code 13 (INTERNAL), with `Failed to decompress zstd-compressed blob: invalid input: magic number mismatch`. The overall batch RPC succeeds. The valid identity-compressed sibling is stored and readable; FindMissingBlobs confirms the corrupt entry is absent.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/compression:corrupt_batch_zstd_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-compression-corrupt-batch-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-compression-corrupt-batch-status.md) — finding on main; reproducer [PR #18](https://github.com/altdansalt/buildbuddy-validation/pull/18) (`area/app-compression`).
- [app-cas-batch-malformed-zstd-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/005e2a54850199800ccfce663cdbde8a355542ba/findings/app-cas-batch-malformed-zstd-status.md) — finding revision from `origin/area/app-cas-batch-3` ([finding PR #97](https://github.com/altdansalt/buildbuddy-validation/pull/97)); reproducer [PR #70](https://github.com/altdansalt/buildbuddy-validation/pull/70) (`area/app-cas-batch-2`).

#### CAS tree traversal reports an uploaded metadata-only directory as missing

**Components:** app, executor. **Severity:** low.

An empty child Directory carrying NodeProperties (mtime and unix_mode) is valid under the [REAPI Directory contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto). Optional properties may be ignored, but their presence should not cause existing CAS content to be reported missing. The `optional-node-properties` subtest uploads an empty child Directory with metadata, then Execute returns FAILED_PRECONDITION (9), wrapping a GetTree NotFound error. The second reproduction explicitly reads the child blob back from fixture CAS and checks its nonzero length before Execute; these checks pass. Thus this is not an absent upload or authentication failure. The matching nonempty-directory metadata control is in the untagged suite, and ordinary empty directories are covered there as well.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/input-tree:input_tree_limits_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-input-tree-metadata-only-directory.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-input-tree-metadata-only-directory.md) — finding on main; reproducer [PR #29](https://github.com/altdansalt/buildbuddy-validation/pull/29) (`area/executor-input-tree-2`).

#### Cache proxy reports exec_enabled=true but Execute returns unknown service

**Components:** app, cacheproxy. **Severity:** low.

The [REAPI ExecutionCapabilities contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) defines `exec_enabled` as whether remote execution is enabled for the particular server/instance. An endpoint without an Execution service should omit execution capabilities or report `exec_enabled=false`, rather than forward the upstream app's execution advertisement unchanged. The proxy returns the upstream app's `execution_capabilities` with `exec_enabled=true`, SHA256 and additional digest functions, and priority range -1000..1000. An Execute request to that proxy returns `UNIMPLEMENTED` with `unknown service build.bazel.remote.execution.v2.Execution`.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:proxy_execution_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906.

**Evidence and PRs:**

- [app-capabilities-proxy-execution.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-capabilities-proxy-execution.md) — finding on main; reproducer [PR #32](https://github.com/altdansalt/buildbuddy-validation/pull/32) (`area/app-capabilities`).

#### Checksum cache hits bypass oldest_content_accepted

**Components:** app. **Severity:** low.

The [pinned Remote Asset API](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto) permits reuse of cached fetched content subject to `oldest_content_accepted`. That field is the oldest accepted time measured when content was pushed or its underlying origin retrieval started. The repro fetches a SHA256-qualified asset from a test-owned origin, waits until after completion and sets oldest_content_accepted to a new current timestamp. A second origin retrieval should satisfy that fresh lower bound. The initial fetch returns OK and produces exactly one origin request. The second fetch also returns OK, but there is still exactly one origin request: the older checksum cache entry was returned without consulting the origin.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/remote-asset:freshness_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-remote-asset-oldest-content-accepted.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-remote-asset-oldest-content-accepted.md) — finding on main; reproducer [PR #24](https://github.com/altdansalt/buildbuddy-validation/pull/24) (`area/app-remote-asset`).

#### Compressed ByteStream Read accepts nonzero read_limit instead of INVALID_ARGUMENT

**Components:** app, cacheproxy. **Severity:** low.

[REAPI compressed download rules](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L313-L317) require INVALID_ARGUMENT for every nonzero compressed Read read_limit. App and proxy instead return OK and compressed partial content. Unlimited controls return correct full bytes. This is invalid-request conformance and interoperability impact, with no demonstrated persistent corruption; app, proxy, large-blob and seeded-property reports describe the same defect.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/compression:compressed_read_limit_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Additional confirmed repro: `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/compression:compressed_read_limit_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all`.

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843 | enterprise-v2.314.0; proxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-compression-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-compression-read-limit.md) — finding on main; reproducer [PR #18](https://github.com/altdansalt/buildbuddy-validation/pull/18) (`area/app-compression`).
- [cacheproxy-compression-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/cacheproxy-compression-read-limit.md) — finding on main; reproducer [PR #10](https://github.com/altdansalt/buildbuddy-validation/pull/10) (`area/cacheproxy-compression`).
- [app-bytestream-compressed-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/ed2995a785c243f6107aa4a08070424b5f67fb83/findings/app-bytestream-compressed-read-limit.md) — finding revision from `origin/area/app-bytestream-3` ([finding PR #91](https://github.com/altdansalt/buildbuddy-validation/pull/91)); reproducer [PR #66](https://github.com/altdansalt/buildbuddy-validation/pull/66) (`area/app-bytestream-2`).
- [cacheproxy-large-blobs-compressed-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1fa07d70a537fad1a06c3161e560544850dba18/findings/cacheproxy-large-blobs-compressed-read-limit.md) — finding revision from `origin/area/cacheproxy-large-blobs` ([finding PR #65](https://github.com/altdansalt/buildbuddy-validation/pull/65)); reproducer [PR #65](https://github.com/altdansalt/buildbuddy-validation/pull/65) (`area/cacheproxy-large-blobs`).
- [crosscutting-rand-cas-compressed-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/340cbfe981fbcf8a67d1c0e1736a0105bd5af0b1/findings/crosscutting-rand-cas-compressed-read-limit.md) — finding revision from `origin/area/crosscutting-rand-cas-2` ([finding PR #99](https://github.com/altdansalt/buildbuddy-validation/pull/99)); reproducer [PR #62](https://github.com/altdansalt/buildbuddy-validation/pull/62) (`area/crosscutting-rand-cas`).

#### Explicit nested output file omitted from OutputFiles

**Components:** executor. **Severity:** low.

Command.output_paths says a resulting file is returned in an OutputFile field. A separately requested directory Tree does not remove that explicitly listed file path. The command writes tree/file and tree/nested/file and requests sorted output_paths=[tree, tree/nested, tree/nested/file]. Execution succeeds with two OutputDirectories and zero OutputFiles; the explicitly requested tree/nested/file is omitted. Reference: [REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:overlapping_file --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [executor-outputs-overlapping-file.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-outputs-overlapping-file.md) — finding on main; reproducer [PR #60](https://github.com/altdansalt/buildbuddy-validation/pull/60) (`area/executor-outputs-2`).

#### Fresh BES streams starting above one are ACKed without persistence

**Components:** app. **Severity:** low.

The released [OrderedBuildEvent contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto#L150-L159) says sequence numbers are consecutive natural numbers starting at one. The response identifies an event that has been committed. A brand-new UUID with no earlier events cannot be a suffix retry of an already committed invocation. Reject the invalid initial sequence without ACKing it, or actually retain it with an explicit supported recovery policy. Sending one valid Bazel BuildStarted payload at sequence 2 for a fresh UUID returns an ACK for sequence 2 and successful stream completion. The second reproduction also queries GetInvocation immediately after the ACK: the app returns NotFound.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:fresh_offset --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-bes-fresh-offset.md](https://github.com/altdansalt/buildbuddy-validation/blob/5e61ba80376f446685e9b365e902d1670ca26de6/findings/app-bes-fresh-offset.md) — finding on main; reproducer [PR #22](https://github.com/altdansalt/buildbuddy-validation/pull/22) (`area/app-bes`).

**Review history:** medium → low on independent review; malformed-client ACK integrity is established, ordinary-client or cross-organization effects are not.

#### GetCapabilities accepts invalid UTF-8 in instance_name

**Components:** app. **Severity:** low.

Proto3 `string` fields must contain UTF-8 or ASCII text, according to the [protobuf scalar type contract](https://protobuf.dev/programming-guides/proto3/#scalar). The released [Remote Execution API proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/remote_execution.proto) declares `GetCapabilitiesRequest.instance_name` as proto3 `string`, field 1; the [upstream REAPI](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) agrees. The request payload `0a 01 ff` is therefore malformed. The [gRPC status mapping](https://github.com/grpc/grpc/blob/master/doc/statuscodes.md) specifies INTERNAL for an error parsing the request proto. The app returns gRPC OK with populated cache capabilities for `0a 01 ff`. The reproducer sends the bytes with a raw client codec named `proto`, bypassing client-side validation while retaining the ordinary gRPC protobuf content type. The standard Go protobuf decoder rejects these exact bytes in a local control inside the test. A valid `0a 01 61` request succeeds against the same connection, method, credentials, and raw codec before the malformed request is sent.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/grpc-edges:invalid_utf8_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-grpc-edges-invalid-utf8.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-grpc-edges-invalid-utf8.md) — finding on main; reproducer [PR #13](https://github.com/altdansalt/buildbuddy-validation/pull/13) (`area/app-grpc-edges`).

#### GetTree ignores page_size and returns an oversized page

**Components:** app, cacheproxy. **Severity:** low.

GetTreeRequest.page_size is a maximum number of items per page. A two-directory tree requested with page_size=1 must not place both directories in one GetTreeResponse. Traversal order may vary for fresh calls; issued tokens must resume the corresponding suffix. See the [REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto). Both app and proxy return the complete two-directory tree in a single response despite page_size=1. Its next_page_token is empty. Both hosted runs show the same failure. Content is otherwise correct, ruling out malformed input. Reproduction links: [run 1](https://app.buildbuddy.io/invocation/ea7980d9-4bad-42f1-9db4-5d3463f2e3ac) and [run 2](https://app.buildbuddy.io/invocation/41a50bf1-c29b-4d87-905c-f17abc3c7f3d). The linked finding records independent verification.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:bugs_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cache proxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906.

**Evidence and PRs:**

- [app-cas-gettree-page-size.md](https://github.com/altdansalt/buildbuddy-validation/blob/6dee03e6747a7961f2db56153360e6e89aa6e41a/findings/app-cas-gettree-page-size.md) — finding revision from `origin/verify/cas-gettree` ([finding PR #95](https://github.com/altdansalt/buildbuddy-validation/pull/95)); reproducer [PR #75](https://github.com/altdansalt/buildbuddy-validation/pull/75) (`area/app-cas-gettree`).

#### GetTree omits empty root and child Directory messages

**Components:** app, cacheproxy. **Severity:** low.

GetTree must return the Directory messages reachable from its root, including an empty root or child. An empty Directory has a valid zero-byte protobuf encoding and the SHA256 empty-blob digest. Uploading that encoding succeeds. See the [REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto). Both app and proxy return no Directory for an empty root (expected one), and only the parent for a parent containing one empty child (expected two). Both cases reproduce in independent hosted runs. Reproduction links: [run 1](https://app.buildbuddy.io/invocation/ea7980d9-4bad-42f1-9db4-5d3463f2e3ac) and [run 2](https://app.buildbuddy.io/invocation/41a50bf1-c29b-4d87-905c-f17abc3c7f3d). The linked finding records independent verification.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:bugs_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cache proxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906.

**Evidence and PRs:**

- [app-cas-gettree-empty-directories.md](https://github.com/altdansalt/buildbuddy-validation/blob/6dee03e6747a7961f2db56153360e6e89aa6e41a/findings/app-cas-gettree-empty-directories.md) — finding revision from `origin/verify/cas-gettree` ([finding PR #95](https://github.com/altdansalt/buildbuddy-validation/pull/95)); reproducer [PR #75](https://github.com/altdansalt/buildbuddy-validation/pull/75) (`area/app-cas-gettree`).

#### Ignored legacy output directories alter modern action execution

**Components:** executor. **Severity:** low.

[REAPI Command.output_paths](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) requires legacy output_files/output_directories to be ignored when modern output_paths is present. With output_paths=[out] and output_directories=[ignored], the executor still precreates ignored; the command observes it and produces unexpected rather than expected output. The legacy-only precreation probe is an intentional compatibility workaround and is not the confirmed defect. The actionable mixed-field reproducer is output_paths_precedence.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:output_paths_precedence --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [executor-outputs-legacy-directory-precreation.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-outputs-legacy-directory-precreation.md) — finding on main; reproducer [PR #74](https://github.com/altdansalt/buildbuddy-validation/pull/74) (`area/executor-outputs-3`).

**Review history:** the original broad legacy-only interpretation was narrowed to modern/legacy field precedence; the standalone legacy workaround was rejected as intentional. PR #74 contains the narrow test.

#### JSON HTTP RPC responses omit Content-Type

**Components:** app. **Severity:** low.

A successful JSON protolet response should declare `Content-Type: application/json`. The [released WriteProtoToResponse implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/http/protolet/protolet.go) explicitly sets that media type. See also [RFC 9110 section 8.3](https://www.rfc-editor.org/rfc/rfc9110.html#section-8.3). Cookie-authenticated `POST /rpc/BuildBuddyService/GetUser` with Content-Type `application/json` and body `{}` returns HTTP 200 with valid user JSON but no Content-Type header. Both isolated reproductions fail on that header assertion. The same endpoint is used successfully by the harness to provision the user and API key, ruling out failed login, missing service configuration, or invalid JSON.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/http-endpoints:protolet_content_type_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-http-endpoints-protolet-content-type.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-http-endpoints-protolet-content-type.md) — finding on main; reproducer [PR #54](https://github.com/altdansalt/buildbuddy-validation/pull/54) (`area/app-http-endpoints`).

#### Malformed digest aborts valid peers in CAS batch RPCs

**Components:** app. **Severity:** low.

BatchUpdateBlobs requests may succeed or fail independently, with digest mismatches reported as individual INVALID_ARGUMENT. BatchReadBlobs explicitly states that every individual read error is returned in the corresponding digest status. A malformed hash should produce an item error while the valid peer returns OK and its data. Both BatchUpdateBlobs and BatchReadBlobs return whole-RPC INVALID_ARGUMENT and no per-item responses when a 12-character malformed hash precedes a valid digest. The valid peer was successfully uploaded independently before both subtests. Both fresh explicit-SHA256 runs fail identically. Reference: [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-batch:malformed_digest_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [app-cas-batch-malformed-digest-aborts-batch.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-cas-batch-malformed-digest-aborts-batch.md) — finding on main; reproducer [PR #11](https://github.com/altdansalt/buildbuddy-validation/pull/11) (`area/app-cas-batch`).

#### Requested output mtime silently omitted

**Components:** executor. **Severity:** low.

Command.output_node_properties=[mtime] requests timestamps for output files and directories. Unsupported properties should be explicitly rejected; this test permits INVALID_ARGUMENT rather than requiring all servers to support mtime. The command creates file and tree/file and sets their mtimes and the tree directory mtime with touch -t 200001010000. The request succeeds but the top-level OutputFile has no NodeProperties or mtime. The test stops at this first missing property; it does not independently establish directory or nested-file metadata behavior. Reference: [REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:requested_mtime --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [executor-outputs-requested-mtime.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-outputs-requested-mtime.md) — finding on main; reproducer [PR #60](https://github.com/altdansalt/buildbuddy-validation/pull/60) (`area/executor-outputs-2`).

#### Valid repeated-digest input tree fails at the filesystem hardlink limit

**Components:** executor. **Severity:** low.

A canonical tree with 256 directories, each containing 256 sorted file nodes referencing the same uploaded 15-byte blob, should materialize successfully. Internal hardlink deduplication should fall back to copying when the filesystem link limit is reached. The [REAPI Directory contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) does not forbid repeated file digests. The `65536-files-256-directories` subtest returns UNKNOWN (2) with `too many links` before running the shell command. It reproduces without the concurrent mutation test. The tree has unique sorted names and every referenced Directory and file blob was uploaded successfully; the existing valid-tree coverage passes using the same fixture/configuration.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/input-tree:input_tree_limits_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

**Tested version:** enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843.

**Evidence and PRs:**

- [executor-input-tree-hardlink-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-input-tree-hardlink-limit.md) — finding on main; reproducer [PR #29](https://github.com/altdansalt/buildbuddy-validation/pull/29) (`area/executor-input-tree-2`).

## Rejected reports

- **Completed ByteStream upload status is unimplemented through the cache proxy** — QueryWriteStatus is deliberately unsupported in this release; the proxy faithfully forwards UNIMPLEMENTED and supported upload recovery restarts at zero. Files: [cacheproxy-bytestream-completed-write-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/da9ba045c0c9c09d5a379b2e7b1bee8103fa6669/findings/cacheproxy-bytestream-completed-write-status.md), [app-bytestream-query-write-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-bytestream-query-write-status.md).
- **Malformed input tree with a file/directory name collision returns UNKNOWN instead of INVALID_ARGUMENT** — Unique child-name canonicalization is a client obligation that REAPI permits servers not to validate; UNKNOWN for the malformed collision does not establish a mandatory INVALID_ARGUMENT violation. Files: [executor-input-tree-name-collision-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/executor-input-tree-name-collision-status.md).

The QueryWriteStatus records disagree: the earlier main app report calls unknown/finalized queries confirmed (medium), while the newer 2026-10-13 independent proxy review rejects the same unconditional implementation behavior and downgrades its diagnostic concern to low. The newer review cites the [pinned intentional UNIMPLEMENTED implementation](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/server/remote_cache/byte_stream_server/byte_stream_server.go#L867-L876) and Bazel's restart fallback. This report counts the merged claim as rejected, including the unknown-resource variant; it does not claim a later release fixed it. Maintainers should reconcile the older finding status and retire/reframe the bug assertions in PRs [#37](https://github.com/altdansalt/buildbuddy-validation/pull/37) and [#72](https://github.com/altdansalt/buildbuddy-validation/pull/72). The collision report likewise requests follow-up to reframe its existing test.

## Unverified / pending

These remain pending even when their authors recorded repeated observations. Independent verification and, for some, a supported-contract decision are still required. Duplicate pending reproducers are listed under the merged confirmed entries instead of inflating this list.

### SpliceBlob succeeds but ByteStream cannot read the resulting small blob

**Components:** app. **Proposed severity:** medium. **Status:** unverified.

Upload `abc` and `def`, then splice them into the digest of `abcdef`. Splice succeeds, and SplitBlob returns the two correct chunk digests, but ByteStream Read for the six-byte result returns NOT_FOUND. Repeated uncached observations and passing larger-blob controls are recorded in the finding; independent verification is pending.

- [app-capabilities-small-splice-unreadable.md](https://github.com/altdansalt/buildbuddy-validation/blob/74d691a146f3b836dd61296ec2ec1844654042bb/findings/app-capabilities-small-splice-unreadable.md) — finding revision from `origin/area/app-capabilities-2` ([finding PR #82](https://github.com/altdansalt/buildbuddy-validation/pull/82)); reproducer [PR #82](https://github.com/altdansalt/buildbuddy-validation/pull/82) (`area/app-capabilities-2`).

### Valid unknown WaitExecution operation names wait until the client deadline

**Components:** app. **Proposed severity:** medium. **Status:** unverified.

Two independent fresh fixtures returned no operation and no server error for a valid but unknown operation name during a five-second bounded wait. The client eventually saw DeadlineExceeded; the proposed expectation is prompt NOT_FOUND, pending independent contract review.

- [app-execution-api-unknown-operation-hangs.md](https://github.com/altdansalt/buildbuddy-validation/blob/dc1a618f8b75e2e63dc9e18540c249e34df21d25/findings/app-execution-api-unknown-operation-hangs.md) — finding revision from `origin/area/app-execution-api-2` ([finding PR #89](https://github.com/altdansalt/buildbuddy-validation/pull/89)); reproducer [PR #89](https://github.com/altdansalt/buildbuddy-validation/pull/89) (`area/app-execution-api-2`).

### App, executor and cache proxy start successfully after ignoring a misspelled YAML setting

**Components:** app, executor, cacheproxy. **Proposed severity:** low. **Status:** unverified.

All three released components warn about the unknown key, ignore it, and return HTTP 200 from fixture-local `/readyz?server-type=<component>`. App and cache proxy logs explicitly show monitoring starting on the default port 9090 instead of 9191. The implementation is not silent in logs, but successful startup does not enforce the requested rejection invariant.

- [crosscutting-config-validation-unknown-yaml-key.md](https://github.com/altdansalt/buildbuddy-validation/blob/342fffbb14afc840b4c860d192f9609730a43064/findings/crosscutting-config-validation-unknown-yaml-key.md) — finding revision from `origin/area/crosscutting-config-validation` ([finding PR #94](https://github.com/altdansalt/buildbuddy-validation/pull/94)); reproducer [PR #94](https://github.com/altdansalt/buildbuddy-validation/pull/94) (`area/crosscutting-config-validation`).

### Docker actions accept termination-grace-period but cannot deliver the graceful signal

**Components:** executor. **Proposed severity:** low. **Status:** unverified.

Two separate runs returned DeadlineExceeded with exit -2 and stdout `started`, missing `terminated`. A self-TERM control succeeds, while timeout logs show Unimplemented for graceful signaling. Whether the generic termination-grace-period property promises Docker support remains a contract question.

- [executor-timeouts-cancel-docker-grace-unimplemented.md](https://github.com/altdansalt/buildbuddy-validation/blob/44e3fdf6b573cf9e8ad07b24e257308ce5caa579/findings/executor-timeouts-cancel-docker-grace-unimplemented.md) — finding revision from `origin/area/executor-timeouts-cancel-2` ([finding PR #92](https://github.com/altdansalt/buildbuddy-validation/pull/92)); reproducer [PR #92](https://github.com/altdansalt/buildbuddy-validation/pull/92) (`area/executor-timeouts-cancel-2`).

### FindMissingBlobs succeeds for unadvertised, unknown digest function 999

**Components:** app. **Proposed severity:** low. **Status:** unverified.

FindMissingBlobs returns OK and the requested digest in missing_blob_digests for algorithm 999. Reproduced independently in the invocation above and https://app.buildbuddy.io/invocation/58aa48b5-61c1-4a63-8832-6bf4c9668fb9. Both used --nocache_test_results. A valid SHA256 lookup of the same digest returns the expected missing result; the local app is healthy and authenticated. The same observation occurred for several known but unadvertised algorithms in exploration, but the minimal repro only uses an unequivocally unknown enum.

- [app-capabilities-unknown-digest-accepted.md](https://github.com/altdansalt/buildbuddy-validation/blob/74d691a146f3b836dd61296ec2ec1844654042bb/findings/app-capabilities-unknown-digest-accepted.md) — finding revision from `origin/area/app-capabilities-2` ([finding PR #82](https://github.com/altdansalt/buildbuddy-validation/pull/82)); reproducer [PR #82](https://github.com/altdansalt/buildbuddy-validation/pull/82) (`area/app-capabilities-2`).

### Invalid BES sequences return Unknown instead of InvalidArgument

**Components:** app. **Proposed severity:** low. **Status:** unverified.

A stream with sequences `[1, 3]` or `[1, 1]` returns `Unknown` with an explicit sequence-mismatch description and no ACKs. This reproduced in [run 1](https://app.buildbuddy.io/invocation/93ffaa2e-bf0a-4b57-89d2-cca06d34c301) and [run 2](https://app.buildbuddy.io/invocation/099da032-0bf8-44de-85d8-4134eda6c41f), each in an independent fixture-local app container with uncached tests. Both fail only the intended status-code assertions.

- [app-bes-invalid-sequence-code.md](https://github.com/altdansalt/buildbuddy-validation/blob/5e61ba80376f446685e9b365e902d1670ca26de6/findings/app-bes-invalid-sequence-code.md) — finding revision from `origin/area/app-bes-2` ([finding PR #87](https://github.com/altdansalt/buildbuddy-validation/pull/87)); reproducer [PR #87](https://github.com/altdansalt/buildbuddy-validation/pull/87) (`area/app-bes-2`).

## Duplicate consolidation

| Merged entry | All finding files | Basis |
| --- | --- | --- |
| Compressed ByteStream Read accepts nonzero read_limit instead of INVALID_ARGUMENT | [app-compression-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-compression-read-limit.md)<br>[cacheproxy-compression-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/cacheproxy-compression-read-limit.md)<br>[app-bytestream-compressed-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/ed2995a785c243f6107aa4a08070424b5f67fb83/findings/app-bytestream-compressed-read-limit.md)<br>[cacheproxy-large-blobs-compressed-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1fa07d70a537fad1a06c3161e560544850dba18/findings/cacheproxy-large-blobs-compressed-read-limit.md)<br>[crosscutting-rand-cas-compressed-read-limit.md](https://github.com/altdansalt/buildbuddy-validation/blob/340cbfe981fbcf8a67d1c0e1736a0105bd5af0b1/findings/crosscutting-rand-cas-compressed-read-limit.md) | Explicit duplicate labels plus matching protocol/source behavior. |
| ByteStream Read past EOF panics instead of returning OUT_OF_RANGE | [app-bytestream-read-past-end-panic.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-bytestream-read-past-end-panic.md)<br>[cacheproxy-bytestream-beyond-eof.md](https://github.com/altdansalt/buildbuddy-validation/blob/da9ba045c0c9c09d5a379b2e7b1bee8103fa6669/findings/cacheproxy-bytestream-beyond-eof.md) | Matching contract, symptom and implementation; adds endpoint/property coverage rather than a separate defect. |
| Closing an unfinished ByteStream Write omits the response message | [app-bytestream-unfinished-write-response.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-bytestream-unfinished-write-response.md)<br>[crosscutting-rand-cas-unfinished-write-response.md](https://github.com/altdansalt/buildbuddy-validation/blob/340cbfe981fbcf8a67d1c0e1736a0105bd5af0b1/findings/crosscutting-rand-cas-unfinished-write-response.md) | Matching contract, symptom and implementation; adds endpoint/property coverage rather than a separate defect. |
| BatchUpdateBlobs reports INTERNAL for malformed client zstd data | [app-compression-corrupt-batch-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-compression-corrupt-batch-status.md)<br>[app-cas-batch-malformed-zstd-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/005e2a54850199800ccfce663cdbde8a355542ba/findings/app-cas-batch-malformed-zstd-status.md) | Explicit duplicate labels plus matching protocol/source behavior. |
| Completed ByteStream upload status is unimplemented through the cache proxy | [cacheproxy-bytestream-completed-write-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/da9ba045c0c9c09d5a379b2e7b1bee8103fa6669/findings/cacheproxy-bytestream-completed-write-status.md)<br>[app-bytestream-query-write-status.md](https://github.com/altdansalt/buildbuddy-validation/blob/d1cece6a7439536fb2cd55bb001fded5a5dd1524/findings/app-bytestream-query-write-status.md) | Matching contract, symptom and implementation; adds endpoint/property coverage rather than a separate defect. |

The metadata-only nonzero Directory traversal defect is separate from GetTree's omitted zero-byte empty Directory messages. Overlapping-file omission and incomplete parent Trees are separate returned-result defects even though their reproducers share an output traversal path. Legacy type mismatch, mixed-field directory precreation and mixed-field symlink loss violate different observable contracts and remain separate entries.

## Coverage and limits

The directories and READMEs gathered from main and open PRs describe the following areas without a separate active finding. Some coverage is still in open PRs. “No defect reported” summarizes the bounded scenarios in this snapshot, not a fresh full-suite pass or exhaustive verification. Areas with pending claims are not presented as clean merely because those claims are unconfirmed.

- **app:** `tests/app/auth-groups/`, `tests/app/digest-function-execution/`, `tests/app/rand-bes/`, `tests/app/storage-gc/`.
- **cacheproxy:** `tests/cacheproxy/action-cache/`, `tests/cacheproxy/auth-passthrough/`, `tests/cacheproxy/digest-functions/`, `tests/cacheproxy/failure-modes/`, `tests/cacheproxy/model-based/`, `tests/cacheproxy/read-through/`, `tests/cacheproxy/write-through/`, `tests/cacheproxy/smoke/`.
- **executor:** `tests/executor/caching-results/`, `tests/executor/file-cache/`, `tests/executor/lifecycle/`, `tests/executor/platform-props/`, `tests/executor/resources/`, `tests/executor/scheduling/`, `tests/executor/stdio-exit/`, `tests/executor/smoke/`.
- **crosscutting:** `tests/crosscutting/bazel-download-modes/`, `tests/crosscutting/bazel-flags/`, `tests/crosscutting/bazel-matrix/`, `tests/crosscutting/e2e-proxy-exec/`, `tests/crosscutting/metrics-consistency/`, `tests/crosscutting/rand-exec/`, `tests/crosscutting/rand-trees/`, `tests/crosscutting/soak/`, `tests/crosscutting/upgrade-compat/`.

Passing controls also exist within defect-bearing areas: normal CAS/AC transfers and complete tree traversal, compression round trips, BES upload/replay, HTTP/API happy paths, non-overlapping output collection, regular timeout cleanup, and runner workspace/environment cleanup. Those controls help isolate the failures; they do not negate the findings.

Coverage sources are each area's BUILD/README at the fetched heads. In particular, [file-cache coverage and recorded green runs](https://github.com/altdansalt/buildbuddy-validation/blob/62a21bd6f9fecf67a0925b12d0d6af6be4cf3038/tests/executor/file-cache/README.md) validates eviction/concurrency/restart bytes, and [upgrade compatibility](https://github.com/altdansalt/buildbuddy-validation/blob/9ceacc1b54a1a093aab018be01aa45c05ceb3691/tests/crosscutting/upgrade-compat/README.md) records successful v2.313.0→v2.314.0 storage upgrades and app/executor skew. The ten-minute soak excludes slower leaks and capacity/eviction claims. Auth-group coverage uses one self-auth user owning two groups, not independently authenticated tenants. Resource tests distinguish tool/RLIMIT errors from actual executor cgroup enforcement. Scheduling finite-batch completion does not establish starvation freedom.

Firecracker remains untestable without a runner exposing usable `/dev/kvm`, executor device passthrough and the provider enabled. The `isolation-firecracker` directory's confirmed input-poisoning finding actually exercises **bare isolation**, not Firecracker. Privileged Docker alone cannot provide missing KVM; startup failures in this environment are not product defects. See [runner environment](../docs/environment.md) and [fixture instructions](../AGENTS.md).

No dedicated directory in the gathered tree covers persistent workers, macOS/Windows execution or browser UI workflows. The READMEs additionally exclude or limit distributed cache, MySQL/Postgres and multi-node deployments; external OIDC/SSO or independent-user trust boundaries; strict priority/fairness, retry exhaustion and hedging; in-flight scheduler migration, downgrades and arbitrary release chains; host-power-loss persistence; exhaustive seeds and long-duration/capacity tests. These are follow-up scopes, not presumed defects.
