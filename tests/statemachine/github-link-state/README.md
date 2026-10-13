# GitHub link state conformance

Released app: enterprise-v2.314.0,
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
No version overrides, BuildBuddy source build, external GitHub account, or shared
component deployment is used.

## Mutation and read inventory

All reads below are checked through authenticated gRPC, cookie-authenticated
HTTP protobuf, and the browser's HTTP JSON protolet endpoint after each operation.
Every successful mutation runs over gRPC. The model independently tracks user
and legacy org account links, installation presence and commit-status setting,
repository presence and default-workflow setting. GitHub-side installation and
repository authorization stays fixed when BuildBuddy-side links change.

| Mutation | Read surfaces and model expectations |
| --- | --- |
| UnlinkUserGitHubAccount | GetUser user flag clears; GetGithubUser, GetGithubUserInstallations and GetAccessibleGitHubRepos lose access; org flag, GetRepos and local repo/installation listings remain unchanged. Repeated unlink succeeds. |
| UnlinkGitHubAccount (unlink_user_account=true, deprecated path) | Same user-token transition and independent org-token state. |
| UnlinkGitHubAccount (org path) | GetUser.user_group[].github_linked clears; GetRepos loses access; local installation/repo listings remain unchanged. |
| LinkGitHubAppInstallation | GetGitHubAppInstallations gains installation ID, app ID, owner, group and default commit-status setting; accessible/user-installation listings remain unchanged; user/org flags remain independent. Valid repeat replaces the same row. |
| UpdateGitHubAppInstallation | GetGitHubAppInstallations reports the changed commit-status setting; all other link and authorization state is preserved. Toggle true/false and repeat identical updates. |
| UnlinkGitHubAppInstallation | GetGitHubAppInstallations loses the installation; linked repo rows are intentionally preserved; external accessible/user-installation listings remain unchanged. The repo's derived read_write_app_installed flag should clear: known divergence described in the finding. Relink restores the default status setting. |
| LinkGitHubRepo | GetLinkedGitHubRepos gains canonical URL, default-workflow=false and read-write installation flag; GetAccessibleGitHubRepos and legacy GetRepos remain GitHub authorization listings, not local link listings. Unlink/relink creates fresh default settings. |
| UpdateGitHubRepoSettings | GetLinkedGitHubRepos reports changed use_default_workflow_config; all other state is preserved. Toggle true/false and repeat identical updates. |
| UnlinkGitHubRepo | GetLinkedGitHubRepos loses the row; accessible repos, GetRepos, installation lists and account flags remain unchanged. |

GetGroup is also checked on each step using the valid public URL-identifier
lookup: it exposes public org identity, **no GitHub link flag**. The org account
flag is exposed by GetUser's nested Group messages. Group-ID GetGroup lookup is
server-admin-only and is not required for org-member state reads.

The proxy does not register BuildBuddyService or expose GitHub link state;
`enterprise/server/cmd/cache_proxy/cache_proxy.go:382` registers cache services.
Therefore there is no app/proxy GitHub state replication contract to test. The
`bb` CLI has no GitHub link reader. Codesearch Index and its separately configured
backend are excluded as requested. GitHub repository-content mutations (tree,
blob, commit, refs, PRs, CreateRepo) are outside this item: they mutate GitHub-side
content rather than BuildBuddy account/installation/repository link records.

## Source references (released source checkout)

- `proto/buildbuddy_service.proto:204-233`, `proto/github.proto`: account,
  installation and repo link RPC contracts; the repository flag explicitly means
  an app installed **and linked to the org**.
- `server/buildbuddy_server/buildbuddy_server.go:2146-2338`: mutation dispatch;
  `GetUser` assembles user and nested org flags; `GetGroup` returns public identity.
- `enterprise/server/githubapp/githubapp.go:250-293,706-1058`: local link
  listings, mutations and GitHub authorization readers.
- `enterprise/server/githubapp/githubapp.go:1574-1640`: GitHub user/installations
  readers and token validation.
- `enterprise/server/workflow/service/service.go:988-1044`: legacy GetRepos uses
  the org OAuth token and lists GitHub repos, independent of local repo linking.
- `server/backends/github/github.go:316-419`: real OAuth callback links the
  disposable mock token through the public handler, without SQLite writes.

## Fixture and bounded convergence

A test-owned CONNECT proxy accepts only github.com:443 and api.github.com:443,
then tunnels to a local TLS mock. It never resolves or connects to the requested
external host. The app receives HTTPS_PROXY and a mounted generated CA; handlers
use fixed realistic account, installation and repository responses. This also
contains clients which hard-code public GitHub URLs (legacy GetRepos and token
validation). The browser initiates the real OAuth flow, retains state cookies,
and returns a fixture code to the real callback without following external
redirects. Unexpected GitHub routes fail the test. Unrelated image startup
OAuth discovery attempts are blocked by the proxy.

The app uses harness self-auth. gRPC uses its supported signed JWT mechanism,
with the fixture-only HMAC secret and the same real provisioned user/org-admin
role as the browser; two separately provisioned org-level ORG_ADMIN API keys perform installation
updates, repo-setting updates and repo deletion. Both keys independently read
repo and workflow state after every step. User-required mutations alternate
between two gRPC connections; HTTP uses the same real user via cookies.
No host credential is read or forwarded. All RPCs and HTTP requests have a maximum 5s deadline nested in a single
30s convergence context (including JSON calls). After the first complete
matching snapshot, three more complete snapshots must match on a 100ms ticker.
Each step logs the observed convergence upper bound and stability duration. Harness saves image metadata,
configs and container logs.

