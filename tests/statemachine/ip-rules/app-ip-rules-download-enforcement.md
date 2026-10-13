# UI download routes bypass IP rules that gRPC/HTTP RPCs enforce

Title: `/file/download` (raw JSON, build log, CAS) serves private content to an IP denied by IP rules
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: After an org revokes an IP (rules enabled), that client is denied by CAS, GetUser, GetEventLogChunk and GetFile, yet can still download invocation raw JSON, build logs and CAS blobs via the UI download route (needs a valid session/credential; bypasses the IP restriction).
Status: confirmed
Repro test targets: //tests/statemachine/ip-rules:artifact_download_enforcement_bug, //tests/statemachine/ip-rules:cache_download_enforcement_bug (tag `bug`; excluded by --config=nobug)
BuildBuddy invocation: https://app.buildbuddy.io/invocation/672a500d-1d71-4cff-aab8-1bd252f13876
Second run: https://app.buildbuddy.io/invocation/e9889ace-69d4-4af5-81b2-7d7996268cd2 (third: https://app.buildbuddy.io/invocation/1f1a8fab-09fa-48e4-bcb5-cd675d1c0c7f)

## Expected behavior
Once the rule change is visible (CAS PermissionDenied, GetUser DENIED_BY_IP_RULES), every endpoint the UI uses should deny the same IP.

## Actual behavior
Reproduced on 3 independent remote runs (both targets each time). Permanent within the observation window: HTTP 200 with private content in all 25 samples over ~7.2s after CAS denial (~1.0s after revoke) — the window is bounded, not a proven infinite one. Uses default rule-cache TTL; requires IP rules enabled and trusted X-Forwarded-For (the config needed to use IP rules per-client at all); cache variant additionally enables signed app identity.

## Logs excerpt
```text
CAS revoked after 1.015602175s; checking artifacts for another 6 seconds
raw_json: revoked IP received HTTP 200 with private content in 25 samples over 7.22s; CAS=PermissionDenied, GetUser=DENIED_BY_IP_RULES, GetEventLogChunk=HTTP 403
buildlog: ... HTTP 200 ... ; cache: ... HTTP 200 ...
```

## Notes
Isolated app-only fixture; no proxy/OLAP. The main consistency suite records this divergence explicitly instead of failing. Details in README.md.
