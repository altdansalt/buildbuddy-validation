# Encryption state conformance (released enterprise v2.314.0)

The fixture starts only released app/proxy images. Local AES KMS files, signed
client identity, two same-org API keys and web cookies are disposable credentials.
No component traffic addresses a shared deployment.

## Reference model and sequences

Every successful mutation is followed by a full retained-record snapshot.
SetEncryptionConfig runs disable → enable → repeat enable → disable → repeat
disable → re-enable → repeat enable. Each state writes via both mutation APIs,
repeats writes with an intervening snapshot, uploads an executable file Directory and a parent Directory with a nested subtree,
and creates/updates/updates ActionResults using alternating app/proxy clients.
Each ActionResult write is repeated with an intervening read. CAS identities and
complete submitted ActionResult fields are compared to the model; server-added
worker metadata is adopted from the mutation response and compared across reads.

Records are retained, including old directories and ActionResults. Source
`enterprise/server/filestore/filestore.go` includes encryption key ID in storage
keys. Enabling therefore changes the visible namespace, disabling restores the
plaintext namespace and deletes encrypted keys, and re-enabling creates a new
namespace. This is an intentional visibility change, not data migration. Explicit
old-key ID/version lookups must converge to NotFound and remain absent.

Convergence is bounded and followed by three further agreement samples. Test logs
report elapsed convergence time. The broad deterministic sequence uses 100ms key
TTL, zero app API-key group TTL and 1ms proxy JWT TTL; the error-cache TTL remains
10 seconds. `TestDefaultCacheConvergence` separately primes both keys/endpoints,
keeps production TTLs (app auth 5m, proxy auth 1m, encryption key 10m), enables
and checks that fresh writes eventually contain encryption overhead at all four
clients, with a 6-minute bound. These configuration differences are explicit.

## Surface inventory from the released source

| Mutation | Read surfaces checked after mutation |
| --- | --- |
| SetEncryptionConfig | app GetEncryptionConfig using both keys, JSON and binary HTTP protolet; latest and explicit authorized GetEncryptionKey; full retained CAS/AC snapshots |
| ByteStream.Write / BatchUpdateBlobs | app/proxy ByteStream.Read, FindMissingBlobs, BatchReadBlobs (digest/status/bytes), Cache.GetMetadata; app UI GetCacheMetadata gRPC, binary HTTP, JSON; HTTP `/file/download` and `/file/view`; `bb download` against app and proxy |
| Directory uploads through those RPCs | all CAS surfaces above; app/proxy REv2 streaming GetTree; BuildBuddyService streaming GetTree over gRPC and the frontend's framed HTTP transport; GetTreeDirectorySizes gRPC/binary HTTP/JSON; CLI Directory JSON and recursive extraction on both endpoints |
| UpdateActionResult | complete returned result and app/proxy GetActionResult using both keys; inline stdout/stderr/output-file reads; complete ActionResult HTTP download/view; app/proxy AC metadata and app UI metadata |

Metadata asserts logical and stored sizes, positive modification/access timestamps,
access ≥ modification and no future timestamps. App UI stored/logical sizes and
modification time must equal app cache metadata; endpoint-local timestamps are
not required to equal another endpoint's timestamps (different storage events).
Small uncompressed encrypted CAS payloads must have overhead; larger records may compress below logical size. Access timestamps may advance on reads.

Sources inspected: `proto/{encryption,cache,cache_service,buildbuddy_service}.proto`,
`server/buildbuddy_server/buildbuddy_server.go`, `server/http/protolet/protolet.go`,
`server/cache_server/cache_server.go`, `server/remote_cache/directory_size`,
`enterprise/server/{crypter_service,crypter_key_cache,backends/pebble_cache,
action_cache_server_proxy,cmd/cache_proxy}`, `app/service/rpc_service.ts`,
`app/invocation/invocation_action_card.tsx`, `enterprise/app/encryption/encryption.tsx`,
and `cli/download/download.go`.

There is no public cache deletion or key rotation mutation in these services;
disable/re-enable exercises key deletion/re-creation. QueryWriteStatus is
Unimplemented on the released app, not a committed-state read oracle. Proxy has
no encryption settings UI. Invocation raw JSON/build logs/run logs and execution
profiles use the invocation blobstore and require BES/execution mutations; CAS/AC
writes do not create them. Audit logging is a separately configured conditional
surface absent from this fixture. CLI download supports CAS/Directory, not AC or
an encryption settings command. HTTP file/view shares the content-serving handler
with file/download. No unsupported RPC is silently skipped.

## Images

- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

Run: `bb remote test //tests/statemachine/encryption-state/... --test_output=errors`.

## Confirmed divergence

[Proxy AC metadata](cacheproxy-encryption-state-ac-metadata.md): full AC reads
agree, but proxy metadata remains NotFound after 15.09s/15.06s in two default-TTL
fixtures. Source shows a structural internal-key mismatch. The green suite
asserts this known response explicitly; `ac_metadata_bug_test` is bug-tagged and
requires agreement. It is excluded by ordinary `--config=nobug` CI.
