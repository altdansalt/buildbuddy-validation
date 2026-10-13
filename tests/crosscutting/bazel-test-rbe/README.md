# Bazel 8 test behavior on the released local stack

`go_default_test` downloads checksum-pinned Bazel 8.4.2 on the hosted runner,
starts the digest-pinned app and executor with the harness, then generates
small dependency-free Starlark shell tests. No nested command contacts an
external execution, cache, or BES endpoint. Nested Bazel ignores all rc files
and receives a restricted environment and only the fixture-local API key.

The logged seed varies shard counts (1–4) and payloads. Each case checks:

- Byte-identical local/remote shard logs, supplied JUnit XML, and the contents
  of nested zipped undeclared outputs.
- Test-result reuse by the same client and a fresh client, including downloaded
  artifacts, and forced execution with `--cache_test_results=no`.
- Streamed marker output and artifact parity. Bazel 8 deliberately executes
  streamed tests locally without sharding, even with a remote executor configured.
- Three exhausted flaky attempts, with all attempt logs returned, and a bounded
  test timeout with matching local/remote classifications.
- `coverage --combined_report=lcov` with synthetic one-line shell instrumentation
  and a tiny reference merger. Every shard contributes one hit, so the exact
  expected combined hit count is known. This tests coverage transport and
  aggregation; it does not test C++/Java coverage instrumentation.

The bare executor image lacks `file` and `zip`. The test binary supplies narrow
helpers for Bazel's undeclared-output wrapper. The fixture mounts them read-only
at the same absolute path used by the local run, with explicit test `PATH`.
The coverage merger travels as a declared action input. No packages are installed
in the released image and no shared harness changes are required.

Run only on hosted runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/bazel-test-rbe/... --config=nobug
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/bazel-test-rbe/... --config=nobug --nocache_test_results --test_arg=-bazel_test_seed=42 --test_arg=-bazel_test_iterations=6
```

The default is seed `314008`, three cases; iteration count is bounded to 1–12
and the entire nested command sequence has a nine-minute context deadline.
Nested command logs and harness image/config/container artifacts are preserved
in undeclared test outputs. Failures print their seed and generated case name.
A suspected product mismatch must be minimized and reproduced twice before
adding a separate deterministic `bug` target and finding.

Behavior references:

- [Bazel test encyclopedia](https://bazel.build/reference/test-encyclopedia):
  sharding environment, XML, undeclared outputs, test environment.
- [Bazel 8.4.2 test wrapper](https://github.com/bazelbuild/bazel/blob/8.4.2/tools/test/test-setup.sh)
  and [coverage wrapper](https://github.com/bazelbuild/bazel/blob/8.4.2/tools/test/collect_coverage.sh).
- [REAPI Action / ActionResult / ExecuteResponse](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto):
  timeout-sensitive action identity, returned output trees, exit status, cache reuse.
- [Released BuildBuddy runner](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/runner/runner.go):
  output directory creation and `UploadOutputs` delegation to the workspace.

Follow-up coverage: successful recovery of a genuinely flaky test (the current
probe checks exhausted retries), real language instrumentation, `--runs_per_test`
cache policy, and more varied undeclared-output names/content types.

Validation: the default three-case suite passed in 131.4 seconds on
[the BuildBuddy test invocation](https://app.buildbuddy.io/invocation/c0127ddb-3c72-4fdc-91d6-351e505e2ed1)
([outer runner](https://app.buildbuddy.io/invocation/7b997a8a-1d11-4538-92f2-f2ac56054db5)).
No product findings were identified. Initial failures were missing bare-image
tools, a fixture Starlark error, and an incorrect streamed-sharding oracle; these
were resolved before the passing run.
