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

## Round 2 isolated reproduction

The minimal sequence (valid user OAuth link, installation link, repo link,
installation unlink) reproduces independently in:

- https://app.buildbuddy.io/invocation/bfb0b0d0-082d-417d-8b24-7397bc2458b3
- https://app.buildbuddy.io/invocation/30c99860-38d3-428a-8015-836b5aec70e5

Both repeatedly observe the mismatch for the 30s polling bound. Each failed
snapshot first verifies an empty installation listing; repository gRPC and HTTP
protobuf are identical and the JSON response explicitly contains
`readWriteAppInstalled:true`. No mutation warnings occur. The mock verifies user
Authorization headers, and added content/review readers agree before deletion.
The final deadline cancels the last snapshot; it is the preceding repeated
model mismatch, not that cancellation, which establishes the divergence.

```text
UnlinkGitHubAppInstallation snapshot: linked repository does not match model:
0a2868747470733a2f2f6769746875622e636f6d2f666978747572652d6f776e65722f70726f6a6563741801;
HTTP protobuf identical; JSON=map[repos:[map[readWriteAppInstalled:true
repoUrl:https://github.com/fixture-owner/project]]]
```

Classification: permanent derived-state error until installation relink or repo
deletion, not an observed transient delay; no convergence in 30s. The released
query computes the flag from the persisted repo AppID, so waiting cannot update
it. This classification combines bounded observation with source inspection,
not an unbounded timing claim. Non-default config: GitHub App integration must
be enabled and configured (app ID 7, disposable OAuth credentials/signing key,
local TLS mock). Default database/cache/auth harness config and pinned image
are unchanged; no non-default consistency/cache flag is involved.

The ordinary expanded sequence passes and records this known exception:
https://app.buildbuddy.io/invocation/56620077-467f-44ea-a6fe-9234f4550c80.
All other modeled mutations agree on their first complete snapshot and remain
stable for three additional snapshots. Detailed timing is in the verbose test
log. This does not establish functional OLAP history or migrated legacy-row
cleanup coverage; see the README's explicit limits.

A third run on the final diagnostic code also reproduces:
https://app.buildbuddy.io/invocation/dce4f73c-d6ed-4cc0-b454-81e6fc5399b1.
It preserves the product mismatch in the terminal failure instead of replacing
it with the canceled final RPC:

```text
UnlinkGitHubAppInstallation failed to converge within 30.000332089s:
linked repository does not match model: ...1801;
HTTP protobuf identical; JSON=map[repos:[map[readWriteAppInstalled:true ...]]]
```
