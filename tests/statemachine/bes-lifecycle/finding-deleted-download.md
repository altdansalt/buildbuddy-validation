# Deleted invocation downloads map NotFound to Internal

Title: UI raw-event and build-log downloads return Internal after invocation deletion
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: A valid authenticated deletion produces inconsistent missing-resource errors across readers. This can mislead clients into retrying a permanent absence; no stale content or authorization bypass was observed.
Status: confirmed
Repro test target: //tests/statemachine/bes-lifecycle:deleted_download_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/acd3c17a-54e5-4657-ad23-1675fa0b6e56

## Expected behavior

After `BuildBuddyService.DeleteInvocation` succeeds, `/file/download` and
`/file/view` for `raw_json`, `raw_proto`, and `buildlog` should return HTTP 404,
matching detail and event-log RPCs returning gRPC NotFound. The artifact handler
itself explicitly implements this intended mapping for status.NotFound errors.

## Actual behavior

An isolated valid BES stream is completed and all three downloads succeed before
deletion. After deletion, detail and log RPCs return NotFound, but the raw JSON
download returns HTTP 500 with Internal. Independently reproduced on remote runners:

- https://app.buildbuddy.io/invocation/acd3c17a-54e5-4657-ad23-1675fa0b6e56
- https://app.buildbuddy.io/invocation/9edb4e18-866d-4294-a3a6-f5ec176dd4de

The original auditor additionally confirmed raw JSON and build-log downloads for
success/failure builds in both runs:
https://app.buildbuddy.io/invocation/5db00179-e468-4ef5-bda1-52ddd1213baa
and https://app.buildbuddy.io/invocation/2af6de94-f068-4e00-98b1-3e0e061809d2.

This is permanent error mapping for an absent invocation, not delayed state
convergence. Source inspection identifies the common guard before artifact
selection, affecting both routes and all three artifacts. Expanded isolated verification in
https://app.buildbuddy.io/invocation/1d5673a2-a358-4ce8-85e3-d4f65d827cdd
checked all six combinations of route/artifact. Every combination remained
HTTP 500 for its two-second observation; source establishes that waiting cannot
change this mapping for an absent row. This is the measured non-convergence bound,
not a claim that a two-second delay causes the error.

## Logs excerpt

```text
deleted raw_json: HTTP 500 "rpc error: code = Internal desc = Internal server error\n"
expected: 404
actual: 500
```

## Notes

Default fixture configuration only: isolated released app, SQLite, local blobstore
and Redis, self-auth, a disposable local organization/key. No non-default flags,
shared endpoints, malformed messages, or fault injection. Startup, authenticated
pre-delete downloads and successful delete rule out fixture/network errors.

At v2.314.0, `server/buildbuddy_server/buildbuddy_server.go:serveArtifact` calls
`InvocationDB.LookupInvocation` and tests `status.IsNotFoundError`. Lookup returns
a DB record-not-found error, so this guard chooses Internal. By contrast,
`server/eventlog/eventlog.go:GetEventLogChunk` recognizes DB record-not-found.

The failing repro is tagged `bug`; normal conformance stays green. Run explicitly:
`bb remote test //tests/statemachine/bes-lifecycle:deleted_download_repro --test_tag_filters=bug --nocache_test_results --test_output=errors`.
The finding is intentionally inside the owned area; no root findings or generated
index was changed. Findings stay in this repository.

Expanded second isolated repeat: https://app.buildbuddy.io/invocation/c29ed9b6-c243-40e9-b8d7-a9be7fadd029. All six route/artifact combinations
again returned 500 after two seconds each. Detail/log RPC NotFound and pre-delete
successful downloads were checked in the same fixture.