## Run

```sh
bb remote test //tests/statemachine/github-link-state:go_default_test --test_output=errors
bb remote test //tests/statemachine/github-link-state:installation_flag_bug_test --test_tag_filters=bug --test_filter=TestUnlinkInstallationClearsRepositoryFlag --nocache_test_results --test_output=errors
```

The default target asserts all transitions, explicitly recording the released
stale derived flag as a known divergence. The bug-tagged target requires the
flag's documented semantics and fails on the released image; default CI excludes
that target. The finding lives under this item directory because the task
restricts all edits to this directory. No root findings INDEX changes are made.

Validation: [normal target passed](https://app.buildbuddy.io/invocation/d676ee78-3494-4b5c-98b0-b257e57c3068)
with uncached test execution. The finding records both independent bug reproduction
invocations, each failing on the same derived repository flag after installation
unlink.


## Round 2: every-step browser inventory and limits

The local audit branch's GetGithubRepo and GetGitHubAppInstallPath checks are
included with original expectations. No perturbations are retained.
Every row in the mutation table also checks ALL of these readers over gRPC,
HTTP protobuf and HTTP JSON after every mutation and OAuth callback:

- GetGithubRepo (default branch) and GetGitHubAppInstallPath (exact path or
  documented account-not-linked denial).
- GetGithubContent (downloaded bytes), GetGithubBlob (raw bytes), GetGithubTree
  (tree SHA), GetGithubCompare (ahead count), GetGithubForks (exact fork owner),
  GetGithubCommits (exact commit SHA, tree SHA and message).
- GetGithubPullRequest (empty dashboard) and GetGithubPullRequestDetails
  (exact PR ID). These run against test-local REST and GraphQL endpoints.
- GetWorkflows (empty legacy list), GetWorkflowHistory (the precise
  `database not configured` prerequisite on this SQLite-only fixture).

Positive reads compare gRPC and HTTP protobuf, then independently assert JSON
model values. When user authorization disappears, every token-dependent reader
must fail with an account/token-specific denial, rather than accepting any error.
The mock verifies Authorization presence and the fixture user token on user REST
and GraphQL calls. It does not simulate remote token revocation: unlinking a
BuildBuddy record is not a GitHub-side revocation operation. All mutation warning
fields are asserted empty. The initial, unrelated org's account flag must stay
false throughout. Deprecated `GetLinkedGitHubRepos.repo_urls` is explicitly
asserted empty in this release; source deliberately only populates `repos`, and
no reviewed frontend consumes the deprecated field. Its omission is not claimed
as a new bug.

Released-source inventory:

| Surface | Source and applicability |
| --- | --- |
| Integration settings | enterprise/app/settings/{github_link,user_github_link}.tsx: installation, path, user readers |
| Code editor | enterprise/app/code/{code,code_v2,code_pull_request}.tsx/.ts: repo, content, tree, blob, compare, forks, commits |
| Review UI | enterprise/app/review/{review_list,view_pull_request,file_content_service}: dashboard, details, compare, content |
| Workflow UI | enterprise/app/workflows/{github_app_import,workflows}.tsx: user/install/path and legacy/history readers |
| HTTP routing | server/libmain/libmain.go:451-465: JSON/protobuf protolet and `/file/download`; the latter downloads invocation/cache artifacts, not link records |
| Streaming/raw logs | proto/buildbuddy_service.proto: no GitHub link streaming RPC; invocation logs/raw-event JSON expose recorded build data, not mutable account links |
| CLI | cli/ source contains no GetGithub/GetGitHub/GetLinkedGitHub reader |
| Cache proxy | enterprise/server/cmd/cache_proxy/cache_proxy.go registers cache APIs, not BuildBuddyService; no applicable mutation or replicated GitHub reader |

**Coverage limits, not passing deletion claims:** this release has no public
legacy-workflow creation API (proto service and workflow service contain listing,
deletion and execution but no insertion path). Creating a legacy row would
require database seeding outside the valid-input public state machine. Empty
GetWorkflows checks cannot establish deletion of pre-existing migrated rows.
Functional GetWorkflowHistory needs an OLAP database; its absence is explicitly
asserted, not silently accepted as any error. Coordinator follow-up: an OLAP
fixture and a supported migration/legacy-row setup would enable these two
additional checks. No shared file changes are made here.

The isolated bug target now executes only account setup, installation link,
repo link and installation unlink; it does not depend on setting toggles or
unrelated deletions. Its 30s observation bound distinguishes the permanent
source-derived stale flag from a transient eventual-consistency delay.

Round-2 validation: [expanded suite](https://app.buildbuddy.io/invocation/8b0064f8-2ee6-4b9c-baf8-782925410c44)
passes all 25 initial/OAuth/mutation steps. First complete matching snapshots
were 62.7–113.0ms after starting each read sweep, with 287.8–318.0ms of subsequent
stable observations. These are conservative observation upper bounds including
RPC sweep time, not measured asynchronous replication latency. No transient
mismatch was observed. [Final suite](https://app.buildbuddy.io/invocation/56620077-467f-44ea-a6fe-9234f4550c80)
also passes with the bounded-error diagnostic and all final readers.
The finding records two independent round-2 isolated reproductions.
