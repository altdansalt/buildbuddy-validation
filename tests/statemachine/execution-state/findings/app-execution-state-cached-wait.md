# Cached Execute operations cannot be replayed

Title: Cache-hit Execute returns a completed operation that WaitExecution cannot observe
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Authenticated cache-hit clients cannot reconnect to the returned operation using the standard execution wait API. The original Execute stream still succeeds; no data loss or isolation impact is established.
Status: confirmed
Repro test target: //tests/statemachine/execution-state:cached_replay_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/c30db903-56d0-4677-a1c7-7cec274f720f
Second invocation: https://app.buildbuddy.io/invocation/44742c06-7944-4fc5-86ac-78c021d2560a

## Expected behavior

The completed operation name returned by REAPI Execute should let REAPI WaitExecution replay that execution's completion, as it does for uncached executions. WaitExecution's REAPI contract waits for an execution previously started by Execute; the name is taken verbatim from Execute.

## Actual behavior

Both runs return a terminal ExecuteResponse with cached_result=true and a unique operation name. WaitExecution using that exact name receives no operation before the 10-second deadline (10.000755s and 10.000696s respectively). No convergence was observed.

This is a persistent missing publication by source inspection, rather than an observed delayed convergence: enterprise/server/remote_execution/execution_server/execution_server.go execute() lines 1184–1192 sends the cached terminal operation directly through stateChangeFn and returns without Redis publication or execution-response caching. waitExecution() reads the Redis execution stream. Runtime measurement proves absence for the ten-second observation window, not an infinite duration.

The same suite successfully replays non-cache-hit executions against local released app/executor containers. This isolated reproducer seeds a valid zero-exit, empty-output ActionResult using UpdateActionResult, uploads its Action/Command/Directory to local CAS, and uses ordinary Execute with cache lookup enabled. No executor is needed for a cache hit; no fault injection or unsupported auth is involved.

## Logs excerpt

```text
Execute: terminal cached_result=true
WaitExecution observation after 10.000754606s: rpc error: code = DeadlineExceeded
WaitExecution observation after 10.00069578s: rpc error: code = DeadlineExceeded
completed cache-hit Execute operation must be replayable
```

## Notes

Default fixture configuration and digest-pinned images; no non-default product flags or version overrides. Cache proxy image in fixture: gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906. No executor container is started. The `bug` target is excluded from the default suite. Run explicitly with `--test_tag_filters=bug --nocache_test_results`.

The broader absence of cached submissions from UI execution listings and the execution-ID AC namespace follows the same source branch but is not independently claimed by this minimal reproducer. Findings stay in this directory; no upstream report or PR was filed.
