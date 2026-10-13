# API-key expiry is absent from read projections

Title: Expiring organization keys are projected as having no expiry
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Metadata clients cannot distinguish expiring keys from unlimited-lifetime keys. This finding concerns projection consistency, not bypass of expiry enforcement.
Status: confirmed
Repro test target: //tests/statemachine/org-api-keys:expiry_projection_repro
BuildBuddy invocations:
- https://app.buildbuddy.io/invocation/a9b831f0-7ff5-489b-827f-69c97b992326
- https://app.buildbuddy.io/invocation/c2da5b94-a177-4d0c-b860-c6c57ffa5c93

## Expected behavior

A valid CreateApiKey request with expires_in=3600 seconds should return an expiry_usec within the clock-bounded interval one hour after request dispatch/completion. GetApiKey, GetApiKeys and GetBazelConfig expose the same ApiKey message and should expose that stored expiry.

## Actual behavior

All four projections return zero. Reproduced twice on independent test-local default app fixtures. AuthDB.CreateAPIKey stores the future expiry, but the released server handler constructors and toProtoAPIKeys omit ExpiryUsec. The neighboring impersonation constructor explicitly includes it. The independent green TestExpiryEnforcement checks that a short-lived key does stop authenticating through app/proxy; metadata disappearance is not used as an expiry oracle.

## Logs excerpt

```text
CreateApiKey expiry_usec=0 want within [1791874236383016,1791874236385616]
GetApiKey expiry_usec=0 want within [1791874236383016,1791874236385616]
GetApiKeys expiry_usec=0 want within [1791874236383016,1791874236385616]
GetBazelConfig expiry_usec=0 want within [1791874236383016,1791874236385616]
```

## Notes

Permanent projection omission, not transient cache lag: response constructors do not assign the field. Observed immediately; no non-default configuration required in the repro. Uses ORG_ADMIN capability and a positive duration, with no fault injection. Tagged bug and excluded from ordinary CI. The HTTP transport invokes the same released constructors, but the minimal repro directly asserts gRPC projections only. No upstream report or PR.
