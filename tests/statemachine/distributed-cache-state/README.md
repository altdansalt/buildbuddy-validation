# Distributed cache state and surface consistency

Run the green suite with:

```sh
bb remote test //tests/statemachine/distributed-cache-state/... --nocache_test_results --test_output=errors
```

Run the intentionally failing deletion conformance repro with:

```sh
bb remote test //tests/statemachine/distributed-cache-state:proxy_delete_conformance --test_tag_filters=bug --nocache_test_results --test_output=errors
```

The round-1 audit's valid BES invocation and HTTP download probe are incorporated. All intentional expectation perturbations were removed. A reference byte register is checked after every peer Write/Delete/Heartbeat and public upload/AC update, using empty and nested instance names and two independently provisioned keys in the same org. Sequences cover create, repeated write, AC replacement, three public updates using proxy/second app client/app, repeated zone-bearing Heartbeat, delete, repeated delete, and recreate. CAS replacement respects immutability and repeats the same valid Directory bytes; AC fixtures include real CAS output references, tree references, symlinks, executable flags, stderr and execution metadata.

| Mutating RPC | Surfaces checked after each step | Result |
| --- | --- | --- |
| DistributedCache.Write | Peer FindMissing/GetMulti identities, Metadata/GetWithMetadata sizes and modify timestamps, access timestamp positivity/order, full and ranged streaming Read; app GetCacheMetadata gRPC/JSON sizes and modify timestamp; app/proxy CAS existence/batch/full, ranged and ZSTD ByteStream/GetTree or full AC GetActionResult; UI download/view; CAS GetFile streaming through two keys, GetFileRange gRPC/HTTP, UI GetTree gRPC/browser-prefixed HTTP and GetTreeDirectorySizes gRPC/JSON/browser-prefixed HTTP; mandatory bb download through app/proxy | Agreement, with exact known absence translations |
| DistributedCache.Delete | Same projections; repeat delete and recreate | App and authoritative existence absent; warmed proxy CAS bytes persist, isolated in finding/repro |
| DistributedCache.Heartbeat | Same byte/metadata projections after each zone-bearing heartbeat | Data unchanged; private routing map has no external read oracle |
| ByteStream.Write via app/proxy/second client | Same projections for immutable CAS rewrites | Agreement |
| ActionCache.UpdateActionResult via app/proxy/second client | Same peer/UI/AC/download/view projections; compares all ActionResult fields | Agreement |
| ApiService.DeleteFile | Isolated app ByteStream/proxy existence/proxy ByteStream/BatchRead; replicated sequence also checks both peers | Retention divergence in independent proxy topology |

Each state-machine step polls the complete applicable observer set for at most 10 seconds at 100ms intervals, then checks it again after 100ms. Logs report the elapsed observation bound (RPC/CLI traversal time included), not an exact replication latency. Browser-prefixed HTTP metadata/tree/size checks validate both data frames and terminal gRPC status, matching `rpc_service.ts`; HTTP 200 alone is insufficient. The known proxy CAS retention is explicitly outside the agreement oracle and gets its own bounded observation and strict `bug` target. No catch-all transport/auth failures are accepted. CLI availability is mandatory.

`TestReplicatedPublicSequence` configures app and proxy as two distributed peers with replication factor 2 and disabled lookaside caching. Public mutations own replica fanout; peer RPCs address individual backing replicas. It checks both peer GetWithMetadata projections and app/proxy existence/ByteStream after every write, delete, recreate and zone-bearing heartbeat. It uses an explicit shared fixture-only HMAC key, a locally minted JWT, HS256 remote auth, and `auth.reparse_jwts=true` for the cluster. Initial internal writer readiness is bounded; repeated uploads use identical valid CAS bytes. No worker credentials are used.

## Released-source surface enumeration

Inspected locally at v2.314.0; no BuildBuddy source is built:

