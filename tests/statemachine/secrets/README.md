# Encrypted secret lifecycle (v2.314.0)

Target: `//tests/statemachine/secrets:go_default_test`.

The fixture starts the digest-pinned released app and executor, using a random,
test-local AES master key with the supported development-only local KMS. No
external KMS, credentials, deployments, or product source builds are involved.
Default app digest: `sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
Default executor digest: `sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8`.
Actual images/configs/logs are preserved by the fixture.

| Mutation | Read surfaces after every successful operation |
| --- | --- |
| GetPublicKey (lazy group key creation) | gRPC and cookie-authenticated UI HTTP GetPublicKey; subsequent encryption and successful UpdateSecret |
| UpdateSecret (create/replace/repeat/recreate) | gRPC ListSecrets, UI HTTP ListSecrets, stable GetPublicKey on both transports, uncached include-secrets remote action, runner-installed bb secrets list |
| DeleteSecret (delete/repeat) | Same listing/key surfaces; consumer verifies deleted variable is absent |

The model maintains two names and current plaintext values. Listings must equal
the sorted model names and omit both ciphertext and plaintext. Consumer output
contains SHA256 hashes only; expected hashes are computed from the model, with
an absent variable represented by the empty string. Replacing one secret must
preserve the other. Every execution disables action-cache lookup and storage.
The app's transactions are synchronous, so the test checks immediately following
successful writes; there is no sleep-based convergence assumption.

Source inventory (released checkout): `proto/secrets.proto`, secret RPCs in
`proto/buildbuddy_service.proto`, `enterprise/server/secrets/secrets.go`,
`server/buildbuddy_server/buildbuddy_server.go`,
`enterprise/server/util/keystore/keystore.go`,
`enterprise/server/backends/kms/kms.go`,
`enterprise/server/remote_execution/execution_server/execution_server.go`,
`cli/secrets/secrets.go`, and
`enterprise/server/cmd/cache_proxy/cache_proxy.go`.

The cache proxy does not register BuildBuddyService or expose secret state.
There is no plaintext read RPC. `bb secrets list` is the CLI read surface;
create/update/delete are wrappers around the same mutating RPCs. The test uses
only a disposable local API key for the CLI subprocess, with ORG_ADMIN and
CACHE_WRITE capabilities, an isolated HOME and an explicitly constructed
environment. The test requires a runner-installed bb binary; absence fails
validation.

The UpdateSecret app wrapper emits CREATE/UPDATE audit events; the released
DeleteSecret wrapper has no audit hook. The audit read
surface requires enabling audit logging and an OLAP database; the shared harness
has no OLAP fixture. Audit conformance is therefore outside this test's verified
coverage. No harness changes are made.

Verified targeted run (including required CLI surface and all nine transitions):
https://app.buildbuddy.io/invocation/b6da2ae8-5b51-49ce-b778-17d9b4c9e294

```sh
bb remote test //tests/statemachine/secrets:go_default_test --test_output=all --test_arg=-test.v --nocache_test_results
```
