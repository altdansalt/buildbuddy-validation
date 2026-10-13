# User API key state machine, round 2

Released enterprise-v2.314.0 images:
- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

Only disposable fixture endpoints receive component requests. The CLI release
download is checksum-pinned bb 5.0.492 (SHA256
`e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`).
CLI subprocesses use an isolated Git repository and a clean environment; they
do not inherit dispatch credentials.

## Mutation / surface matrix

| Mutation | Surfaces checked after each step |
| --- | --- |
| BuildBuddyService.CreateUserApiKey | Response, all modeled keys through GetUserApiKeys/GetUserApiKey/GetApiKey over gRPC and cookie JSON HTTP, org-list exclusion, GetBazelConfig credentials over both transports, creation metadata, optional certificates, GetUser identity, Authenticate JWT identity/group/capabilities, bb login --check, app/proxy GetCapabilities and effective CAS/AC/ByteStream writes and reads |
| api.v1.ApiService.CreateUserApiKey | gRPC and public HTTP creation, then the same model checks |
| BuildBuddyService.UpdateUserApiKey | Same surfaces, unchanged secret/creation timestamp, CAS-only/full-write/read-only transitions, repeated identical valid updates |
| BuildBuddyService.DeleteUserApiKey | Lists/setup omit deleted keys, single readers return NotFound, stable bounded app/proxy auth revocation, revoked GetUser/Authenticate and bb login --check, remaining live keys retain their complete projections |
| Re-create after delete | Fresh ID and secret, same complete model checks, followed by deletion |
| Expiration after CreateUserApiKey(expires_in) | Both single readers on gRPC/HTTP, both lists, HTTP setup omission, bounded stable app/proxy rejection |

The fast machine uses one-second app API-key and proxy JWT cache TTLs. Each
poll requires three consecutive matching samples and records elapsed convergence
time in the test log; stable sampling is finite, not proof of perpetual stability.
Separate TestDefaultTTLRevocation keeps the released five-minute app / one-minute
proxy defaults and measures rejection through both components. Certificate
generation uses an ephemeral fixture CA and enables SSL in the fast machine;
certificates are parsed, their private key checked, signature chain verified,
and subject API-key identity compared. This is not an mTLS transport test.

Every step reads all modeled keys, including deleted keys and two same-label keys
in the same org. Cache probes alternate app/proxy writers and compare reads from
both components. Fresh digests distinguish silently dropped read-only writes
from stored writes. BatchReadBlobs, FindMissingBlobs, ByteStream Read and
GetActionResult check stored state. Capability advertisement sends Bazel 7
request metadata, avoiding the server's legacy-client permissive fallback.

Lists explicitly reject duplicate IDs. HTTP deletion checks require the
NotFound status text, rather than accepting any transport/server error.
The local audit's GetApiKey/setup probes were integrated; intentional auditor
perturbations were removed.

## Released source inventory

- proto/api_key.proto, proto/api/v1/api_key.proto, proto/bazel_config.proto,
  proto/auth.proto and proto/buildbuddy_service.proto define these surfaces.
- server/buildbuddy_server/buildbuddy_server.go implements reads and
  toProtoAPIKeys: setup credentials omit capabilities/secrets/expiry/creation
  metadata, but include user_owned. User-key single readers omit ownership,
  expiry and creation metadata. User-key lists expose creation metadata.
- enterprise/server/backends/authdb/authdb.go provides storage, expiry filters,
  permission checks and the five-minute API-key authentication cache.
- enterprise/server/remoteauth/remoteauth.go supplies the proxy's one-minute
  JWT cache.
- enterprise/app/api_keys/api_keys.tsx retrieveValue uses GetApiKey with
  includeCertificate; its list UI displays creationMetadata.
- enterprise/app/settings/settings.tsx selects GetUserApiKeys for personal keys.
- app/docs/setup_code.tsx uses GetBazelConfig and GetApiKey for credentials
  and certificates.
- cli/login/login.go authenticates login --check using GetUser.
- server/remote_cache/capabilities_server/capabilities_server.go advertises AC
  update permission only for modern Bazel client request metadata.
- server/ssl/ssl.go encodes the API key ID in certificate subject SerialNumber.

There is no public user-key list/update/delete API and no proxy key-management
API. Invocation downloads/log/raw JSON and streaming invocation endpoints do
not project the user-key record; this suite does not enumerate every RPC that
can authenticate with a key.

## Remaining limitations

This is expanded coverage, not a claim of exhaustive EVERY-surface conformance.
Distinct-user/role access requires a local mock OIDC provider; the built-in
fixture has one identity. Such access remains untested. Generated certificates
are checked as credentials, but mTLS acceptance/revocation is untested.
Expiry is tested separately with short caches; default-TTL expiry is untested.
Default-TTL capability updates are not exhaustively sequenced.
The key model retains an independent user-owned setup-reader credential for
GetBazelConfig because an org key has no authenticated user ID.
There are no skips or intentionally passing substitutes for these limitations.

## Run

```
bb remote --timeout=25m test //tests/statemachine/user-api-keys/... --test_output=all --test_arg=-test.v --nocache_test_results
```

The sequence and default-TTL/expiry tests are separate large targets, each with
the standard long timeout. The targeted package command runs both.

## Validation

Expanded key readers/JWT/capabilities:
https://app.buildbuddy.io/invocation/4990ce3e-bd87-4692-b856-472add33de97

Every-key checks including pinned CLI login --check:
https://app.buildbuddy.io/invocation/e9f35b05-7894-4ba8-b224-d84f69c0fd18

No confirmed product divergence has been documented.

Final package run (both targets, uncached, Go verbose logs retained):
https://app.buildbuddy.io/invocation/81451e25-4461-45ab-8d3c-014b9eac5a12
Runner invocation:
https://app.buildbuddy.io/invocation/07177aa2-f500-49eb-8dec-b1a99a434f98
The sequence target passed in 328.4s and default-TTL/expiry target in 350.5s.
Both stayed below the repo's configured 600-second long-test timeout.
Per-component convergence and expiry observation times are emitted in test.log.
Each creation transport uses a separate fixture with two same-label keys, so
the full package retains all three creation paths without a growing pool of
unrelated keys increasing CI runtime.

Expiry independently passed:
https://app.buildbuddy.io/invocation/ceeffc95-480b-4e86-9b64-169fccb691ac

Certificate-enabled sequence and setup credential checks independently passed:
https://app.buildbuddy.io/invocation/867a35c1-fc7a-47c7-935a-d7872c1a2b12
https://app.buildbuddy.io/invocation/84aa8849-7e79-4198-8730-48aad71a42a3

Development failures were invalid administrative auth/request context and a
corrected BUILD syntax error, not product findings. Superseded long validation
jobs were canceled. No bug-tagged reproducer or finding was added.
