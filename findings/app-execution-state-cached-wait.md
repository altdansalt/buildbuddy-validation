# Cache-hit Execute operation cannot be waited on

Title: Cache-hit Execute returns a completed operation that WaitExecution never answers
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: A client that reconnects to a cache-hit operation with WaitExecution gets no reply and must time out. The Execute stream itself succeeds, and Bazel does not call WaitExecution after a done operation, so practical impact is small. No data loss or isolation impact.
Status: confirmed
Repro test target: //tests/statemachine/execution-state:cached_replay_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/c87da5e9-d4d1-491f-a0b7-5e2b146e2b1a
Second invocation: https://app.buildbuddy.io/invocation/baaeeb1e-6a72-49d9-bdbe-f344cad0c049

## Expected behavior

The operation name returned by REAPI Execute should be accepted by WaitExecution, which should answer with the operation's current state. For a completed execution that is the terminal operation. REAPI only specifies that WaitExecution waits on an operation previously returned by Execute.

## Actual behavior

One mutation (`UpdateActionResult` seeding a result for an action), then three surfaces:

- Control, AC `GetActionResult`: returns the seeded result.
- `Execute` (cache lookup on): returns a done operation with `cached_result=true` and an operation name.
- `WaitExecution` on exactly that name: no message until the client's 10s deadline (`DeadlineExceeded` after 10.0007s and 10.0009s in the two runs below).

Root cause at v2.314.0 (`~/buildbuddy-src`, `enterprise/server/remote_execution/execution_server/execution_server.go`): `execute()` lines 1182-1190 answer a cache hit by calling `stateChangeFn(COMPLETED, ...)` directly on the client stream and returning. Nothing is published to the Redis execution stream. `waitExecution()` (line 1324) subscribes with `SubscribeTail` on that stream (line 1350), so for a cache-hit name it has nothing to read and blocks until the client gives up.

Only the 10s window was measured. "Never answers" comes from the source reading: nothing in the cache-hit path publishes to that stream, so a later answer would require the stream to be populated some other way. That was not tested. The test ruled out harness/config error: default fixture, only the app is started (no executor is needed for a cache hit), the same name format is used by the normal path, and the seeded result and `GetActionResult` control succeed.

## Logs excerpt

```text
cached_replay_test.go:39: cached operation=/uploads/da713da8-.../blobs/e2339850.../138
cached_replay_test.go:46: WaitExecution observation after 10.000665798s: rpc error: code = DeadlineExceeded desc = context deadline exceeded
Messages: completed cache-hit Execute operation must be replayable
```

## Notes

Default fixture configuration; no non-default product flags and no version override. App image only, as pinned above. Not claimed here: absence of cached executions from UI execution listings or other surfaces. The `bug` tag excludes the target from the default suite and `--config=nobug`. Run it with `--test_tag_filters=bug --nocache_test_results`. An earlier unlogged run also failed (exit 3). The two invocations above are the ones whose output was inspected.
