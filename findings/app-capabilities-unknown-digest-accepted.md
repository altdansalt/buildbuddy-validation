# FindMissingBlobs accepts an unknown digest function

Title: FindMissingBlobs succeeds for unadvertised, unknown digest function 999
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Low is retained as the original triage rating. Verification found no established protocol violation or impact; this rejected report should not be counted as an actionable low-severity defect.
Status: rejected
Repro test target: //tests/app/capabilities:unknown_digest_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/5baf85c8-69e3-4287-a01d-efe91a51e49c

## Original expected behavior (rejected in verification)

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
can successfully upload data. Independent review rejects the required-error premise; see below. No upstream
report was sent.


## Verification

Verdict: **rejected**, independently reviewed on 2026-10-13 against PR #82
head `74d691a`. The observed OK/missing response is real and reliable, but the
test's requirement that enum 999 produce INVALID_ARGUMENT or UNIMPLEMENTED is
not established by the cited protocol. No unsupported-algorithm upload,
incorrect presence result for a supported algorithm, or data corruption was
demonstrated. Low remains the original triage rating, not an assessment of a
confirmed defect.

The [REAPI v2.12 FindMissingBlobs contract](https://github.com/bazelbuild/remote-apis/blob/v2.12.0/build/bazel/remote/execution/v2/remote_execution.proto#L350-L362)
explicitly has no method-specific errors. Its
[request definition](https://github.com/bazelbuild/remote-apis/blob/v2.12.0/build/bazel/remote/execution/v2/remote_execution.proto#L1780-L1800)
specifies the digest function and inference when unset, but does not require a
particular response for an undefined enum value. The newer revision linked in
the original report likewise has no rejection requirement. CacheCapabilities
lists supported algorithms; a successful absence lookup does not advertise or
prove support for hashing or storing blobs with algorithm 999. Enum 999 is not
UNKNOWN (0), but that distinction alone supplies no mandatory error contract.
Requiring either of two specific status codes is therefore an unjustified test
oracle. Stricter input diagnostics could be an improvement, but this test does
not establish a correctness defect.

Source at the exact pinned release also disproves the implication that the
algorithm is accepted for storage. The
[CAS handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L121-L146)
constructs resource names and delegates presence checks. It does not infer
SHA256 for explicit 999:
[NewResourceName](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go#L123-L135)
only infers for UNKNOWN. In the default Pebble backend,
[makeFileRecord](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/backends/pebble_cache/pebble_cache.go#L1854-L1861)
validates the digest; [hashLength](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go#L425-L443)
returns -1 for 999, so the 64-character hash fails validation.
[PebbleCache.FindMissing](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/backends/pebble_cache/pebble_cache.go#L1970-L2018)
conservatively includes a resource in the missing list whenever its per-resource
lookup returns an error, including this validation error. Separately,
[HashForDigestType](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go#L367-L386)
rejects unsupported algorithms. Thus the observed RPC success masks a lookup
validation failure; it is not successful use of algorithm 999. Review of the
release's cache/RBE documentation found no promise of the two asserted error
codes for presence checks.

The harness is healthy: the SHA256 missing-blob control passed, capabilities
excluded 999, and fixture-local authentication/readiness succeeded. Both fresh
runs failed only at the asserted rejection status, with OK and the same digest
in missing_blob_digests. There was no timeout or intermittent transport failure.

Executed both bug targets in each invocation using:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:small_splice_bug_test //tests/app/capabilities:unknown_digest_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Independent run 1](https://app.buildbuddy.io/invocation/48fd0cea-192c-4416-a0c8-ec98b991b148): algorithm 999 returned OK/missing; required-error assertion failed.
- [Independent run 2](https://app.buildbuddy.io/invocation/20c506ab-f45c-4667-a249-e9104fbb518b): identical OK/missing behavior and assertion failure.

Both logs identify the exact app digest recorded above, with no image or config
overrides. Docker components ran inside the remote tests; caching was disabled.
The bug test should be retired or replaced with an invariant supported by the
spec (for example, unsupported algorithms cannot successfully store content).
Test changes are outside this verification PR's findings-only scope.
