# Modern API-key visibility is ignored

Title: CreateApiKey and UpdateApiKey ignore modern visibility
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: Valid requests using the documented replacement field cannot grant developer visibility. The deprecated boolean is a workaround. This reproducer demonstrates under-granting, not unauthorized disclosure.
Status: confirmed
Repro test target: //tests/statemachine/org-api-keys:modern_visibility_repro
BuildBuddy invocations:
- https://app.buildbuddy.io/invocation/8e5ce0d8-ef7f-446b-9be8-2281da55988f
- https://app.buildbuddy.io/invocation/0553ff81-388e-45d6-8c4b-fc7674f57df2

## Expected behavior

proto/api_key.proto documents visibility as the replacement for visible_to_developers. Creating or updating a key with [VISIBLE_TO_DEVELOPERS, VISIBLE_TO_GROUP_ADMINS] should expose that set through GetApiKey and GetApiKeys.

## Actual behavior

Both mutations return/persist only VISIBLE_TO_GROUP_ADMINS. Both read RPCs report mask 2 instead of 3 in two independent remote runs. The fixture uses default app configuration and a locally provisioned ORG_ADMIN key; no shortened TTL or proxy participates in this repro.

## Logs excerpt

```text
create GetApiKey visibility mask=2 want 3 (developers + admins)
create GetApiKeys visibility mask=2 want 3 (developers + admins)
update GetApiKey visibility mask=2 want 3 (developers + admins)
update GetApiKeys visibility mask=2 want 3 (developers + admins)
```

## Notes

Permanent for the persisted state, rather than cache convergence: released server/buildbuddy_server/buildbuddy_server.go CreateApiKey passes only GetVisibleToDevelopers to AuthDB, and UpdateApiKey builds its table object from the same deprecated boolean. Neither reads GetVisibility. AuthDB derives the persisted visibility bitmask from that boolean. No non-default configuration dependence. Observation is immediate; no eventual repair path exists in these handlers. The isolated repro is tagged bug and excluded from normal CI. Findings remain in this directory as required by this task; no upstream report or PR.
