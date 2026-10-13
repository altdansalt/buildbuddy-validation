# Graceful action timeout leaves a TERM-ignoring descendant running

Title: Graceful action timeout leaves a TERM-ignoring descendant running after the grace period
Components: executor
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app: gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: With bare-runner isolation and a nonzero termination grace period, a timed-out action can leave descendants consuming resources or modifying files after its result is finalized. This bypasses the configured execution bound and can interfere with subsequent actions on the same executor. The repro demonstrates a delayed file write inside the executor container; it does not establish a host or cross-organization escape.
Status: confirmed
Repro test target: //tests/executor/timeouts-cancel:graceful_timeout_child_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/0a1d5f58-db88-4faf-9780-0472840e3f0a

## Expected behavior

[REAPI Action.timeout](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) specifies a timeout after which execution should be killed. BuildBuddy additionally supports the [`termination-grace-period` platform property](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go): the executor sends graceful termination at the action timeout and sets a force-shutdown deadline at timeout plus grace period. A descendant that ignores TERM should be stopped by that final deadline, including when its parent handles TERM and exits zero.

The released [`commandutil` process-tree cleanup](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil_unix.go) explicitly aims to kill descendant processes. The action should not continue making observable changes after timeout and grace have both elapsed.

## Actual behavior

A one-second action with a two-second termination grace period starts a background child that ignores TERM, sleeps five seconds, and writes `survived` to a unique `/tmp` marker. The parent handles TERM by printing `terminated` and exiting zero. Child stdin/stdout/stderr are redirected away from the action's pipes.

The parent returns `ExecuteResponse.status=DEADLINE_EXCEEDED` and exit code `-2`, with stdout `startedterminated`. A subsequent action on the same executor waits six seconds and finds the child's marker. The child therefore ran beyond both the one-second timeout and the three-second force-shutdown deadline.

Independent fixture reproductions both fail on the intended assertion:

