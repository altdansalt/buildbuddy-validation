# Deleted directory remains visible through cached trees

Title: CAS deletion does not invalidate derived GetTree and directory-size results
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0
- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: An authorized deletion leaves derived directory listings and sizes inconsistent with backing CAS. This is stale metadata, not evidence of another organization's access or data corruption.
Status: confirmed
Repro test target: //tests/statemachine/convergence-deletes:deleted_tree_repro
BuildBuddy invocations:
- https://app.buildbuddy.io/invocation/14bb4cc2-736c-4d10-a8b6-04f0b3fb70b8
- https://app.buildbuddy.io/invocation/1ca64732-b65e-4402-ad8b-b681a38acc5f

## Expected behavior

After deleting a child directory, direct CAS reads and derived listings should agree that the child is unavailable. REAPI permits partial trees when a descendant is missing; the regression assertion requires only that returned directory bodies do not expose the deleted child, rather than requiring a particular error or complete partial tree.

## Actual behavior

Upload a valid five-directory hierarchy (all referenced file content exists), warm app and proxy GetTree, wait for asynchronous tree-cache finalization, then delete the leaf using the app's public DeleteFile gRPC API. App ByteStream.Read returns NotFound and FindMissingBlobs returns the exact deleted digest. Nevertheless app/proxy REAPI GetTree keep returning all five directories throughout five seconds of 250ms polling. Decoded frontend GetTree gRPC and its real length-prefixed HTTP endpoint also contain the deleted directory. GetTreeDirectorySizes still returns five size entries.

Both invocations reproduced this in two independent disposable fixtures. In the first the observation durations were 5.061628126s and 5.063603581s; in the second 5.064879087s and 5.065626709s. Convergence was not observed. This establishes retention for at least five seconds, **not permanent retention or a measured eviction lifetime**. Repeated reads may maintain cache residency.

The normal `TestDirectoryStateMachine` disables tree caching and checks create, repeated writes, delete, repeated absent delete, and re-create across the same tree/size surfaces. Historical cache-disabled audit control: https://app.buildbuddy.io/invocation/267be873-ff3c-4d30-8e40-765992355b16. This rules out an unsuccessful delete and identifies derived tree caching as the cause.

## Logs excerpt

```text
deleted child still exposed after 5.064879087s app=true proxy=true
frontend GetTree after deletion: messages=1 terminal=EOF
frontend deleted child gRPC=true HTTP=true; directory sizes still include all five
GetTree exposed a deleted child
```

## Notes

Non-default flags: `--enable_cache_delete_api=true` enables the deletion API; `--cache.tree_cache_write_probability=1` removes probabilistic sampling. Tree caching itself is enabled by default, with write probability 0.01. The repro does not establish the frequency under default sampling. The five-directory chain meets the released tree-cache eligibility threshold. No faults, eviction manipulation, external deployments, or source-built components are used.

Source anchors: `proto/{cache,remote_execution}.proto`, `server/remote_cache/content_addressable_storage_server/` (cached tree lookup/write), `server/remote_cache/directory_size/`, `server/buildbuddy_server/buildbuddy_server.go`, `app/util/get_tree.ts`, `app/invocation/invocation_action_card.tsx`, `app/service/rpc_service.ts`, and `server/http/protolet/`.

The target is tagged `bug` and intentionally fails the desired invariant. Default CI excludes it. Findings stay in this directory as requested; no upstream report or PR was opened.
