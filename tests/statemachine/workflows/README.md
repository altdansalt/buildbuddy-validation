# Workflow consistency coverage: prerequisite gap

Status: **unreachable in the current fixture; no product finding**. The included
`go_default_test` verifies only the fresh app's empty workflow definition list,
twice, over authenticated gRPC and the cookie-authenticated UI JSON RPC. It does
not claim create/run/delete state-machine coverage or silently skip those steps.

Released app: `gcr.io/flame-public/buildbuddy-app-enterprise:enterprise-v2.314.0`,
pinned by the harness to
`sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.

## Mutation and read-surface inventory

Source references below are relative to the v2.314.0 checkout.

| Mutating RPC | State and read surfaces to check after each operation | Coverage |
| --- | --- | --- |
| `BuildBuddyService.ExecuteWorkflow` | Response action invocation/execution IDs; `GetWorkflows`, `GetLinkedGitHubRepos`, `GetWorkflowHistory`; `GetInvocation`, `GetExecution`, `GetEventLogChunk` via gRPC and `/rpc/BuildBuddyService/*` UI JSON RPC; public API invocation/action/log readers; fixture-addressed `bb view` logs; app/proxy CAS/AC output reads where applicable | Blocked: valid repository and GitHub integration absent |
| `api.v1.ApiService.ExecuteWorkflow` (gRPC and public HTTP API) | Same run readers; response action IDs must join the selected repository, action, invocation, and execution | Blocked: same prerequisite |
| `BuildBuddyService.DeleteWorkflow` (gRPC and UI JSON RPC) | Legacy definition absent in `GetWorkflows`; repository discovery must retain its own meaning; retained invocation/execution/log data readable by saved IDs; history follows remaining linked repositories | Blocked: no legacy workflow definition to delete |
| `BuildBuddyService.LinkGitHubRepo` / `UnlinkGitHubRepo` (sequence prerequisites) | `GetLinkedGitHubRepos`, derived workflow lookup, history repository selection; unlink also deletes scheduled runs | Blocked: installation/token setup absent |

There is no workflow create/update RPC in `proto/buildbuddy_service.proto`.
Modern repository linking creates `GitRepositories`; legacy `DeleteWorkflow`
deletes `Workflows`, not `GitRepositories`. These are distinct state models.
`GetRepos` discovers repositories available to the legacy GitHub token, rather
than listing workflow definitions, so deleting a workflow must not imply that
GitHub repository discovery becomes empty. Cache proxy does not expose workflow
management/history; its applicable surfaces are cached action artifacts.
Repeat execution creates another run; it is not a run-deduplication oracle.
Legacy deletion looks up a definition first, so repeated deletion is not
specified as success. Historical data must be tested by saved IDs separately
from the workflow page's filtered history.

## Evidence from released source

- `proto/workflow.proto`, `proto/api/v1/workflow.proto`, and
  `proto/buildbuddy_service.proto` define the requests and run IDs.
- `enterprise/server/api/api_server.go:806` maps public execution to
  `GetLegacyWorkflowIDForGitRepository` and the workflow service. Public
  execution requires a linked repository, even though its derived ID is named
  "legacy".
- `enterprise/server/workflow/service/service.go:317` deletes only the legacy
  workflow row and unregisters its provider webhook. It does not delete
  invocation records. This is source behavior, not a verified retention test.
- `service.go:443` resolves/authorizes a workflow, fetches provider config, and
  schedules actions. `service.go:725` requires the GitHub app installation token
  for modern repository execution.
- `service.go:825` requires both an OLAP DB and a GitHub app before reading
  history. History selects linked repositories (falling back to legacy
  definitions) and complete CI invocations from the last seven days.
- `service.go:1026` constructs a default GitHub API client for legacy
  `GetRepos`, with no enterprise-host override in that path. Calling this
  surface with a token would address GitHub rather than a local fixture.
- `enterprise/server/githubapp/githubapp.go:1477` permits an enterprise HTTPS
  host for authenticated REST clients, but `IsTokenValid` at line 1574 creates
  a default GitHub client. A local fixture needs to account for all integration
  paths, not merely change the host setting.
- `githubapp.go:943` unlinks repository definitions and scheduled runs without
  deleting invocation records.
- `cli/view/view.go` reads `GetInvocation` and `GetEventLogChunk` for run logs.

## Required shared fixture work

Provide a local GitHub protocol server with git transport, OAuth exchange,
installation/token and repository/config APIs, trusted TLS and routing for all
GitHub paths that ignore enterprise-host configuration. Provision installation
and repository state through supported integration flows. Provide a pinned
local ClickHouse/OLAP service with ingestion enabled for workflow history. The
current harness starts Redis, app/executor/proxy only, and supplies neither this
integration nor an OLAP sidecar. These are fixture gaps, not released-product
inconsistencies. No shared harness or dependency files were changed.

Run the available baseline with:

```
bb remote test //tests/statemachine/workflows:go_default_test --test_output=errors
```

Verified baseline (1 test passed, 16.9 seconds):
https://app.buildbuddy.io/invocation/5e2f9556-0898-4d2a-b183-7df30acefd10

Remote runner:
https://app.buildbuddy.io/invocation/078e97ca-5d33-4169-b9e5-0e21e8e1eabd
