# Remote-run creation and linkage

Source inventory: released v2.314.0, inspected locally under
`~/buildbuddy-src` (no source-built components).

| Mutator | Implementation / request contract | Read surfaces |
| --- | --- | --- |
| `buildbuddy.service.BuildBuddyService.Run` | `proto/runner.proto`, `server/buildbuddy_server/buildbuddy_server.go`, `enterprise/server/hostedrunner/hostedrunner.go` | `GetInvocation`, `GetExecution`, `GetEventLogChunk` over public gRPC and cookie-authenticated UI HTTP protolet |
| `api.v1.ApiService.Run` | `proto/api/v1/remote_runner.proto`, `enterprise/server/api/api_server.go:Run`; translates into hostedrunner.Run | `api.v1.GetInvocation`, `GetLog`, `GetAction` over gRPC and `/api/v1/*` HTTP |

Both HTTP creation projections are exercised too. Run is append-only: there is
no update/delete run RPC and no request idempotency key. Repeated valid requests
must create distinct invocation IDs. Direct invocation/log mutation, cancellation,
and REAPI execution mutations are owned by other items.

Each creation transport runs a successful command and a command intentionally
returning status 7. This is a valid workload result, not fault injection. Commands
read a fixture-local mounted Git repository, print a unique marker, and upload
an artifact. Requests explicitly select Linux/amd64 bare execution and route
BES/cache/execution to the test-local app. All three images use harness digest
pins, with actual images/configs/logs retained by the harness.

The reference model records each run identity, expected success/exit code, log
marker, and artifact contents. Queued/started requests use a fixture-local FIFO to hold a valid shell command
in the running state while comparing invocation and execution projections.
Live execution publication is explicitly enabled using
`remote_execution.write_execution_progress_state_to_redis`. The test releases
the FIFO after bounded condition polling observes the expected running state,
then polls both invocation and execution completion. Queued is an acceptance
condition; no minimum queued duration is assumed. Completed reads
compare identities, terminal invocation status, success, execution stage, exit
status, logs, artifacts, and app/proxy ByteStream contents. The cache proxy has
no invocation service or Run endpoint; its shared surface is artifact storage.

`api.v1.GetAction` projects BES target actions rather than the outer hosted-runner
REAPI action (`proto/api/v1/action.proto`, `api_server.go:GetAction`). Its REST and
gRPC results are compared separately from `GetExecution`.

The released CLI is read from the hosted-runner action input root in the local
CAS. `bb view --target=grpc://<fixture>` must expose the same log marker;
`bb download artifacts --target=grpc://<fixture>` must produce the modeled
artifact bytes. CLI subprocesses receive a clean environment with only the
fixture key (never the dispatch key). The mounted source repository has a
fixture-only `/etc/gitconfig` safe-directory entry for its host-owned Git data.

Run targeted validation with:

```
bb remote test //tests/statemachine/remote-runs:go_default_test --test_output=errors
```

## Known divergence

The released CLI currently prints no logs for a running invocation whose log
exists only as a live chunk, although `GetEventLogChunk` and public `GetLog`
return the already-published marker when their returned chunk token is followed.
`live_cli_repro` adds the live `bb view` assertion and is tagged `bug` so normal
CI remains green. See `app-remote-runs-live-cli-logs.md` for reproduction evidence.

```
bb remote test //tests/statemachine/remote-runs:live_cli_repro --test_tag_filters=bug --nocache_test_results --test_output=errors
```

Only QUEUED acceptance is asserted; scheduling can be too fast for a read to
observe the transient queued stage. Running is held deterministically by the
FIFO, and completion is polled with a deadline. No shared harness changes are
required. The finding follows `findings/TEMPLATE.md` but stays in this item's
owned directory, as required by the task's edit restriction.

Validation: [green normal suite](https://app.buildbuddy.io/invocation/4218bf07-97ba-4e72-8d74-ebbdae7a0f33),
[earlier green coverage](https://app.buildbuddy.io/invocation/197f4607-ec79-4c1f-8bab-adbe6199df15),
[uncached repro 1](https://app.buildbuddy.io/invocation/c26a8a1c-3cae-4952-aa76-107b590dba10),
[uncached repro 2](https://app.buildbuddy.io/invocation/fafe1832-3023-472a-b8ef-563f3f4c4d33).
