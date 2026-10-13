# Bare executor ignores Command PATH during executable lookup

Title: Bare executor ignores the action's PATH when resolving an unqualified executable
Components: executor
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8
Severity: medium
Severity justification: Valid actions that locate tools via their declared PATH fail before execution; executor-default PATH can also select a different tool than the action requests. Requires the bare runner and an unqualified command name. No cross-tenant or sandbox escape is demonstrated.
Status: confirmed
Repro test target: //tests/executor/env-args:path_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/74ade986-8d92-47c7-a89a-b4cde16d88a6

## Expected behavior

[REAPI Command.arguments](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)
permits an unqualified executable resolved through the OS equivalent of PATH and
states that an explicitly supplied PATH **SHOULD** be respected. This is a SHOULD
recommendation, not a MUST requirement. Relative executables are resolved from
the command working directory.

Given an executable input `tools/envargs-tool`, `working_directory="tools"`,
`arguments=["envargs-tool", "path"]`, and `environment_variables=[PATH="."]`,
the executor should invoke that input executable and return exit code 0 and
stdout `input-tool:path\n`.

## Actual behavior

The action returns `ExecuteResponse.status.code=5` (NOT_FOUND), exit code -2,
and `exec: "envargs-tool": executable file not found in $PATH`.

Reproduced with uncached independent fixtures twice:

