# Warmed proxy readers contradict missing-blob lookup after app deletion

Title: Warmed cache proxy serves CAS bytes which its FindMissingBlobs reports missing
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0
- app: gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
- proxy: gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: An explicit administrative app deletion does not remove warmed proxy bytes, and clients see contradictory presence on the same proxy. Requires a previously cached blob and the non-default app deletion API flag. No security isolation claim is made.
Status: confirmed
Repro test target: //tests/statemachine/public-cache-delete:warmed_proxy_delete
BuildBuddy invocations:
- https://app.buildbuddy.io/invocation/64c919c6-9255-49de-b72c-b73304a5483e
- https://app.buildbuddy.io/invocation/22a83021-2d2a-47d2-bf60-c157c7522888

## Expected behavior

Under the requested cross-surface conformance contract, CAS deletion should eventually produce coherent missing state across app and proxy. Independently, the proxy's missing-blob lookup and byte readers should agree on whether the same digest is available. Remote deletion invalidation is not established as a documented product guarantee.

## Actual behavior

Two independent fixtures upload to app, read twice through proxy, delete using app API, then sample both fixtures at 0, 5, 30, and 60 seconds. App ByteStream and batch readers return NotFound. Both app and proxy FindMissingBlobs identify the requested digest as missing. Proxy ByteStream and batch reads return OK and exact original bytes at every sample. Desired convergence assertion fails at 60 seconds in both runs.

## Logs excerpt

```text
after=1m0s surface=app missing=1 batch_status=5
after=1m0s surface=proxy missing=1 batch_status=0
after=1m0s app_read=NotFound proxy_read=OK proxy_bytes="audit warmed proxy deletion payload"
warmed proxy still serves app-deleted CAS after 60 seconds
```

Final exact repro source was also run twice in:
https://app.buildbuddy.io/invocation/020a85fa-8e09-4424-a91e-754e7b3bc6c9
Both run_1_of_2 and run_2_of_2 failed the same 60-second assertion. The four
normal state-machine tests passed in that combined invocation.

## Notes

Observed duration: >=60 seconds, with no convergence during the observation window. Permanence is unknown. Non-default config: app --enable_cache_delete_api=true, necessary for the mutation; otherwise default harness app/proxy configuration and pinned images. The same payload and digest are checked before deletion, the app confirms deletion, and the proxy's missing lookup also confirms remote absence. Source enterprise/server/{byte_stream_server_proxy,content_addressable_storage_server_proxy} serves local read hits without validating remote presence, whereas missing lookup forwards remote state. No fault injection, cloud component requests, or shared credentials. Bug-tagged target preserves the desired expectation and is excluded from normal CI. The retained audit independently observed the same behavior twice.
