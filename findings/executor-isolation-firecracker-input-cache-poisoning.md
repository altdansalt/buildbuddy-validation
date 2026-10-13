# Bare action input mutation poisons subsequent action inputs

Title: Bare action input mutation poisons subsequent action inputs
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: An action writing its own ordinary input file causes a subsequent action on the same executor to receive incorrect contents for a valid CAS digest. This can silently change build results. Demonstrated within one local organization using bare isolation; cross-organization impact and other isolation providers were not tested. Bare execution already lacks host isolation, so this report makes no claim of a new sandbox escape.
Status: confirmed
Repro test target: //tests/executor/isolation-firecracker:input_mutation_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/03955bf8-ccd9-44b8-bae6-0345585bfa10

## Expected behavior

Each action's input tree should be materialized with the bytes named by its
`InputRootDigest` and the contained `FileNode.digest`. See the
[REAPI Action and FileNode definitions](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto).
A later action naming the original input digest should read `original\n`, even
if an earlier action modified its workspace copy. If input modification is
unsupported, preventing it would also avoid corrupting later materialization.

## Actual behavior

The first action runs `cat input; printf 'poisoned\n' > input` in its workspace.
The second action has a distinct command/action digest, no runner recycling, no
workspace preservation, and reads the same input digest. It succeeds but reads
`poisoned\n` rather than `original\n`. An intervening authenticated read of that
digest from the fixture-local app CAS returns the original bytes.

Both actions disable action caching (`DoNotCache=true`, `SkipCacheLookup=true`).
All requests address fixture-local app/executor containers. The fixture uses its
default supported bare configuration; no shared deployment or external API key
is involved. The only mutated path is the relative input file within the action
workspace, not a cache directory or arbitrary host path. This rules out action
cache reuse, uploading the wrong digest, and direct writes to harness storage.

Initial broader tests also reproduced changed inputs, including a traced run:
https://app.buildbuddy.io/invocation/95438fb4-2ef9-4a1d-a7df-9fc0f202605c.
The minimal target reproduced the same mismatch in two independent executions:

