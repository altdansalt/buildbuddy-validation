# Proxy CAS reads retain deleted state

Title: Proxy CAS existence and byte reads disagree after successful upstream deletion
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: Deleted CAS content remains available to authorized clients through a warmed proxy. CAS bytes are immutable, so this affects deletion/existence consistency rather than content integrity. No cross-org access or unauthorized disclosure is established.
Status: confirmed
Repro test target: //tests/statemachine/distributed-cache-state:proxy_delete_conformance
BuildBuddy invocation: https://app.buildbuddy.io/invocation/9da25df8-381b-4bb5-a346-8ad3f36ef6be

## Expected behavior

Under this suite's supplied cross-surface conformance goal, successful deletion should eventually result in absent state in app and proxy existence and byte-read surfaces. This is a conformance divergence; it is not a claim that REAPI specifies a CAS deletion/invalidation protocol.

## Actual behavior

Upload a valid CAS blob to the local app, warm the local proxy with ByteStream.Read, then delete through the app's DeleteFile API. The app ByteStream returns NOT_FOUND and proxy FindMissingBlobs returns the exact missing digest. Proxy ByteStream.Read and BatchReadBlobs still return the exact original bytes with OK.

The strict conformance assertion failed independently twice:
- https://app.buildbuddy.io/invocation/9da25df8-381b-4bb5-a346-8ad3f36ef6be (immediate mismatch)
- https://app.buildbuddy.io/invocation/87b48c88-a9a7-4823-9d94-3e933f06bbde (no convergence within 10 seconds)

The default-suite characterization polls all four projections once per second and requires the mismatch to stay stable for the entire observation window. Independent characterization subtests passed for 10.044774021s and 10.033475515s:
- https://app.buildbuddy.io/invocation/1e2c7340-b0a9-460b-8fb4-45fca5036ad9
- https://app.buildbuddy.io/invocation/9ec426fe-a56d-4c8d-a5fb-539e756b8100

Those two overall invocations failed unrelated checks while the broader suite was being developed; TestProxyDeleteRetention passed in each. All responses above address fixture-local containers. Successful deletion and simultaneous app absence rule out authentication or dispatch failure.

## Logs excerpt

```text
DIVERGENCE: app missing, proxy existence missing, proxy ByteStream/BatchRead present;
stable for 10.044774021s; no convergence within 10s bound
Condition never satisfied: proxy reads deleted bytes while FindMissing reports absent
```

## Notes

Persistent beyond the measured 10-second bound; no transient convergence time was observed. Released `enterprise/server/content_addressable_storage_server_proxy` serves immutable CAS reads from local storage and forwards default FindMissing queries upstream. There is no delete invalidation channel; retention is unbounded until eviction, not a timed convergence mechanism. An infinite lifetime is not experimentally proven.

The isolated repro uses default app/proxy cache topology and enables the otherwise disabled DeleteFile API with `--enable_cache_delete_api=true`. This flag enables the mutator; proxy retention uses default configuration. The peer state-machine also observes the same retained-byte behavior after DistributedCache.Delete with its explicit one-node ring and disabled lookaside cache.

The strict target is tagged `bug` and excluded from the default suite. The untagged TestProxyDeleteRetention deliberately characterizes the known divergence with exact identities/status/bytes; it does not assert eventual agreement. No upstream issue or PR was filed.
