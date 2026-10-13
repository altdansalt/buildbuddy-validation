# Deleted invocation downloads return Internal instead of NotFound

Title: Deleted invocation downloads return HTTP 500 instead of HTTP 404
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Missing invocations look like server failures to download clients; no surviving data disclosure observed.
Status: confirmed
Repro test target: //tests/statemachine/invocation-management:divergences_test (TestDeletedDownloadIsNotFound)
BuildBuddy invocations: https://app.buildbuddy.io/invocation/bcd69526-6224-42e8-9df5-552e6dfbd2fd and https://app.buildbuddy.io/invocation/5f39e088-1f07-473d-9170-7efbd81e688e

## Expected behavior

After successful DeleteInvocation, detail gRPC and authenticated raw-event downloads agree on absence. `/file/download?artifact=raw_json` returns HTTP 404. Released `serveArtifact` explicitly maps NotFound to 404.

## Actual behavior

The identical authenticated GET succeeds before deletion, then returns HTTP 500 with Internal while GetInvocation returns NotFound. Both runs observed ten samples spanning 3 seconds without convergence. This is a permanent status-mapping discrepancy according to source: LookupInvocation returns an unclassified missing-row error to serveArtifact. A finite test alone establishes only persistence through the observation window.

## Logs excerpt

```text
deleted download elapsed=720.251µs status=500 body=rpc error: code = Internal desc = Internal server error
deleted download elapsed=2.717625616s status=500 body=rpc error: code = Internal desc = Internal server error
expected: 404 actual: 500
```

## Notes

Default fixture config; disposable org/key; same URL and key before and after deletion. No image override, external endpoints or injected failures. Normal conformance explicitly records the known 500 response for raw_json/raw_proto/buildlog download/view routes; strict expected behavior is isolated behind `bug`. Findings are kept in this assigned directory as requested.
