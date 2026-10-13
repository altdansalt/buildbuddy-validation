# Repository listing retains installation flag after installation unlink

Title: GetLinkedGitHubRepos reports a linked read-write installation after unlink
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Authenticated org users receive misleading integration state. Repository rows are supposed to persist, but the derived installation flag misrepresents whether they have a linked app. No privilege escalation or data loss is demonstrated.
Status: confirmed
Repro test target: //tests/statemachine/github-link-state:installation_flag_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/64a99c21-6d21-49e0-803e-77de54cc2cb5
Second reproduction: https://app.buildbuddy.io/invocation/a42378bf-daee-48e9-b407-74526b4d8d15

## Expected behavior

`proto/github.proto` defines `GitRepository.read_write_app_installed` as whether
the repository owner has the read-write GitHub app installed **and linked to the
org**. After successful `UnlinkGitHubAppInstallation`, the installation listing
should be empty and that derived repo flag should be false. As documented by
`UnlinkAppInstallationRequest`, repository rows themselves should be preserved.
GitHub-side authorization is unchanged by unlinking the BuildBuddy-side record.

## Actual behavior

Provision a real local user/org, link a GitHub account via OAuth callbacks,
link installation 101/app 7, link `https://github.com/fixture-owner/project`,
then unlink the installation while keeping the repository row.
`GetGitHubAppInstallations` is immediately empty, but `GetLinkedGitHubRepos`
continues returning `read_write_app_installed=true` over gRPC and HTTP protobuf.
The ordinary coverage target also asserts this released behavior over HTTP JSON.
Both independent reproduction runs fail after bounded 30s convergence polling.

This is not a mock authorization or harness/configuration failure: all preceding
links, toggles, readbacks and unlink/relink operations succeed; the installation
listing proves that the local deletion completed; GitHub authorization correctly
persists. The released source independently explains the difference:
`enterprise/server/githubapp/githubapp.go:280-286` computes the flag solely by
matching the repository's stored AppID to the configured read-write app, without
checking for a remaining GitHubAppInstallations row.

## Logs excerpt

```text
UnlinkGitHubAppInstallation failed to converge: linked repository does not match model:
0a2868747470733a2f2f6769746875622e636f6d2f666978747572652d6f776e65722f70726f6a6563741801
```

The protobuf suffix `18 01` is field 3 (`read_write_app_installed`) set to true.
Before that assertion, the test verifies the installation count is zero and
checks byte-for-byte equality of the gRPC and HTTP protobuf repository responses.

## Notes

The mock supplies fixed valid OAuth/user/installation/repository responses.
Every GitHub TLS request is contained by the test's closed CONNECT proxy and
local TLS server. No shared GitHub or BuildBuddy service is addressed. The app
runs inside a harness-owned container, with SQLite and self-auth. No image
version overrides are used. The gRPC user JWT uses the fixture signing secret
and the same locally provisioned org-admin user as the browser session.

Default coverage explicitly records this known released divergence, allowing CI
to stay green. The `bug`-tagged target requires the documented false flag and
is excluded from default CI; run it explicitly with `--test_tag_filters=bug`,
`--test_filter=TestUnlinkInstallationClearsRepositoryFlag` and
`--nocache_test_results`. Relinking the installation restores consistent state.

This finding uses the repository template but is stored inside the item directory
to comply with the task's edit restriction. Coordinator action: promote it into
root `findings/` and regenerate INDEX if desired. No harness change is needed.
