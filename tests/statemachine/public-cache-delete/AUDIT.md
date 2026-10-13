# Local rigor audit

Audited origin/cand/sm1/public-cache-delete at e6f7fa0. Verdict: shallow relative
to the requested every-surface and app/proxy convergence contract.

## Actual assertions

- BatchUpdateBlobs (via harness.Upload): initial target/unrelated blobs and
  Action/Command/Directory protos are uploaded. Checks are grouped, not after
  every upload, and only the target/unrelated blob gets the full reader matrix.
- ByteStream.Write: target CAS recreation gets the full matrix immediately.
- CAS DeleteFile gRPC/REST: FindMissingBlobs exact missing digest; BatchReadBlobs
  count/status/data; ByteStream.Read status/data; GetFile gRPC/HTTP status/data;
  GetFileRange gRPC/HTTP slice/status; /file/download status/data; metadata
  existence gRPC/HTTP; bb download bytes or any process error (empty instance).
- UpdateActionResult: response and subsequent GetActionResult equal the full
  reference proto. AC GetFile gRPC/HTTP and /file/download match serialized bytes;
  AC metadata presence is checked. Target and unrelated CAS remain readable.
- AC DeleteFile gRPC/REST: these same checks assert absence; second AC key and
  output CAS are retained. CAS deletion happens only after removing every AC
  reference, so dangling-output AC behavior is untested.

All original RPCs address the app. No cache proxy is started. Metadata gRPC
responses are discarded; HTTP metadata need only be nonempty, so incorrect
sizes/timestamps pass. BatchReadBlobs responses need not identify each requested
full digest exactly once; two copies of the unrelated response can pass. CLI
absence accepts any failure. Missing GetFile wraps errors as Unavailable, but
other readers corroborate absence. No skips or unconditional early success.
Metadata retries only Unavailable for 15s, never contradictory presence results.
Some closures capture the parent t; fatal failures remain failures but can abort
subtest execution less cleanly.

## Missing source-backed readers

- REAPI CAS GetTree; BuildBuddyService GetTree and GetTreeDirectorySizes (including
  frontend HTTP transports). Relevant to uploaded Directory CAS state; only an
  empty Directory is seeded, and directory deletion isn't exercised.
- /file/view and compressed/range/download variants; CLI download directory mode.
- GetActionResult inline output/stdout/stderr and conditional-result variants.
- Metadata field values, nested CLI paths, all warmed proxy readers.

Source: proto/{remote_execution,cache,buildbuddy_service}.proto;
server/buildbuddy_server/buildbuddy_server.go;
app/util/get_tree.ts; app/invocation/invocation_action_card.tsx;
app/service/rpc_service.ts; cli/download/download.go.
QueryWriteStatus exists but the released app returns Unimplemented, so it is
not a usable presence oracle. API GetAction describes BES invocation actions,
not a direct AC reader; it should not be counted as an omitted AC oracle.

## Assertion perturbations: 3/3 failed

Three independent copies of the original test were executed with TestPerturb
filter. Each also failed in both empty and nested instances:
1. Post-delete casPresent=false changed to true: FindMissingBlobs assertion failed.
2. BatchReadBlobs expected response count 2 changed to 3: count assertion failed.
3. Reference AC result cloned and ExitCode incremented: proto equality failed.

https://app.buildbuddy.io/invocation/be7bc8e7-f356-45cb-80bd-7584e1191acb
Perturbations were removed after this run.

## New divergence: observed twice

TestAuditWarmedProxyDelete uploads through app, warms proxy with two successful
ByteStream reads, calls app DeleteFile, and samples at 0, 5, 30, 60 seconds.
Both independent fixture runs produced at every sample:

| Surface | App | Proxy |
| --- | --- | --- |
| FindMissingBlobs missing count | 1 | 1 |
| BatchReadBlobs status | NotFound (5) | OK (0), original bytes |
| ByteStream.Read | NotFound | OK, original bytes |

The 60-second convergence assertion failed twice:
https://app.buildbuddy.io/invocation/b28416ba-2720-4986-8f2d-6eced0e8de44

Pinned v2.314.0 images:
app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
proxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906

This establishes a missed inconsistency and lack of convergence within 60s,
not that convergence can never occur or that the released product promises
remote deletion invalidation. Source explains the behavior: proxy ByteStream
Read and BatchReadBlobs serve local hits without validating remote presence.
No shared deployments or credentials were used for component calls.

Only a local throwaway branch is changed; no push or PR.

Original TestDeleteStateMachine passed unchanged in the comparison run:
https://app.buildbuddy.io/invocation/53770263-33d3-4ad6-be3f-b03b3fa3d0d4
