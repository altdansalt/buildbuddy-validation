# Bazel → cache proxy → app → executor

`//tests/crosscutting/e2e-proxy-exec:go_default_test` starts the three pinned
released containers and runs Bazel 8.4.2 as a client inside the hosted test runner.
The downloaded client is checked against the release SHA256 checksum.

The supported topology has separate RPC routes: Bazel cache operations use the
proxy, Execute uses the app, and executor cache operations use the proxy. The
proxy does not implement the Execution service; lack of that service is not a bug.
No remote operation targets a shared deployment. Local execution fallback is
disabled, and each build gets a fresh output base.

Coverage checks cold execution, warm action-cache reuse, invalidation after an
input change, reuse after reverting that change, byte-exact 1.28 MB binary outputs,
empty outputs, output CAS visibility through both app and proxy, and repeated
nonzero action exits. Execution logs distinguish actual execution from cache hits.

Run with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/e2e-proxy-exec/... --config=nobug --test_output=all
```

Contract references:

- [REAPI ActionCache and Execution](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto): action digests include input-root digests; successful cached results reference CAS outputs.
- [Released proxy service registrations](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/cmd/cache_proxy/cache_proxy.go).
- [Released executor cache routing](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/cmd/executor/executor.go).
- [Released app cache lookup and result publication](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go).

The test is intentionally a sequential state-transition check. Cancellation,
concurrent action merging, directory/symlink outputs and container restarts are
follow-up coverage.

Validation on the pinned release: [passing hosted invocation](https://app.buildbuddy.io/invocation/f909d8fb-dfab-46c6-88f5-7529205cef7b)
([outer runner](https://app.buildbuddy.io/invocation/1192354f-acb0-4b86-8caa-1112dcbf4def)).
All six cases passed; no product finding was observed.