- `proto/distributed_cache.proto`, `enterprise/server/util/distributed_client/distributed_client.go`: three peer mutators, five peer reads, ranges, metadata and key projections.
- `enterprise/server/backends/distributed/distributed.go`: replication fanout and Heartbeat's private `peerZones` routing map. Zone headers now accompany heartbeats; cache observations do not prove the private map's exact contents.
- `proto/cache.proto`, `proto/buildbuddy_service.proto`, `server/buildbuddy_server/buildbuddy_server.go`: UI metadata, streaming GetTree, directory sizes and download/view handlers.
- `app/service/rpc_service.ts` and `app/invocation/invocation_action_card.tsx`: the UI loads action results, directories, stdout/stderr/log CAS blobs via `/file/download`; `/file/view` shares its handler. Exact bytes cover text/protobuf views of the same resources.
- `proto/api/v1/file.proto`, `enterprise/server/api/api_server.go`: GetFile streaming, GetFileRange gRPC and `/api/v1/GetFileRange`, DeleteFile's opt-in flag. GetFileRange is CAS-only; no invalid AC range requests are sent.
- `server/remote_cache/byte_stream_client/byte_stream_client.go`: GetFile deliberately sanitizes missing reads to UNAVAILABLE. JSON metadata embeds NotFound in HTTP 500. GetTree's missing-root helper returns FAILED_PRECONDITION. Tests check these specific absence representations rather than accepting arbitrary errors.
- `enterprise/server/content_addressable_storage_server_proxy/` and `enterprise/server/action_cache_server_proxy/`: immutable CAS retention and default AC revalidation; proxy GetTree defaults to authoritative forwarding.
- `cli/download/download.go`: raw CAS downloads through ByteStream. The CLI does not expose a corresponding AC download command; AC is checked through its applicable RPC/UI projections.

Historical invocation raw JSON, build logs, scorecards and execution-download metadata are not current-cache-presence oracles: deleting a CAS blob does not rewrite BES records or historical hit records. QueryWriteStatus is UNIMPLEMENTED in this release. Capabilities are static rather than blob state. These were enumerated, not used as misleading current-state assertions.

Coverage boundaries: this remains a deterministic SHA256/identity-write fixture with ZSTD read transcoding, not every compressor/digest algorithm, pagination, tree shape or randomized sequence. The main tree is a valid leaf Directory; the separate replicated topology has narrower observer coverage than the main state machine. Access times can advance during reads and are checked for valid ordering, not equality across sequential reads. Heartbeat's routing contents are not directly externally observable. No blanket skip hides these boundaries.

See [the confirmed retention finding](FINDING-cacheproxy-delete-retention.md) for exact image digests, repeated reproduction evidence, configuration and duration. All images are default digest-pinned enterprise-v2.314.0. Fixtures retain exact configs/container logs in undeclared outputs. Only this directory is modified; no PR is opened.

Validation before final area run: [main suite](https://app.buildbuddy.io/invocation/c2b5de45-8d07-4cbb-a14a-cb11a5be22dd), [two-peer sequence](https://app.buildbuddy.io/invocation/0328d3cc-a094-42f4-80d1-93ead8dcd5d7).

Final area validation: [both coverage targets passed](https://app.buildbuddy.io/invocation/06da171c-3a4f-41d0-b213-81f8ac82e542). The preceding complete run [also passed](https://app.buildbuddy.io/invocation/d13619e7-d390-43c5-b5fc-993162bf280a), observing the modeled projections in 109–676ms and replica projections in 104–145ms, followed by a stability recheck. These are observation bounds including polling/RPC work. Proxy deletion retention persisted for 10.030544798s in the final area run.

Final code validation, including ranged/ZSTD ByteStream reads and missing-digest identities: [both coverage targets passed](https://app.buildbuddy.io/invocation/08417944-f1c8-474d-ab83-c5feb46db0ec). Proxy retention was stable for 10.026106502s.
