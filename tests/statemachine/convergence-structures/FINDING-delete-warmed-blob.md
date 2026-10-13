# App deletion leaves proxy ByteStream serving a blob reported missing

Title: App DeleteFile does not invalidate warmed proxy ByteStream state
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: medium
Severity justification: An enabled cache-delete API removes the backing object but clients can still retrieve deleted data from a warmed proxy. This is a state consistency failure, not evidence of an authentication bypass.
Status: confirmed
Repro test target: //tests/statemachine/convergence-structures:delete_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/23388f11-8cae-4384-aa46-ef976037ea86

## Expected behavior

After acknowledged DeleteFile, app and proxy read/presence surfaces eventually agree that the digest is absent. ByteStream.Read must not return the deleted payload while the same proxy's FindMissingBlobs reports it missing.

## Actual behavior

A 25-byte blob is uploaded through the proxy and successfully read from both endpoints. DeleteFile succeeds on the app. App reads return NotFound. Proxy FindMissingBlobs also reports the exact digest missing, while proxy ByteStream.Read repeatedly returns OK and the original 25 bytes.

Reproduced independently on the released containers twice:
- https://app.buildbuddy.io/invocation/23388f11-8cae-4384-aa46-ef976037ea86 — no convergence for 30.005832839 seconds.
- https://app.buildbuddy.io/invocation/063c1e1f-c281-41c2-bfcd-8dbfc1b45e6d — no convergence for 30.122523275 seconds.

Persistence: sustained for the complete bounded observation windows; no transient convergence time was observed. This is a permanent stale-cache path absent eviction/restart/recreation, based on source inspection, not an experimentally established infinite duration.

## Logs excerpt

```text
deletion failed to converge after 30.122523275s:
side=1 Read code=OK bytes=25 equals_original=true
missing=missing_blob_digests:{hash:"f6a121337a61fed59a805603e75a15142c70bebf056e061eace5fa809a80f357" size_bytes:25}
error=<nil>
```

## Notes

Requires non-default app `--enable_cache_delete_api=true`; proxy configuration is the harness default. Uses valid SHA256 CAS requests and one test-local org/key, no fault injection. Successful pre-deletion reads and successful app deletion rule out auth, digest, networking and fixture errors. Source: `enterprise/server/api/api_server.go` DeleteFile deletes only the app cache; `enterprise/server/byte_stream_server_proxy` serves local cached bytes; default FindMissingBlobs consults the backing cache. There is no deletion invalidation broadcast in this path.

Run explicitly with `bb remote test //tests/statemachine/convergence-structures:delete_repro --test_tag_filters=bug --nocache_test_results --test_output=errors`. Tagged bug, so ordinary CI excludes this expected failure. Finding remains in this directory as requested; no generated index or shared files changed.
