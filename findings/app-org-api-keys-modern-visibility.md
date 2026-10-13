# CreateApiKey ignores the repeated visibility field

Title: CreateApiKey ignores the repeated `visibility` field; only the deprecated `visible_to_developers` bool is honored
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: `visibility` is the documented replacement for a deprecated field and the proto says an empty list is tolerated "during migration", so this is an unfinished migration. A caller using only the new field gets a more restrictive key (admins only), never wider access. Keys stay usable and expiry is unaffected.
Status: confirmed
Repro test target: //tests/statemachine/org-api-keys:modern_visibility_repro (on branch `pick/sm1/org-api-keys`, commit 4603117; not on main; control: //tests/statemachine/org-api-keys:visibility_control)
BuildBuddy invocation: https://app.buildbuddy.io/invocation/cc83ed69-4752-4f42-8804-3c9ed6111a42
BuildBuddy invocation (second run): https://app.buildbuddy.io/invocation/b1689229-b752-46ef-8ec2-40aeab899eb2

## Expected behavior

`proto/api_key.proto` marks `visible_to_developers` as "Deprecated: use visibility instead". `CreateApiKeyRequest.visibility = [VISIBLE_TO_DEVELOPERS, VISIBLE_TO_GROUP_ADMINS]` should give the same stored state as `visible_to_developers = true`: mask 3 on every read surface.

## Actual behavior

The two inputs express the same intent but produce different state. Control (`visible_to_developers=true`) reads mask 3 on CreateApiKey, GetApiKey and GetApiKeys. The repeated field reads mask 2 (admins only) on all three. The repro ran twice against default app config with a test-local ORG_ADMIN key; the control passing in the same fixture rules out harness or auth error.

`UpdateApiKey` has the same cause (it also builds its update from the bool only) but is not covered by this minimal repro.

## Logs excerpt

```text
CreateApiKey visibility mask=2 want 3
GetApiKey visibility mask=2 want 3
GetApiKeys visibility mask=2 want 3
```

## Notes

Root cause, in `~/buildbuddy-src` at v2.314.0: `server/buildbuddy_server/buildbuddy_server.go:1171-1173` (`CreateApiKey`) passes only `req.GetVisibleToDevelopers()` to `AuthDB.CreateAPIKey`, and `:1225` (`UpdateApiKey`) copies only that bool into `tables.APIKey`. Neither reads `GetVisibility()`. `enterprise/server/backends/authdb/authdb.go:758,804-811` derives the stored mask solely from the bool.

Permanent on default config; no non-default configuration is needed and there is no later repair path in these handlers. Reads are consistent with each other (all mask 2); the divergence is between request and stored state.

Control run (passes): https://app.buildbuddy.io/invocation/99151521-86a4-42bb-aecb-ac0c625b30b0

Related, not reproduced here: `ExpiryUsec` is omitted from the CreateApiKey response and the GetApiKey/GetApiKeys projections (`buildbuddy_server.go:1185-1194`, `:1536-1546`) although the expiry is stored and enforced; the impersonation constructor at `:1284` does set it. Found on `origin/cand/sm1/org-api-keys`.

Priority (sm1 run): P2 of 5. Judges scored interest 4/7/8 and confidence 9/7/8; the lowest-interest judge called visibility an unfinished migration field. Required config: default, no flags. Permanence: permanent (stored state, no repair path). The separate `ExpiryUsec` omission has a second repro on `origin/cand/sm1/org-api-keys` and is the same family (create/list projections drop a stored field); it was not independently reproduced on the pick branch. Table: [sm1/PRIORITY.md](sm1/PRIORITY.md).
