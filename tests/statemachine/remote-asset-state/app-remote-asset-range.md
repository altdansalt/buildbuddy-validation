# Proxy asset full-file/range disagreement

Title: App full-file readers follow proxy URI, range readers ignore its host
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; proxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: A valid proxy-addressed asset can be downloaded but cannot be read through the app range API. No corruption or auth boundary crossing demonstrated.
Status: confirmed
Repro test target: //tests/statemachine/remote-asset-state:proxy_range_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/5d069f6e-a0c2-4533-838f-944260af5806

## Expected behavior

The same bytestream URI should identify the same content in full-file and range APIs. This is the requested cross-surface agreement contract.

## Actual behavior

Proxy FetchBlob succeeds. App streaming GetFile, HTTP GetFile, and /file/download return its exact bytes. App GetFileRange returns gRPC NotFound; HTTP range returns 500 with NotFound. The isolated positive-contract test failed twice with fresh fixtures using -test.count=2 in the invocation above. Independent earlier audit repetitions: https://app.buildbuddy.io/invocation/da204756-2b27-4ec5-b5a6-4e04bfff7121 and https://app.buildbuddy.io/invocation/a37596c8-cf41-48c2-b8ec-fc8c0bd300e8 . Expanded negative checks repeat the HTTP result after every operation: https://app.buildbuddy.io/invocation/c22a3509-1175-40d4-b8cd-69f89f315c25 .

## Logs excerpt

```text
TestProxyFetchRangeAgreement (both fresh fixtures):
rpc error: code = NotFound desc = key "PTdefault/d912709b96278511c6e08e1dd61fcb29af2b61188ba8ab88bf08d51b89e52bc0/1/cas/v5" not found
AUDIT same proxy URI: HTTP GetFileRange status=500 body=rpc error: code = NotFound ...
```

## Notes

Permanent routing difference while the blob remains proxy-only, not measured transient convergence. api_server.go GetFileRange discards the URI host and calls app-local cache.Reader; full-file readers follow that host. Proxy FetchBlob intentionally does not replicate upstream. The suite checks app absence for one second then rechecks all retained versions after every step; this is a bounded observation, not proof about infinite time. No nondefault cache routing flags. Only remote_asset.allowed_private_ips=0.0.0.0/0 permits the test-local origin, which is necessary to avoid shared deployments. Both default and nondefault instances exhibit it. Same-org auth, full reads, and successful app-fetch positive controls rule out fixture auth/network problems. Default CI asserts the known negative behavior; the bug-tagged target preserves the positive contract. No upstream bug report or new-bug novelty claim.
