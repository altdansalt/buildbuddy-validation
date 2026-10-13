# CLI download cannot observe a namespaced splice

Title: bb download rewrites a documented namespaced resource and misses its splice manifest
Components: app / cacheproxy / bb CLI
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
CLI binary SHA256: `dbaa1fa5cb256ca60bdac9817a2ec826d092b306ba986689193921caae94ac8b`
Severity: low
Severity justification: Valid namespaced manifest-only CAS objects cannot be downloaded through the CLI. Direct gRPC and web downloads work; no data loss or authorization boundary violation was observed.
Status: confirmed
Repro test target: //tests/statemachine/convergence-metadata:cli_namespace_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/f2e18b72-8a1e-43e4-a7de-8b29ea032040
Second reproduction: https://app.buildbuddy.io/invocation/b5c4e669-8885-47dc-b804-4f419522ae15

## Expected behavior

After successful SpliceBlob in instance `cli-splice-model`, CLI download of
`/cli-splice-model/blobs/HASH/5242880` should return the same bytes as ByteStream.Read
through either app or proxy. The released `cli/download/download.go` help explicitly
shows `/foo/blobs/HASH/SIZE` as an example with a remote instance name.

## Actual behavior

The isolated test uploads 3 MiB and 2 MiB chunks, then splices their valid SHA256
whole-blob digest. Direct app and proxy gRPC reads both reconstruct all bytes.
CLI download through both endpoints consistently returns FailedPrecondition with
a manifest-key NotFound. Eleven attempts, spaced one second apart, failed on
both independent fixtures:

| Run | App last observed failure | Proxy last observed failure |
| --- | --- | --- |
| f2e18b72 | approximately 12.5 s | 12.523 s |
| b5c4e669 | 12.472 s | 12.436 s |

This is permanent for this CLI resource parsing path, rather than measured
replication lag: source prepends `/blobs/` unless the argument already starts
with `/blobs`, even for a documented `/INSTANCE/blobs/...` resource. The server
therefore receives a different instance. The resource parser parses backwards
and treats the extra leading path as part of the instance. CAS deduplication
masks the mistake for materialized ordinary blobs; splice manifests retain the
instance and expose the incorrect identity.

No non-default component config is required. Correct gRPC controls use the same
fixture-local API key, digest, instance and endpoints; the green conformance
suite also checks HTTP reconstruction and both compressed and identity reads.
The original CLI probe without a leading slash failed similarly in
https://app.buildbuddy.io/invocation/d44912df-37dc-4703-a64b-e4e514f65bec.

## Logs excerpt

```text
CLI binary sha256: dbaa1fa5cb256ca60bdac9817a2ec826d092b306ba986689193921caae94ac8b
app attempt=10 elapsed=12.472281942s error=exit status 1
rpc error: code = FailedPrecondition desc = read db4a1c09693302cc2ec5f590e071001e65e103b500449c882218ad65e037646f/5242880:
rpc error: code = NotFound desc = key "PTdefault/4ac1829ce1f57c8b301670daf79c5bea4062ea49d8076d71793906c27c69bafe/1/ac/v5" not found
cacheproxy attempt=10 elapsed=12.435555733s error=exit status 1
```

## Notes

Reproduce with:

```sh
bb remote test //tests/statemachine/convergence-metadata:cli_namespace_repro --test_tag_filters=bug --nocache_test_results --test_output=all
```

The target intentionally asserts correct behavior and fails until fixed. The
`bug` tag excludes it from default CI; the green target omits CLI reads for the
5 MiB manifest-only objects and retains their gRPC/HTTP checks. No expected
failure is silently accepted in the green target.

This finding identifies a CLI surface divergence, not a server replication bug.
The runner CLI is installed externally to the fixture. Its binary hash is fixed
across the two runs above; its semantic version query cannot initialize in the
Bazel test environment without HOME/XDG configuration, and that ancillary query
failure is logged, not claimed as a product bug. The known failure is the later
successful RPC dispatch to a wrong resource namespace. A coordinator-provided
pinned CLI runfile would improve version reproducibility.

All components run inside remote test fixtures. No host component test, fault
injection, image override, shared service request, upstream issue or PR was used.
