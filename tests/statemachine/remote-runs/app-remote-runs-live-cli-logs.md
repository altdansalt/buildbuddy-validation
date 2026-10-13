# Released bb view omits available live remote-run logs

Title: bb view exits successfully with empty output while APIs expose live run logs
Components: app / released embedded bb CLI
Image version/digest: enterprise-v2.314.0
- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- executor: `gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8`
- cache proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: Users cannot inspect already-published logs using bb view until logs flush to storage. This impedes diagnosis of long-running or blocked remote commands; no data loss or authorization boundary violation was observed.
Status: confirmed
Repro test target: //tests/statemachine/remote-runs:live_cli_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/c26a8a1c-3cae-4952-aa76-107b590dba10
Second reproduction: https://app.buildbuddy.io/invocation/fafe1832-3023-472a-b8ef-563f3f4c4d33

## Expected behavior

`bb view <invocation> --target=grpc://<fixture-app>` should display existing build logs for the same running invocation that the UI and public APIs expose. It need not follow newly arriving logs indefinitely. The released CLI usage describes fetching and displaying build logs; `GetEventLogChunk` documents that an empty chunk ID selects the last chunk.

## Actual behavior

A valid fixture-local repository is checked out on the released bare executor. A shell step prints a marker and waits on a test-local FIFO. Bounded polling verifies PARTIAL invocation status, EXECUTING execution stage, and the published marker via GetEventLogChunk. api.v1.GetInvocation and GetLog (gRPC and REST) confirm the same live state. The released bb binary is downloaded from that run's CAS input root, so there is no worker CLI version mismatch. Its view command returns exit status 0 with empty stdout/stderr.

Both uncached reproduction invocations fail at this assertion for all four creation transports: BuildBuddyService.Run gRPC/UI HTTP and api.v1.Run gRPC/REST. The main conformance target passes for completed runs, including CLI view and artifact download, so local authentication, routing and CLI executable compatibility are verified. Test commands, repositories, keys, and endpoints are all fixture-local. The fixture releases the FIFO even on assertion failure; this is valid workload coordination, not fault injection.

## Logs excerpt

```text
--- FAIL: TestRemoteRunSurfaces/ui-grpc
Error: "" does not contain "conformance-ui-grpc-started"
Messages: API live logs contain this marker while the fixture-local shell waits at its FIFO
--- FAIL: TestRemoteRunSurfaces/api-grpc
Error: "" does not contain "conformance-api-grpc-started"
```

## Notes

Source inspected at released v2.314.0:
- `cli/view/view.go:ViewLogs` issues GetEventLogChunk and prints only Buffer; it does not follow NextChunkId if the first buffer is empty.
- `server/eventlog/eventlog.go:GetEventLogChunk` returns an empty Buffer and NextChunkId `0000` when there are no persisted chunks and the request does not explicitly identify the available live chunk. Following that returned token retrieves the marker.
- `enterprise/server/api/api_server.go:GetLog` maps PageToken to the same ChunkId.

The normal target follows returned live chunk tokens and verifies all terminal projections. The bug-tagged target adds the failing live CLI assertion. Mitigation: follow GetLog/GetEventLogChunk tokens while running, or inspect the invocation after completion.

Fixture config explicitly selects `workload-isolation-type=none`, Linux/amd64, fixture-local BES/cache/execution endpoints, and `remote_execution.write_execution_progress_state_to_redis=true`. No image overrides are used. Harness artifacts retain exact configs, image inspect metadata and container logs. QUEUED is checked as acceptance, without assuming a minimum queue duration.

This file follows `findings/TEMPLATE.md` and remains in the owned item directory because the task only authorizes edits there. The coordinator may copy it to `findings/app-remote-runs-live-cli-logs.md` and regenerate the index. No shared harness changes are needed and no upstream report was filed.
