# Encryption state conformance (v2.314.0)

Only released app/proxy images are started. Local AES KMS files and a signed
cache-proxy client identity are disposable test credentials. The org-admin key
is provisioned through the fixture-local web API. Auth cache intervals are
shortened for deterministic transitions. The production key-cache TTL defaults
to 10 minutes; this fixture sets it to 100ms and uses bounded polling for
key absence after disable and fresh identity after re-enable. Key error caching
retains its supported 10-second default. CAS visibility also uses bounded polling.

Source inventory:

| Mutation | Read surfaces | Sequence |
| --- | --- | --- |
| BuildBuddyService.SetEncryptionConfig | GetEncryptionConfig gRPC and web JSON API; authorized EncryptionService.GetEncryptionKey; cache content and metadata | disabled, repeat disable, enable, repeat enable, disable, repeat disable, re-enable, repeat enable |
| ByteStream.Write / CAS.BatchUpdateBlobs | ByteStream.Read, CAS.FindMissingBlobs, CAS.BatchReadBlobs on app and proxy; Cache.GetMetadata on both; BuildBuddyService.GetCacheMetadata gRPC and web; bb download via proxy | new unique records from both endpoints in every state; retain and read records throughout each enabled epoch |
| ActionCache.UpdateActionResult | ActionCache.GetActionResult on app and proxy | create via app, overwrite via proxy, read both after each operation |
| EncryptionService.GetEncryptionKey | repeated latest-key and explicit ID/version retrieval | stable metadata and key material; re-enable creates different key |

`proto/encryption.proto` and `enterprise/server/crypter_service/crypter_service.go`
show that enabling creates the key transactionally, repeated enable is a no-op,
and disabling deletes the key records. There is no public rotation RPC and no
cache deletion RPC in these services. Old encrypted records are intentionally
outside the readability contract after disable. Key re-encryption is a background
KMS operation, not an RPC transition exercised here.

`proto/cache.proto` exposes logical and stored sizes, timestamps, but no key ID
or version. New encrypted app records must have encryption overhead; logical
sizes must match at app, proxy and UI. Key identity/version is asserted through
the authorized encryption service, rather than inferred from metadata. Proxy
has no settings UI or supported encryption configuration RPC. `cli/download`
uses ByteStream and is exercised against the test-local proxy.

Image defaults (also logged by the fixture):
- app: gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
- proxy: gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906

Run: `bb remote test //tests/statemachine/encryption-state:go_default_test --test_output=errors`
