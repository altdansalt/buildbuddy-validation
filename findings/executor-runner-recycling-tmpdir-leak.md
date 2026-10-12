# Recycled bare runner retains task-scoped TMPDIR files

Title: Recycled bare runner retains task-scoped TMPDIR files across actions
Components: executor
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8
Severity: medium
Severity justification: Same-group, same-instance actions with matching recycling platforms can read stale task-private temporary data or produce incorrect results from stale temporary files. Bare isolation and recycling must be enabled. This is not evidence of cross-group isolation failure.
Status: confirmed
Repro test target: //tests/executor/runner-recycling:tmp_leak
BuildBuddy invocation: https://app.buildbuddy.io/invocation/46e3682e-d476-43a2-b483-852d15450cd0

## Expected behavior

The released [bare provider](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/bare/bare.go) describes `executor.bare.enable_tmpdir` as provisioning a temporary directory for each task and removing it when the task completes. A subsequent action should not find a previous action's file in the executor-provided TMPDIR. [REAPI Command](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) allows worker-provided default environment variables; this finding concerns the lifetime contract of that supplied directory, not a spec requirement to isolate arbitrary absolute paths.

## Actual behavior

Action one writes `private-first-action-data` to `$TMPDIR/recycling-secret`.
Action two, with the same recycling key and no inputs, reads that file and exits
42. Equal `pwd` values confirm actual runner reuse. Both ExecuteResponses have
OK status; the test fails on the command exit code.

Independent fixtures reproduced this twice with `--nocache_test_results`:

- https://app.buildbuddy.io/invocation/46e3682e-d476-43a2-b483-852d15450cd0
- https://app.buildbuddy.io/invocation/6b116c48-9603-463a-a643-c25c2d725839

The fixture uses released digest-pinned images, default bare TMPDIR provisioning,
local authenticated RPCs, isolated app/Redis/executor containers and no custom
mounts. Actions disable action caching and skip cache lookup. The first action
requires nonempty TMPDIR. The same-key test verifies workspace reuse; the
passing regression separately checks workspace and environment cleanup.
No harness-preserved filesystem or explicitly supplied TMPDIR is involved.

## Logs excerpt

```text
Run 1:
instance=tmp-leak key=a status=0 exit=0 stdout="/data/executor/837f5645-ee95-4892-bce8-502356af3dcb\n"
instance=tmp-leak key=a status=0 exit=42 stdout="/data/executor/837f5645-ee95-4892-bce8-502356af3dcb\nLEAK: private-first-action-data"
Run 2:
instance=tmp-leak key=a status=0 exit=0 stdout="/data/executor/0006ad87-c7cc-499b-b357-5b2162b89c69\n"
instance=tmp-leak key=a status=0 exit=42 stdout="/data/executor/0006ad87-c7cc-499b-b357-5b2162b89c69\nLEAK: private-first-action-data"
Error: Should be zero, but was 42
```

## Notes

Run with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/runner-recycling:tmp_leak --test_tag_filters=bug --nocache_test_results --test_output=all
```

Platform: `Arch=amd64`, `OSFamily=linux`, `recycle-runner=true`,
`runner-recycling-test-key=a`, `workload-isolation-type=none`. Fixture defaults
set `executor.enable_bare_runner=true` and `executor.default_isolation_type=none`.
App image is enterprise-v2.314.0 at
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
No version overrides. Full container/config/inspect logs are retained by the
harness as undeclared test outputs.

Source inspection: `Create` sets TMPDIR to the workspace's sibling `.tmp`;
`Remove` deletes it, but `Pause` is a no-op. Runner recycling cleans workspace
children and pauses the container, leaving that sibling intact. Disabling
runner recycling avoids this lifetime issue; tasks can also explicitly use
and clean their own task-specific temporary directory. Bare containers document
that they provide no isolation: this finding does not assert protection for
arbitrary `/tmp` paths. Independent verification below confirms this defect.

## Verification

Verdict: **confirmed**. Independently reviewed on 2026-10-12 against PR #28
commit `06685da3d2ed30e8f259ddd5e30de239f360c474`. Two unchanged bug-test runs in fresh
fixtures failed in the `ok` exit-code assertion at `recycling_test.go:67`: both actions had OK RPC status,
the first exited zero, and the second exited 42 with
`LEAK: private-first-action-data`. Within each run the printed workspace paths
were exactly equal; across runs they were different UUID paths. Both logs
identify the exact app and executor digests listed above.

Command (run twice):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/runner-recycling:tmp_leak --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Independent run 1](https://app.buildbuddy.io/invocation/80d712e7-1202-4517-b0af-01111a0b3fa4)
  ([runner](https://app.buildbuddy.io/invocation/631c104d-9c89-48ac-9886-cab9e9adc430)).
- [Independent run 2](https://app.buildbuddy.io/invocation/9b70d4d9-e973-4478-b155-6602342b967e)
  ([runner](https://app.buildbuddy.io/invocation/199de6a6-3d81-45a8-996f-40dbb9ff7e7e)).

Attempts to disprove the report:

- Harness/config: reviewed fixture defaults and command construction. No
  TMPDIR override, custom mount, input file, preserve-workspace property, or
  action-cache hit explains the file. Both commands use the same authenticated
  local group, instance and recycling platform; task one requires nonempty
  executor-provided TMPDIR and writes the exact content later observed.
- Spec: [REAPI Command.environment_variables](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L777)
  permits worker defaults. REAPI does not independently promise task-private
  temporary storage; the violated promise is BuildBuddy's bare-provider
  lifetime contract.
- Intended behavior: the pinned [recycling docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-platforms.md#L258)
  explicitly warn that recycling reduces hermeticity and retains containers,
  while cleaning workspaces by default. This is the strongest counterargument.
  However, [bare.go Opts.EnableTmpdir](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/bare/bare.go#L35)
  specifically promises a directory provisioned for each task and removed
  when the task completes, without a recycling exception. `Create` allocates
  `workDir + ".tmp"`, `Exec` reuses it, `Remove` deletes it, and `Pause` does
  nothing. [TryRecycle](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/runner/runner.go#L1621)
  cleans only the workspace and pools the runner;
  [Workspace.Clean](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go#L708)
  removes children, not the sibling `.tmp`. Generic retention does not satisfy
  this more specific task-scoped directory promise. Bare isolation's lack of
  security separation does not invalidate the cleanup defect.
- Flakiness: the one-second pooling delay can cause a false negative if a new
  runner is selected; the reuse assertion detects that scenario. The prefix
  assertion is weaker than exact first-line equality, but both actual logs
  show exact equality and the marker content, so it does not undermine these
  reproductions. Neither failure was an infrastructure or readiness error.

Severity remains **medium** for incorrect builds, stale temporary data and
potential disk accumulation under enabled recycling. The demonstrated exposure
is between matching same-group actions, not a cross-group security boundary.
No broader isolation claim or source patch was tested.