1. [First reproduction](https://app.buildbuddy.io/invocation/60caa0e4-bb4a-49c3-98fd-7cf66c171b3b): probe status OK, exit 9, stdout `survived`.
2. [Second reproduction](https://app.buildbuddy.io/invocation/0a1d5f58-db88-4faf-9780-0472840e3f0a): identical probe result; executor logs show graceful termination and DeadlineExceeded at the parent completion.

Harness/configuration checks: each test starts exactly one local app and one released executor, with a fixture-local Redis sidecar. Actions explicitly select `workload-isolation-type=none`, Linux/amd64. The marker includes the unique fixture ID and is only written by the timed-out child; a different action reads/removes it. No Docker exec, shared service, host process, prior fixture, or external API key is involved. Standard hard timeout descendant cleanup and TERM-ignoring top-level force-kill are covered by the passing regression target. Both image references are printed by the fixture; configs, container logs, and Docker inspect metadata are preserved in undeclared test outputs. No Go panic or fatal runtime error was found by the log checks in the second reproduction.

## Logs excerpt

```text
operation ...: status=code:4 message:"deadline exceeded" exit=-2
operation ...: status=<nil> exit=9
Error: Should be zero, but was 9
Messages: descendant survived past timeout and grace period: survived

2026/10/12 23:45:31.930 INF Sending graceful termination signal
2026/10/12 23:45:31.931 WRN Command execution returned error: rpc error: code = DeadlineExceeded desc = deadline exceeded
```

The container log excerpt is from the second reproduction; ANSI formatting and execution/fixture IDs are omitted. Full logs and inspect metadata remain in the test artifacts.

## Notes

Reproduce with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:graceful_timeout_child_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

No image overrides, executor flags, or Docker privileges are used in the repro. The action alone sets `timeout=1s`, `termination-grace-period=2s`, and `DoNotCache=true`; Execute sets `SkipCacheLookup=true`.

Likely source explanation, not a patched-source experiment: [`process.wait`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil_unix.go) closes `p.terminated` when the parent exits. The cleanup goroutine in [`startNewProcess`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil.go) then exits without killing descendants. [`ExecuteTaskAndStreamResults`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/executor/executor.go) finalizes the result immediately after graceful termination returns the command result. Its later context cancellation cannot revive the process cleanup goroutine, and the child is now reparented.

A zero grace period avoids the demonstrated path and is covered by the regression test. This report is limited to Linux bare-runner isolation; Docker and Firecracker lifecycle behavior was not tested. Independent verification below confirms this defect. No upstream issue or report was filed.

## Verification

Verdict: **confirmed**. Independently reviewed on 2026-10-12 against PR #16
commit `73e42db9da019e3a6665d257d54423ce1fe8b67a`. Both fresh-fixture runs of
the unchanged bug test failed at `graceful_orphan_test.go:29`, after the timeout
response and stdout checks passed: the probe had OK RPC status, exit 9, and
`survived`. Both logs identify the exact app and executor digests listed above.

Command (run twice):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:graceful_timeout_child_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Independent run 1](https://app.buildbuddy.io/invocation/bdfaaf09-d174-46e7-bc5a-2300c9aafd91)
  ([runner](https://app.buildbuddy.io/invocation/15519749-447e-4c80-9196-a9457da6f1a3)).
- [Independent run 2](https://app.buildbuddy.io/invocation/2b4e94f7-2807-473a-8f46-ab2d9b1f4ecd)
  ([runner](https://app.buildbuddy.io/invocation/17b3605d-9fc3-4820-91b8-b2e54a3ceadd)).

Attempts to disprove the report:

- Harness/config: inspected the fixture and upload/execute helpers. One local
  executor handles both uncached actions; a fixture-unique absolute marker is
  written only by the child and probed inside that executor. The test neither
  cancels its bounded RPC context nor mistakes stream disconnection for action
  cancellation. No host mount or shared component service supplies the marker.
- Spec: [REAPI Action.timeout](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L691)
  describes killing execution after its execution timeout, excluding queueing
  and input/output overhead. This report allows BuildBuddy's requested grace;
  it does not demand a kill at exactly one second.
- Intended behavior: the pinned [platform docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-platforms.md#L311)
  define grace as the interval before forceful shutdown. In pinned
  [executor.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/executor/executor.go#L410),
  the force deadline is separate from the TERM deadline. In
  [commandutil_unix.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil_unix.go#L52),
  parent wait closes `terminated`; the
  [cleanup goroutine](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil.go#L247)
  returns on that channel instead of retaining responsibility for descendants.
  The signal's best-effort caveat about children creating separate process
  groups does not explain this test: its child does not call setsid or setpgid.
  Bare container removal provides no additional process cleanup. This is a
  source-supported explanation, not a patched-source experiment.
- Flakiness: the child writes at five seconds; the force deadline is three
  seconds and the later probe waits six seconds. A slow/missing child could
  hide the defect, but cannot explain the observed unique marker in both runs.
  Both failures are the intended assertion, not startup, auth, or RPC errors.

Severity remains **medium**: a bounded action can continue resource use and
filesystem mutation after timeout. This establishes no host escape or
cross-group access; Docker and Firecracker are outside the verified scope.

## Version history

Tested on 2026-10-13 using the unchanged bug-tagged reproducer from
`origin/area/executor-timeouts-cancel` at `73e42db9da019e3a6665d257d54423ce1fe8b67a`.
App, Redis and executor were started inside each test on hosted `bb remote`
runners, with fixture-local endpoints and disposable local credentials.
No BuildBuddy component was built from source.

| Enterprise release (release date, UTC) | Result | BuildBuddy invocation |
| --- | --- | --- |
| v2.314.0 (2026-10-08) | Reproduces | [run](https://app.buildbuddy.io/invocation/e5764150-4a48-4b6e-ad0c-c98b8f31468f) |
| v2.313.0 (2026-10-06) | Reproduces | [run](https://app.buildbuddy.io/invocation/cb317630-52ba-4f3e-b30f-28fd56347f43) |
| v2.290.0 (2026-07-29) | Reproduces | [run](https://app.buildbuddy.io/invocation/62408081-e0dc-402f-969a-94cb0f12b958) |
| v2.275.0 (2026-06-04) | Reproduces | [run](https://app.buildbuddy.io/invocation/f947859e-1e56-4cf2-ac7f-e13f61bb2577) |
| v2.258.0 (2026-04-09) | Reproduces | [run](https://app.buildbuddy.io/invocation/e573a509-15e0-4647-aa4d-cd1a6f4fbb49); [independent repeat](https://app.buildbuddy.io/invocation/625e4a40-dd8d-45b3-b0d2-8f5041715a49) |

All five releases failed at the finding's intended assertion: After the expected
DEADLINE_EXCEEDED response, the later probe returned OK, exit 9 and "survived".
The oldest release was reproduced in two independent fresh fixtures.

**Earliest tested reproducing release: v2.258.0 (2026-04-09).** The defect
was present at or before that release; its actual introduction is unknown.
No earlier release or known-good boundary was tested, so this is an upper
bound on introduction, not evidence that v2.258.0 introduced it.

No release-specific configuration changes were needed for these five releases.

Command template, substituting the matching digests from the table below:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:graceful_timeout_child_test \
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
