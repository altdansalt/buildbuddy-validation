# App deletion leaves a warmed cache proxy serving the deleted blob

Title: DeleteFile does not converge with warmed proxy read surfaces
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0
- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: An opt-in administrative deletion leaves cached data readable through an already-warmed proxy. This violates the requested deletion/convergence model, but CAS is immutable and no new cross-org or confidentiality boundary failure is claimed.
Status: confirmed
Repro test target: //tests/statemachine/convergence-streams:delete_convergence_bug
BuildBuddy invocation: https://app.buildbuddy.io/invocation/1ec15f22-09c0-4419-bc39-cf55fe8ce6c9
Second invocation: https://app.buildbuddy.io/invocation/29045d85-fceb-4d11-aa01-67e88ff7dda8
Corrected expanded surface invocation: https://app.buildbuddy.io/invocation/1c0bd2ac-051e-4d4b-8b92-f1aa5825edaf
Repeated corrected invocation: https://app.buildbuddy.io/invocation/b9727dc4-3e31-49ca-bf44-2b4417e65cb0

## Expected behavior

After a successful api.v1.DeleteFile at the app, the app and proxy eventually expose absence of the digest on all read surfaces. A subsequent valid ByteStream.Write re-creates the same immutable content on both endpoints. Enable the supported delete API with `--enable_cache_delete_api=true`; it is disabled by default.

## Actual behavior

Create via app, verify all present-state surfaces (warming the proxy), delete via app, and poll. App ByteStream reports NotFound; proxy ByteStream still returns the blob. The first isolated run observed disagreement for 5.047773453 s. The repeated run observed 30.086332587 s and 30.057439177 s, including re-creation via proxy between deletions. The expanded probe observed 30.931157908 s and 30.813922943 s: proxy identity/zstd ByteStream, BatchReadBlobs, direct metadata, and bb download still expose presence. The final corrected repeat measured 31.449796616 s and 30.795196342 s with only proxy surfaces disagreeing; re-create controls and final restored-state stability passed. Proxy FindMissingBlobs forwards absence from the app, disagreeing with its own local reads.

This is persistent (>30 s measured), not an observed transient. Permanent until local eviction/replacement is the implementation explanation, not an experimentally proven infinite retention claim. The released app DeleteFile calls its own cache.Delete and does not invalidate proxy-local storage. Default proxy reads serve local hits. No invalidation protocol or bounded refresh was found on these paths. The only non-default setting is the supported app delete flag; there is no fault injection or eviction/isolation misconfiguration.

## Logs excerpt

```text
delete did not converge within 30.086332587s: endpoint 1 retained deleted blob (RPC error=<nil>)
delete did not converge within 30.057439177s: endpoint 1 retained deleted blob (RPC error=<nil>)
endpoint1 ByteStream zstd=false expected NotFound: <nil>
endpoint1 ByteStream zstd=true expected NotFound: <nil>
endpoint1 BatchRead retained data/status
endpoint1 metadata expected NotFound: <nil>
endpoint1 CLI missing-state disagrees: <nil>
```

## Notes

All reads use fixture-local containers and disposable keys. Every present-state control passes before the deletion, so the disagreement is not failed provisioning/auth or missing upload. The expanded missing-state probe also exposed differences in error presentation, which are not additional findings: pooled GetFile maps failed retrieval to Unavailable; REST GetFile returns 404; UI CAS downloads try invocation blobstore fallback and return a missing-invocation error when no invocation is associated. A corrected probe sends only the GetFile schema's uri field (the earlier expanded invocation sent extra range fields to that method); those earlier GetFile InvalidArgument errors are excluded from defect evidence.

Reproduce with `bb remote test //tests/statemachine/convergence-streams:delete_convergence_bug --test_tag_filters=bug --nocache_test_results --test_output=errors`. The target asserts the desired absent-state model and intentionally fails on released images. It continues to the re-create transitions after reporting bounded deletion failures, and ends by restoring presence through the app and checking stability. No shared harness/dependency changes or PR are needed.
