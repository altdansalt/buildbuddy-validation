# GitHub object state: unreachable prerequisite

Status: **not covered**, not a product finding. The passing test probes only
the default released app's HTTP integration gate. It does not exercise mutations
or establish cross-surface consistency. No upstream GitHub request is issued.

Release: enterprise-v2.314.0 app,
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.

## Source inventory and intended oracle

All rows require comparing the local upstream model with both the named gRPC
reads and their UI HTTP `/rpc/BuildBuddyService/<method>` equivalents. None of
these transitions has been executed in this item.

| Mutating RPC | Read surfaces required | Result |
| --- | --- | --- |
| CreateRepo | GetGithubRepo, GetLinkedGitHubRepos (unless skip_link) | blocked |
| CreateGithubBlob | GetGithubBlob; GetGithubContent after attaching through tree/commit/ref | blocked |
| CreateGithubTree | GetGithubTree; GetGithubContent after publishing commit/ref | blocked |
| CreateGithubCommit | GetGithubCommits, GetGithubCompare after publishing ref | blocked |
| CreateGithubRef | GetGithubCommits, GetGithubTree, GetGithubContent, GetGithubCompare | blocked |
| UpdateGithubRef | GetGithubCommits, GetGithubTree, GetGithubContent, GetGithubCompare | blocked |
| CreateGithubFork | GetGithubForks, GetGithubRepo on fork | blocked |
| CreateGithubPull | GetGithubPullRequest, GetGithubPullRequestDetails, GetGithubCompare | blocked |
| MergeGithubPull | GetGithubPullRequest, GetGithubPullRequestDetails, GetGithubCommits, GetGithubCompare | blocked |
| CreateGithubPullRequestComment | GetGithubPullRequestDetails comments and draft_review_id | blocked |
| UpdateGithubPullRequestComment | GetGithubPullRequestDetails comment identity/body | blocked |
| DeleteGithubPullRequestComment | GetGithubPullRequestDetails comment absence | blocked |
| SendGithubPullRequestReview | GetGithubPullRequestDetails reviewers, comments, draft_review_id | blocked |

Object APIs expose no repository/blob/tree/commit/ref deletion RPC. Only comment
deletion is in scope. Repeated immutable object creation and repeated ref updates
are candidates for valid idempotency checks; duplicate ref creation or deleting
an already deleted comment must not be assumed valid. Fork reads expose only the
owner, so full upstream fork identities cannot be asserted from that response.
The cache proxy exposes cache services, not these app APIs. No bb CLI
object-management read surface was found.

## Reachability evidence

Inspection of released source at `~/buildbuddy-src`:

- `proto/buildbuddy_service.proto:314-365`, `proto/github.proto`, and
  `proto/repo.proto` define the inventory above.
- `server/buildbuddy_server/buildbuddy_server.go` gates GitHub handlers on the
  registered GitHub app service; disabled integration returns Unimplemented.
- `enterprise/server/githubapp/githubapp.go` registers only when
  `github.app.enabled` or `github.read_only_app.enabled` is true. Object clients
  require the authenticated user's stored GithubToken. The harness supplies
  self-auth and group/API keys, but no GitHub OAuth link or app installation.
- `newAuthenticatedClient` and `newAuthenticatedGraphQLClient` support
  `github.enterprise_host`, using HTTPS. A local protocol fixture would need
  a trusted TLS certificate inside the released container and OAuth bootstrap.
- `GetGitHubAppForAuthenticatedUser` uses installed-app records if present;
  otherwise `IsTokenValid` uses `github.NewClient` without the enterprise host
  override. Simply configuring enterprise_host is insufficient to guarantee
  fixture-local traffic. Provisioning a local installation is a possible bypass
  requiring additional protocol bootstrap, not a proven product defect.
- UI consumers are in `enterprise/app/code/code_v2.tsx`, `repo/repo.tsx`,
  `review/view_pull_request.tsx`, `review/review_list.tsx`, and
  `review/file_content_service.ts`; they use the same BuildBuddyService methods.

Requested shared fixture capability: trusted HTTPS GitHub REST/GraphQL/OAuth
protocol server, local user linking and installation bootstrap, and an outbound
request guard proving that authentication and mutations stay fixture-local.
No harness or dependency changes were made. This prerequisite probe must not be
counted as exhaustive conformance coverage.

Run: `bb remote test //tests/statemachine/github-object-state:go_default_test --test_output=all`.