1. [First test invocation](https://app.buildbuddy.io/invocation/74ade986-8d92-47c7-a89a-b4cde16d88a6), outer runner [81216b9b-80a2-4a23-918f-e42fa751692e](https://app.buildbuddy.io/invocation/81216b9b-80a2-4a23-918f-e42fa751692e).
2. [Second test invocation](https://app.buildbuddy.io/invocation/5894f730-7ab5-4d53-8ce3-ed29360d363b), outer runner [5f9914f4-cac0-4e53-9e62-2516f3fef409](https://app.buildbuddy.io/invocation/5f9914f4-cac0-4e53-9e62-2516f3fef409).

In both fixtures, a control using `arguments=["./envargs-tool", "control"]`
from the identical working directory succeeded with exit code 0 and expected
stdout. This rules out input upload, executable permissions, working-directory,
shebang/interpreter, authentication, scheduler, and bare-runner setup errors.
Actions use `DoNotCache=true` and `SkipCacheLookup=true`; test runs use
`--nocache_test_results`. Only fixture-local containers receive component RPCs.

## Logs excerpt

Test log from both initial reproduction runs:

```text
execution status=<nil> exit=0
execution status=code:5 message:"exec: \"envargs-tool\": executable file not found in $PATH" exit=-2
Error: Should be zero, but was 5
Test: TestCommandPATH
```

A third run of the final reproducer also failed as expected:
[ae46ac13-bad9-4f22-9b9c-a5e5ec6ff61f](https://app.buildbuddy.io/invocation/ae46ac13-bad9-4f22-9b9c-a5e5ec6ff61f).
The test printed this saved executor log excerpt (IDs omitted):

```text
2026/10/12 23:48:15.858 WRN Command execution returned error: rpc error: code = NotFound desc = exec: "envargs-tool": executable file not found in $PATH
```

Its saved-log checks found no Go panic/fatal-error signatures.

Full fixture configs, inspect metadata, and container logs are retained in each
target's undeclared outputs. The excerpt above is the returned execution status,
not an inferred container error.

## Notes

- App: enterprise-v2.314.0, `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
- Default harness configuration; executor bare provider enabled, default isolation
  `none`; command platform `OSFamily=linux`, `Arch=amd64`,
  `workload-isolation-type=none`. No image or flag overrides.
- [Released commandutil source](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil.go)
  calls `exec.Command(executable, args...)` before setting `cmd.Dir` and before
  appending the action's variables to `cmd.Env`. Go's initial unqualified-name
  lookup therefore uses the executor process PATH, not the action PATH.
- Workaround: use an absolute executable path or a qualified relative executable
  such as `./envargs-tool`.
- Docker/OCI and Firecracker isolation providers were not investigated here.
  No upstream report was filed. Independent verification is recorded below.

## Verification

Verdict: **confirmed**. Independently reviewed on 2026-10-12/13 using PR #30
commit `87dd488cbfab3f9e6e0078653003f3afeca77316`; tests were run unchanged from
that branch in a separate checkout. Both runs used the exact app/executor image
digests above, default fixture configuration, local credentials and local
container endpoints. No component was built from source.

Commands (run twice):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/env-args:path_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Verification run 1](https://app.buildbuddy.io/invocation/5c64c010-ed2e-4868-84b1-51e7a7e4e362), [runner](https://app.buildbuddy.io/invocation/8e3b3ddf-bebb-49f8-ade8-4b2d476a2dc3).
- [Verification run 2](https://app.buildbuddy.io/invocation/0618bd02-be32-469c-b2e4-754a877761bd), [runner](https://app.buildbuddy.io/invocation/0ebac6dc-883b-4096-9578-a27c4932a145).

Both reached command execution: `./envargs-tool` returned exit 0 and the
expected stdout; the unqualified name with `PATH=.` returned NOT_FOUND (5),
exit -2, and `executable file not found in $PATH`. The control excludes missing
input, permissions, working directory, interpreter and scheduler failures.
The script uses an absolute `/bin/sh` shebang and shell-builtin printf, so
`PATH=.` does not remove a required external utility. The failures occurred
before the action timeout, and action/result caching was disabled.

[REAPI Command.arguments at 6def1c5d27a5](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L760)
allows unqualified commands and says explicitly supplied PATH **SHOULD** be
respected. This confirms a correctness defect relative to that recommendation,
not a violation of a MUST requirement. The older pre-v2.3 rule forbidding PATH
lookups does not explain this release: its [capabilities server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go#L61)
advertises through REAPI 2.11.

The [bare runner](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/bare/bare.go#L165)
passes the action working directory to commandutil. However,
[constructExecCommand](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil.go#L86)
calls Go `exec.Command` before assigning Dir and Env, which explains lookup
against the executor process environment. Reviewed release [RBE setup docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-setup.md)
and [RBE configuration docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-rbe.md);
found no documented bare-runner exception for action PATH. This appears to be
an implementation omission rather than intentional behavior.

Severity remains **medium**: ordinary tool discovery fails for this provider,
with a straightforward qualified-path workaround. Selecting a different tool
is a source-derived risk, not demonstrated by this reproducer. No security
impact or applicability to other isolation providers is established.

## Version history

Tested on 2026-10-13 using the unchanged bug-tagged reproducer from
`origin/area/executor-env-args` at `87dd488cbfab3f9e6e0078653003f3afeca77316`.
App, Redis and executor were started inside each test on hosted `bb remote`
runners, with fixture-local endpoints and disposable local credentials.
No BuildBuddy component was built from source.

| Enterprise release (release date, UTC) | Result | BuildBuddy invocation |
| --- | --- | --- |
| v2.314.0 (2026-10-08) | Reproduces | [run](https://app.buildbuddy.io/invocation/9880cb80-30d6-46fd-a158-0f3c80f66827) |
| v2.313.0 (2026-10-06) | Reproduces | [run](https://app.buildbuddy.io/invocation/a1bd24a5-78c0-4076-8fc7-4af329245722) |
| v2.290.0 (2026-07-29) | Reproduces | [run](https://app.buildbuddy.io/invocation/f41e17d5-79f2-4a52-bfe4-0271bcf2dbb0) |
| v2.275.0 (2026-06-04) | Reproduces | [run](https://app.buildbuddy.io/invocation/8ea4a3a0-db7f-4b4b-80d0-936d2161586e) |
| v2.258.0 (2026-04-09) | Reproduces | [run](https://app.buildbuddy.io/invocation/dcc942fe-7a26-4a37-9cec-40fd42ce974f); [independent repeat](https://app.buildbuddy.io/invocation/a2e0ce59-f648-41bf-a3d6-613c0d7aed37) |

All five releases failed at the finding's intended assertion: NOT_FOUND (5),
exit -2 for the unqualified executable with action PATH=.; the qualified-path
control returned OK and exit 0.
The oldest release was reproduced in two independent fresh fixtures.

**Earliest tested reproducing release: v2.258.0 (2026-04-09).** The defect
was present at or before that release; its actual introduction is unknown.
No earlier release or known-good boundary was tested, so this is an upper
bound on introduction, not evidence that v2.258.0 introduced it.

No release-specific configuration changes were needed for these five releases.

Command template, substituting the matching digests from the table below:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/env-args:path_bug_test \
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
