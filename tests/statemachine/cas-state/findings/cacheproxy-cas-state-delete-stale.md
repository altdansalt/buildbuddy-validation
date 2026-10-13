# App DeleteFile leaves warmed proxy CAS bytes readable

Title: App CAS deletion disagrees with proxy ByteStream.Read and FindMissingBlobs
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: medium
Severity justification: An authenticated operator's successful cache deletion leaves the artifact retrievable through a warmed proxy; proxy existence and content surfaces contradict each other. Requires enabling the non-default deletion API and a resident proxy copy. This is a cache consistency finding, not a claim of cross-org access.
Status: confirmed
Repro test target: //tests/statemachine/cas-state:delete_state_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/644b328c-956b-4bcf-af3c-95076fca6555
Second reproduction: https://app.buildbuddy.io/invocation/bcd15930-c0b5-45a1-8572-63cb8e89f7a3

## Expected behavior

Under this validation suite's cross-component consistency requirement, successful
`api.v1.ApiService.DeleteFile` must eventually make the digest missing on all read
surfaces, including warmed proxies. `FindMissingBlobs` and `ByteStream.Read` should
agree about the same instance/digest at the same time.

## Actual behavior

A valid proxy BatchUpdateBlobs writes one SHA256 blob, both components read its
exact bytes, then the app successfully deletes the blob. Both components'
FindMissingBlobs immediately report that exact digest missing and app Read returns
NotFound. Proxy Read keeps returning the exact original bytes throughout 5.079641336s
and 5.102154545s observation windows on independent fixtures. No convergence was
observed. This is persistent while the local proxy copy remains resident, rather
than a measured transient delay; tests do not establish an infinite lifetime.
Eviction or local removal could end it.

The source explains the behavior: app `enterprise/server/api/api_server.go`
DeleteFile deletes only `env.GetCache()`, with no proxy invalidation. Proxy
`enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go` Read uses
local cached content. The default proxy FindMissingBlobs consults the remote app.
The minimal test uses a single fixture/group/key, identical resource names, and
requires successful write/delete responses and exact deleted bytes. There are no
external deployments, fault injections, eviction pressure, or auth-negative inputs.

## Logs excerpt

```text
divergence persisted 5.079641336s: app Read missing; app and proxy FindMissingBlobs missing; proxy Read returns exact deleted bytes "delete and recreate this immutable CAS payload"
divergence persisted 5.102154545s: app Read missing; app and proxy FindMissingBlobs missing; proxy Read returns exact deleted bytes "delete and recreate this immutable CAS payload"
```

## Notes

Only non-default setting: app `--enable_cache_delete_api=true`, required for valid
DeleteFile access. Proxy settings, pinned images, storage, auth and connectivity
are fixture defaults. This documents a divergence from the run's consistency goal;
the delete API does not document a distributed proxy invalidation guarantee.

The target is tagged `bug` and excluded from normal CI. Explicit reproduction:

```sh
bb remote test //tests/statemachine/cas-state:delete_state_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

An earlier exploratory sequence reproduced the same read-after-delete symptom:
https://app.buildbuddy.io/invocation/7d4de812-ba4a-4c8d-9f4c-0f1a906441b9 .
An intermediate test assertion compared protobuf internal size caches and failed
before measuring the symptom; that run is not counted as reproduction evidence.
No upstream report or PR was opened. No shared harness changes are needed.
