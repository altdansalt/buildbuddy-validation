# FindMissingBlobs accepts an unknown digest function

Title: FindMissingBlobs succeeds for unadvertised, unknown digest function 999
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Protocol validation inconsistency can let a client proceed with an unsupported algorithm; no data corruption or security impact was demonstrated.
Status: unverified
Repro test target: //tests/app/capabilities:unknown_digest_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/5baf85c8-69e3-4287-a01d-efe91a51e49c

## Expected behavior

An explicit unknown digest algorithm should fail cleanly with INVALID_ARGUMENT
or UNIMPLEMENTED, rather than report successful CAS lookup. Capabilities lists
SHA256, SHA384, SHA512, SHA1 and BLAKE3; enum 999 is neither advertised nor defined.
UNKNOWN (0), which requests inference, is deliberately excluded from the repro.
[REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

## Actual behavior

FindMissingBlobs returns OK and the requested digest in missing_blob_digests for
algorithm 999. Reproduced independently in the invocation above and
https://app.buildbuddy.io/invocation/58aa48b5-61c1-4a63-8832-6bf4c9668fb9.
Both used --nocache_test_results. A valid SHA256 lookup of the same digest returns
the expected missing result; the local app is healthy and authenticated.
The same observation occurred for several known but unadvertised algorithms in
exploration, but the minimal repro only uses an unequivocally unknown enum.

## Logs excerpt

```text
unknown digest response=missing_blob_digests:{hash:"63cae687d634bfc46e46d6e22ea3889eefbcadc27e9fd6124bcde2615536951f" size_bytes:22} error=<nil>
[]codes.Code{0x3, 0xc} does not contain 0x0
unknown digest algorithm must be rejected
```

## Notes

Default fixture config, no image override, all RPCs address the disposable local
app. This is a validation finding; no claim is made that unsupported algorithms
can successfully upload data. Status remains unverified pending maintainer review
as requested for round 2, despite two independent reproductions. No upstream
report was sent.
