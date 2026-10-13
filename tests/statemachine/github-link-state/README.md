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
role as the browser; user-owned API keys cannot grant org-admin capability.
No host credential is read or forwarded. RPC deadlines are 20s, the browser
client timeout is 30s, and convergence retries use a 30s deadline and 100ms
polling ticker, rather than unconditional sleeps. Harness saves image metadata,
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