* [Run 1](https://app.buildbuddy.io/invocation/03955bf8-ccd9-44b8-bae6-0345585bfa10): expected `original`, observed `poisoned`; app CAS read passed.
* [Run 2](https://app.buildbuddy.io/invocation/558f4990-3bea-4405-91d0-b9e33ea2d45c): identical mismatch and passing app CAS read.

Both explicit bug-target runs used `--test_tag_filters=bug --nocache_test_results`.

## Logs excerpt

```text
TestInputMutationDoesNotPoisonLaterAction
expected: "next action: original\n"
actual  : "next action: poisoned\n"
later action must materialize the bytes named by InputRootDigest
```

The intervening CAS read assertion passes in the same test. Full fixture configs,
container logs and Docker inspect metadata are preserved in Bazel undeclared
outputs. No container panic is needed to reproduce this silent data corruption.

## Notes

Default digest-pinned images, no overrides. Executor settings:
`enable_bare_runner=true`, `default_isolation_type=none`, local file cache enabled.
Platform: `OSFamily=linux`, `Arch=amd64`, `workload-isolation-type=none`.
Actions have 30-second timeouts and RPCs have bounded contexts.

Source inspection suggests hardlink sharing with the executor's file cache:
[pinned dirtools](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go)
publishes downloaded files to the local cache and links cache hits into action
workspaces; [pinned fastcopy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/fastcopy/fastcopy_linux.go)
uses `os.Link` by default. This is a mechanism hypothesis, not instrumented inode
proof. Input materialization should protect shared cache contents or validate
and repair corrupted entries before reuse.

Mitigation for trusted bare workloads: replace the input file (unlink then
write), rather than modifying it in place. The passing workspace-cleanup tests
use that method to exercise cleanup independently. A complete mitigation should
be validated by maintainers; no configuration workaround is claimed here.

Firecracker was not tested because `/dev/kvm` is absent on the hosted runner,
as independently logged by this area's test. A KVM-capable runner and provider
setup remain necessary for VM isolation coverage. Independent verification is
recorded below.

## Verification

Verdict: **confirmed**. Independent review on 2026-10-12 of PR #20,
head `ffacde7`, using the unchanged bug target and helpers. Only this finding
is copied into the verification branch; the test remains on PR #20 until merged.

The reproducer is valid for **bare (`none`) isolation**, not Firecracker.
Both actions execute successfully against one disposable fixture-local app and
executor. Their command/action digests differ, `DoNotCache` and
`SkipCacheLookup` disable action-result reuse, and neither runner recycling nor
workspace preservation is requested. The first action's stdout proves its input
initially contains `original\n`; the intervening app CAS read still returns those
bytes. Only the later action reads `poisoned\n`. Re-uploading the original CAS
blob in the helper does not repair the executor-local file cache. This is not a
wrong upload, stale action result, missing KVM, or a fixture startup failure.

The [REAPI definitions at commit 6def1c5](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto)
define `Action.input_root_digest` as the input directory tree and
`FileNode.digest` as the digest of the file's content. These definitions do not
promise that a command can modify inputs in place, but they also do not permit
one command to change the bytes materialized for a later command's unchanged
content digest. Denying the initial write would be acceptable; silently serving
the mutated content to the next action is the defect.

Pinned source (`v2.314.0`, commit
`974929724a4c75f824ab18fbcb560e48bbf2803a`) supports the report's mechanism:

- [dirtools.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go): `linkFileFromFileCache` calls `FastLinkFile`; a successful cache hit bypasses remote fetching.
- [filecache.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/filecache/filecache.go): `FastLinkFile` checks cache presence and links the file without rehashing it. `cloneOrLink` uses `FastCopy` for authenticated groups by default; `executor.local_cache_always_clone` defaults to false.
- [fastcopy_linux.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/fastcopy/fastcopy_linux.go): `FastCopy` uses `os.Link`, sharing the inode. [fastcopy.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/fastcopy/fastcopy.go) defaults reflinking to false, so setting only `local_cache_always_clone` is not an established workaround.
- [rbe-platforms.md](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-platforms.md#action-isolation-and-hermeticity-properties): documents `none` for self-hosted executors and describes recycling/preservation as opt-in. It does not document input-cache corruption as an expected consequence. Intentional hardlink optimization explains the behavior but does not make incorrect later input contents correct.

Bare execution permits access to the host and is unsuitable for hostile actions;
this review does not treat the result as an isolation bypass. The test modifies
only its ordinary relative input path. **Medium severity retained** for silent
same-organization build correctness loss. Cross-organization, Docker, OCI and
Firecracker impact remain unverified. The source explanation is not an
instrumented inode measurement.

Independent runs used the exact pinned app/executor digests listed above, with
no image overrides, and this command from the checked-out PR branch:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-firecracker:input_mutation_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

The empty tag filter explicitly includes the bug target; disabling test-result
caching forces fresh execution. Both runs failed at the final content assertion,
with successful action exit codes and a passing intervening CAS read:

- [Independent run 1](https://app.buildbuddy.io/invocation/02349a86-6e03-4671-9183-1277e83a6acf) ([outer runner](https://app.buildbuddy.io/invocation/0f63fd51-d4b8-450d-8493-47d5ecc43a01)): expected `next action: original\n`, actual `next action: poisoned\n`.
- [Independent run 2](https://app.buildbuddy.io/invocation/e934d5e6-4b03-4aa9-8b72-39d19993def0) ([outer runner](https://app.buildbuddy.io/invocation/b3885fc4-8bd2-4736-9c70-aac6d618f3f6)): identical mismatch in a fresh fixture.

## Version history

Tested on 2026-10-13 using the unchanged bug-tagged reproducer from
`origin/area/executor-isolation-firecracker` at `ffacde7098f82a382fcfd2142828c626cd16e387`.
App, Redis and executor were started inside each test on hosted `bb remote`
runners, with fixture-local endpoints and disposable local credentials.
No BuildBuddy component was built from source.

| Enterprise release (release date, UTC) | Result | BuildBuddy invocation |
| --- | --- | --- |
| v2.314.0 (2026-10-08) | Reproduces | [run](https://app.buildbuddy.io/invocation/652be986-b22c-486b-85cc-0610fafd827a) |
| v2.313.0 (2026-10-06) | Reproduces | [run](https://app.buildbuddy.io/invocation/faeab226-4045-4d23-ad5f-48b77c17ffd1) |
| v2.290.0 (2026-07-29) | Reproduces | [run](https://app.buildbuddy.io/invocation/8b6f2d1c-1cad-429b-9f78-6670b321e224) |
| v2.275.0 (2026-06-04) | Reproduces | [run](https://app.buildbuddy.io/invocation/99de7bfa-c4c1-42ec-a9c8-13c7cd18b56e) |
| v2.258.0 (2026-04-09) | Reproduces | [run](https://app.buildbuddy.io/invocation/2c719411-3940-4949-8faa-f4d029890f90); [independent repeat](https://app.buildbuddy.io/invocation/a8e15e5f-e12e-46a2-b4b5-b6b04835a47e) |

All five releases failed at the finding's intended assertion: The later action
read "poisoned" instead of "original"; both action exits and the intervening app
CAS read passed.
The oldest release was reproduced in two independent fresh fixtures.

**Earliest tested reproducing release: v2.258.0 (2026-04-09).** The defect
was present at or before that release; its actual introduction is unknown.
No earlier release or known-good boundary was tested, so this is an upper
bound on introduction, not evidence that v2.258.0 introduced it.

No release-specific configuration changes were needed for these five releases.
Despite the area name, this target selects bare (`none`) isolation and needs no KVM.

Command template, substituting the matching digests from the table below:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-firecracker:input_mutation_bug_test \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image=gcr.io/flame-public/buildbuddy-app-enterprise@<app-digest> \
  --test_arg=-harness.executor_image=gcr.io/flame-public/buildbuddy-executor-enterprise@<executor-digest>
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
