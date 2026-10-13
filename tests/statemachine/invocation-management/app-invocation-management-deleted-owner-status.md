# Deleted invocation owner returns Unknown

Title: GetInvocationOwner reports Unknown rather than NotFound after deletion
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Server-admin owner lookup exposes a database error instead of the absence status used by invocation detail; no unauthorized owner disclosure observed.
Status: confirmed
Repro test target: //tests/statemachine/invocation-management:divergences_test (TestDeletedOwnerIsNotFound)
BuildBuddy invocations: https://app.buildbuddy.io/invocation/3414cb9a-c920-4227-93bf-6b485d84840d and https://app.buildbuddy.io/invocation/0ff8fe90-c115-4d9b-9359-b5e65f2d6cde

## Expected behavior

A successful owner lookup followed by DeleteInvocation should give NotFound, matching GetInvocation.

## Actual behavior

GetInvocationOwner returns `Unknown: record not found` throughout two independent three-second observation windows. Cookie-authenticated HTTP also contains `record not found` in the normal suite. Permanent per source: LookupGroupIDFromInvocation returns the SQL missing-row error directly; BuildBuddyServer propagates it without status translation. No asynchronous deletion/convergence mechanism appears on this path.

## Logs excerpt

```text
deleted owner elapsed=384.407µs err=rpc error: code = Unknown desc = record not found
deleted owner elapsed=2.714179153s err=rpc error: code = Unknown desc = record not found
expected: NotFound actual: Unknown
```

## Notes

Requires non-default `auth.admin_group_id` set to the disposable fixture group and an ORG_ADMIN API key. The test restarts only the fixture app before ingestion and refreshes published ports and cookie login. Successful pre-delete lookup proves this is not an auth/config error. No shared harness changes. The strict repro is tagged `bug`; normal conformance checks the known exact code and message.
