# GetTree on an absent root Directory returns FailedPrecondition, not NotFound

Title: REv2 ContentAddressableStorage.GetTree status for a missing root
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0
- app: gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
- cacheproxy: gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: Status-code conformance only; no data is lost or exposed. Clients that map NOT_FOUND to a cache miss may misclassify the failure. Same status on app and proxy, so the two endpoints agree with each other.
Status: confirmed
Repro test target: //tests/statemachine/encryption-state:tree_status_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/2a0d5125-868e-4541-9328-371c7ef64c33
Second invocation: https://app.buildbuddy.io/invocation/2cc4aa78-979c-4865-8e96-074b107859cf

## Expected behavior

REv2 documents NOT_FOUND when the root Directory of GetTree is absent from CAS.
ByteStream.Read of the same digest returns NOT_FOUND at both endpoints.

## Actual behavior

For a valid, never-uploaded Directory digest, GetTree's first Recv returns
`FailedPrecondition: Digest ... not found` at app and proxy, with all four
clients (two API keys x two endpoints), for 3 samples each, in both runs.
Permanent (deterministic; same on every sample), default config, encryption
disabled, no TTL overrides.

## Log excerpt

```text
sample=0 client=0 ByteStream=NotFound GetTree=rpc error: code = FailedPrecondition desc = Digest hash:"6994..." size_bytes:78 not found
```

## Notes

The green suite asserts the observed status explicitly so the broad sequence
keeps checking every other surface; the bug-tagged target demands NOT_FOUND.
