# UI /file/download bypasses IP rules that gRPC and /rpc enforce

Title: `/file/download` serves private invocation artifacts (build log, raw JSON) to an IP denied by IP rules
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: After an org enforces IP rules, a client outside the allowlist is denied by CAS gRPC, `/rpc` (GetEventLogChunk, GetUser) but can still read invocation build logs and raw JSON through `/file/download`. It still needs a valid API key/session, so this is a gap in the IP restriction layer, not an authentication bypass. Requires non-default config (below).
Status: confirmed
Repro test target: //tests/statemachine/ip-rules:artifact_download_enforcement_bug
BuildBuddy invocation: https://app.buildbuddy.io/invocation/973f45f3-c58d-487a-b122-788a74d1f0b5

## Expected behavior

Once a rule change is visible (CAS returns PermissionDenied), every endpoint serving the org's data should deny the same client IP, including `/file/download`.

## Actual behavior

Mutation: add an allow rule for the admin IP only (`AddIPRule`) and `SetIPRulesConfig(enforce)`. Then, for a client at a non-allowed IP:

- CAS `FindMissingBlobs`: PermissionDenied (control, enforced)
- `/rpc/BuildBuddyService/GetEventLogChunk`: HTTP 403 (control, enforced)
- `/file/download?artifact=buildlog` and `artifact=raw_json`: HTTP 200 with the private content (before the rule, the same requests also return 200, so only these surfaces disagree)

The bypass is permanent, not a transient cache window: it is structural. `Enforcer.AuthorizeHTTPRequest` only applies IP checks to paths with prefix `/rpc/` or `/api/` and returns the request unchanged for anything else, so `/file/*` is never checked
(enterprise/server/ip_rules_enforcer/ip_rules_enforcer.go:562-567 at v2.314.0). Earlier sampling (25 samples over ~7s in 3 runs) is consistent with this but alone would not prove permanence; the code does. `/file/view` sits on the same unchecked path prefix by the same reading of the code, but is not exercised by this repro.

Config needed (not defaults): `--auth.ip_rules.enable=true`, `--auth.trust_xforwardedfor_header=true` (so the client IP can be set per request), and `--auth.api_key_group_cache_ttl=1s` for speed. Rule enforcement itself is on via `SetIPRulesConfig`. App-only fixture; no proxy, executor, or OLAP.

Reproductions: [run 1](https://app.buildbuddy.io/invocation/973f45f3-c58d-487a-b122-788a74d1f0b5) and [run 2](https://app.buildbuddy.io/invocation/c2443a0b-0115-4e19-92ab-3f5b393ea29b); earlier runs of the original suite: e9889ace-69d4-4af5-81b2-7d7996268cd2 and 672a500d-1d71-4cff-aab8-1bd252f13876. The baseline download (200 before the rule) and enforced controls rule out harness error.

## Logs excerpt

```text
buildlog: revoked IP got HTTP 200 with private content; CAS=PermissionDenied, GetEventLogChunk=HTTP 403
raw_json: revoked IP got HTTP 200 with private content; CAS=PermissionDenied, GetEventLogChunk=HTTP 403
```

## Notes

Run with `--test_tag_filters=bug --nocache_test_results`. Excluded from the default suite by the `bug` tag. The original broad suite and a CAS `bytestream_url` variant (needs signed app identity) live on `origin/cand/sm1/ip-rules` and are not part of this pick.
