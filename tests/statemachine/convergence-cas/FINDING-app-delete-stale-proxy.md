# App CAS deletion leaves hydrated proxy content readable

Title: DeleteFile and proxy read surfaces disagree after successful deletion
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0
- app: gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
- proxy: gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: medium
Severity justification: An explicitly enabled successful cache deletion does not remove a cached copy exposed by another fixture-local peer. Presence and content RPCs give contradictory answers.
Status: confirmed
Repro test target: //tests/statemachine/convergence-cas:delete_convergence_repro
BuildBuddy invocations:
- https://app.buildbuddy.io/invocation/efab68b5-b27e-4cea-9a9a-c140e8fd8b61
- https://app.buildbuddy.io/invocation/330b4f32-8f55-4419-ab4d-3d6acd592748

## Expected behavior

After an authorized DeleteFile of a valid CAS URI, app and proxy content reads
should eventually agree with absence reported by FindMissingBlobs.

## Actual behavior

Upload through proxy, verify bytes at both peers, delete through app. Both
FindMissingBlobs calls report absence (the proxy forwards this RPC by default),
but proxy content reads do not converge to NotFound within the 20-second
observation window. The test polls presence, ByteStream.Read and BatchReadBlobs,
requiring four consecutive matching scans.

Reproduced independently twice: mismatch persisted for 20.075132855s and
20.087167517s. This is persistent over the measured window, not a measured
transient. No assertion of infinite duration is made. Source explains the lack
of active convergence: enterprise/server/api/api_server.go DeleteFile invokes
only the app's env.GetCache().Delete; proxy content RPCs can serve their local
cache, while FindMissingBlobs forwards to the backing app with the default TTL.
An eventual eviction could remove the proxy copy; there is no invalidation
request in this path.

## Logs excerpt

```text
app delete did not converge to hydrated proxy within 20.075132855s; presence + ByteStream + batch polling
app delete did not converge to hydrated proxy within 20.087167517s; presence + ByteStream + batch polling
```

## Notes

Non-default app flag: --enable_cache_delete_api=true (otherwise the RPC is
disabled, not a valid deletion). Proxy configuration is default; no fault
injection, remote peers, authentication shortcuts, or shared deployment.
The fixture has one org and disposable authorized key. Neither peer contains
an unrelated copy of the test digest. The final version logs both peers'
content status and finishes a valid app recreation before reporting the
deletion mismatch.

Run explicitly:
bb remote test //tests/statemachine/convergence-cas:delete_convergence_repro --test_tag_filters=bug --nocache_test_results --test_output=errors

The bug tag keeps default CI green. This finding remains in the owning
directory per the round-2 task; no generated findings index or upstream report.

Third reproduction (with final snapshot and successful recreation):
https://app.buildbuddy.io/invocation/ca355113-fa11-4958-b438-105879540b77

```text
final peer=app stream_code=NotFound stream_bytes=0 ... status:{code:5}
final peer=proxy stream_code=OK stream_bytes=45 ... data:"delete convergence isolated valid CAS payload" status:{}
app delete did not converge to hydrated proxy within 20.025204156s
```
