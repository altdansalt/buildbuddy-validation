# GetZipManifest never populates content_offset

Title: ZIP manifest entries omit documented `content_offset` (gRPC and web HTTP)
Components: app
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
Severity: low
Severity justification: Informational field of a UI helper is always 0; consumers cannot locate stored entry data. No security impact.
Status: confirmed
Repro test target: //tests/statemachine/convergence-deletes:zip_offset_repro
BuildBuddy invocations:
- https://app.buildbuddy.io/invocation/3396e2b7-20b1-42da-a05c-1e9a3a34c24f
- https://app.buildbuddy.io/invocation/700102bc-086b-415b-96f6-085f03ef6e5b

## Expected behavior

`proto/zip.proto` defines `int64 content_offset = 5` per manifest entry; for a valid stored ZIP uploaded to CAS it should equal the data offset (38 for the test archive, validated with Go `archive/zip` DataOffset).

## Actual behavior

BuildBuddyService.GetZipManifest (gRPC and `/rpc/BuildBuddyService` HTTP) returns name, sizes, compression, CRC, but content_offset is 0. Permanent (deterministic, not state dependent); reproduced in 2 invocations x 2 fixtures. `grep ContentOffset` over server/ and enterprise/ in v2.314.0 finds no assignment. Default config.

## Logs excerpt

```text
ZIP manifest content_offset gRPC=0 HTTP=0 want=38; persisted ZIP validated by archive/zip
```

## Notes

The main suite's ZIP checks therefore accept 0 for field 5 and otherwise compare name/sizes/method/CRC after every state transition. Target is `bug`-tagged and excluded from default CI.
