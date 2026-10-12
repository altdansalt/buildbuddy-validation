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

Round 2 adds `TestRound2`: two simultaneous clients building the same target,
clients with overlapping dependency graphs, SIGINT after a slow remote action
starts, directory outputs with nested binary/empty files and a relative symlink,
a symlink output pointing into that directory, and proxy/app/executor restarts
between builds. Every scenario ends with a fresh no-cache build followed by a
fresh cached rebuild. Execution logs assert remote execution and cache-hit state;
recursive snapshots compare all file bytes, directories and symlink destinations.
Restarts also compare cached results directly with the pre-restart outputs.

The cancellation synchronization marker lives only in the fixture executor's
`/tmp`; it confirms execution has begun before sending SIGINT to Bazel. Restart
commands retain the fixture volumes and wait for component readiness. Docker
commands locate the daemon by the exact fixture container, supporting both the
runner daemon and the harness-owned fallback. No fixture key or worker credentials
are inherited by the Bazel client.

Validation on the pinned release: [passing hosted invocation](https://app.buildbuddy.io/invocation/f909d8fb-dfab-46c6-88f5-7529205cef7b)
([outer runner](https://app.buildbuddy.io/invocation/1192354f-acb0-4b86-8caa-1112dcbf4def)).
All six cases passed; no product finding was observed.
