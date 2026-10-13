# Distributed-cache peer state transitions

Run `bb remote test //tests/statemachine/distributed-cache-state:go_default_test --nocache_test_results --test_output=all`.

The fixture publishes peer port 1988 explicitly and configures a static one-node
app ring (`app:1988`, replication factor 1). The proxy forwards to that app.
All RPCs use fixture-local keys and addresses. The app distributed lookaside
cache is disabled so the public reads measure the backing peer state.

| Mutation | Sequence | Read surfaces |
| --- | --- | --- |
| DistributedCache.Write | two-message stream, repeated CAS/AC write, AC replacement, recreate after delete | peer Metadata, Read, FindMissing, GetMulti, GetWithMetadata; app UI GetCacheMetadata via gRPC and authenticated JSON HTTP; app/proxy CAS FindMissingBlobs, BatchReadBlobs and ByteStream.Read; app/proxy AC GetActionResult |
| DistributedCache.Delete | delete then repeat delete (success or NOT_FOUND) | the same surfaces, with missing-state expectations |
| DistributedCache.Heartbeat | repeated valid source matching the static node | empty successful response; all cache projections remain unchanged |

The reference model is an absent/present byte register per resource, updated
only after the mutation completes. CAS is immutable: replacement uses a new key
in real workflows; the test repeats identical bytes. AC is mutable and uses two
valid serialized ActionResults. API-added AC metadata is allowed; exit code and
inline stdout must match. Initial public metadata availability is polled with a
ten-second bound before the sequence; mutations on this one-node ring commit
synchronously, so subsequent checks are immediate.

## Expected divergence and scope limits

The proxy's immutable CAS read-through copy survives upstream deletion. It has
no peer-delete invalidation channel or bounded convergence guarantee. Proxy
FindMissingBlobs is authoritative with its default zero TTL, but ByteStream and
BatchRead may return the previously cached immutable bytes after deletion. The
test explicitly accepts only those original bytes or NOT_FOUND, never arbitrary
content. This is an architectural divergence, not a confirmed released bug; the
coverage target is untagged. AC reads use default backend revalidation and must
agree after replacement and deletion.

This topology exercises the peer listener and distributed public projections,
but not multi-app replication. The harness accepts one instance per component;
additional independent apps would need coordinated auth/database and network
configuration. No shared harness files are modified.

Heartbeat updates private peer-zone routing information only when a zone header
is supplied. Neither its empty response nor any public read RPC exposes that
map; cache data must not change. References to GCS and hinted handoffs are outside
this fixture's configured storage/topology. `bb download` is checked after every CAS transition against the app
when the runner provides `bb`; otherwise the test logs that
CLI coverage is unavailable and checks ByteStream directly.

Source enumeration at v2.314.0:

- `proto/distributed_cache.proto`: all three mutators and all five read RPCs.
- `proto/resource.proto`: CAS/AC isolation and resource field numbers.
- `enterprise/server/util/distributed_client/distributed_client.go`: peer RPCs
  operate on backing storage, Delete propagates missing status, Heartbeat has an
  empty reply and invokes its callback.
- `enterprise/server/backends/distributed/distributed.go`: static nodes,
  distributed public read routing, private `recvHeartbeatCallback` peer-zone map.
- `server/cache_server/cache_server.go`, `proto/cache.proto`,
  `proto/buildbuddy_service.proto`: GetCacheMetadata UI API.
- `enterprise/server/content_addressable_storage_server_proxy/`: authoritative
  FindMissing defaults and immutable local read-through CAS.
- `enterprise/server/action_cache_server_proxy/`: default backend revalidation.
- `cli/download/download.go`: bb download delegates to ByteStream.Read.

Wire projections are defined inline with the released field numbers because
this repo does not depend on BuildBuddy's generated internal protos. No product
source is compiled. App image is
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`;
proxy image is
`gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
(both enterprise-v2.314.0). Harness outputs preserve exact configs and logs.

Validation: [initial green run](https://app.buildbuddy.io/invocation/ed430de3-0a3c-40f7-b247-89fda3cb493b)
and [final green run including bb download after every CAS transition](https://app.buildbuddy.io/invocation/7d1b175d-f7b4-4250-95aa-9f4ab85a4cd5).
The final runner provided `bb`, so CLI coverage ran successfully rather than
being bypassed. No confirmed product defect was found.
