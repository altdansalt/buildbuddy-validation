# Proxy asset Directory bytes/tree disagreement

Title: Proxy FetchBlob stores Directory bytes that proxy GetTree cannot retrieve
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; proxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: Directory traversal fails for readable proxy assets. No corruption or auth boundary crossing demonstrated.
Status: confirmed
Repro test target: //tests/statemachine/remote-asset-state:proxy_tree_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/9a759ad9-a64d-4d5b-a5f1-e615e7ff75a4

## Expected behavior

A valid serialized Directory fetched to the proxy CAS should be readable through that proxy's GetTree as well as ByteStream and CLI Directory decoding.

## Actual behavior

ByteStream reads the exact Directory bytes, but proxy GetTree returns FailedPrecondition naming the root digest (not a missing child). Reproduced twice with separate fresh fixtures via -test.count=2: https://app.buildbuddy.io/invocation/9a759ad9-a64d-4d5b-a5f1-e615e7ff75a4 . Independent state-machine reproduction: https://app.buildbuddy.io/invocation/2d78fe4b-c911-4783-8758-c870709fa482 and passing negative coverage https://app.buildbuddy.io/invocation/c22a3509-1175-40d4-b8cd-69f89f315c25 . App-fetched directories succeed through both CAS endpoints, UI tree RPC and CLI JSON decoding.

## Logs excerpt

```text
TestProxyFetchTreeAgreement (both fresh fixtures):
rpc error: code = FailedPrecondition desc = Digest hash:"6994544955e5824662eaa9cec3e5d072692c4273f21cafa482c15cfa846a31aa" size_bytes:78 not found
```

## Notes

Permanent routing difference while content remains proxy-only. content_addressable_storage_server_proxy.go GetTree forwards to upstream; proxy_skip_remote is explicitly Unimplemented for GetTree. FetchBlob stores local bytes and does not replicate upstream. No nondefault routing flags; only the origin private-IP allowlist described in the range finding. Tree traversal does not read file-node bytes, and the error explicitly identifies the existing root. The main sequence additionally retains the referenced file bytes. No data corruption or auth boundary crossing; impact is unavailable tree browsing for otherwise readable assets. Known divergence from the requested agreement contract, not a novelty claim about an upstream defect. Default CI asserts the observed result; the bug-tagged target asserts the positive contract.
