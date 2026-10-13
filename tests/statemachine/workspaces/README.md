# Workspace overlay persistence: prerequisite gap

Status: not covered; no product defect claimed. The executable target checks the
repository-read prerequisite on the released app with workspaces explicitly enabled.
It does not claim mutation or cross-surface conformance.

Source inventory at v2.314.0:

| Mutation | Read surfaces | Coverage result |
| --- | --- | --- |
| SaveWorkspace (create, replace overlay, repeat save, record file deletions) | GetWorkspace metadata; GetWorkspaceDirectory merged repository tree; GetWorkspaceFile overlay/base content; all three via gRPC and UI `/rpc/BuildBuddyService/<method>` | Blocked: no supported linked GitHub protocol fixture in harness |

There is no workspace-delete RPC or bb CLI overlay reader. The cache proxy does
not expose the workspace service, so app/proxy workspace agreement is inapplicable.

Evidence from released source:
- `proto/buildbuddy_service.proto:193-200` lists all four workspace RPCs.
- `proto/workspace.proto` defines full overlay saves and file change types.
- `enterprise/server/workspace/workspace_service.go`: `nodesFromGitHub`,
  `getGithubFileFromSha`, and `getGithubFileFromPath` require GitHubAppService;
  they return `Unimplemented: No GitHub app configured` without it. No local
  filesystem or generic Git repository fallback exists.
- `enterprise/server/githubapp/githubapp.go`: authenticated repository reads
  require linked installations or a user GitHub token. Enterprise clients support
  a configured HTTPS GitHub enterprise host; this could enable a future local
  protocol fixture, but the current self-auth harness provisions neither linked
  GitHub users nor installations, TLS trust, or a GitHub protocol server.
- `cli/` contains no client for these workspace RPCs.

Required follow-up: a fixture-local HTTPS GitHub protocol server, trusted by the
app container, and supported local GitHub account/app linking. Then run a reference
model through create, modify, add, delete, repeat save, and removal of overlay
changes, comparing metadata, root/nested listings, hashes, file contents, and
untouched base files through gRPC and protolet HTTP after every revision. Merely
omitting repository refs would bypass the base-tree oracle and does not cover this
item. No shared harness or dependencies were modified.

App image: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
(`enterprise-v2.314.0`).
