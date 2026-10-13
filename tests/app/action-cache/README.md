# App ActionCache validation

All RPCs address a fixture-local app using the harness's disposable API key.
Actions, Commands and input roots are uploaded before updating results. The
fixture uses the default digest-pinned enterprise-v2.314.0 app and Pebble cache.

Run regression coverage:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/action-cache/... --config=nobug
```

Run the defect reproducers (each is a separate `bug` target):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/action-cache:missing_streams_test //tests/app/action-cache:malformed_result_test --test_tag_filters=bug --nocache_test_results
```

Coverage includes instance separation, sequential and concurrent overwrite,
requested inline file contents, read requests without inlining, empty files,
inline size limits, missing output files / Trees / root and child files,
malformed Tree bytes, repair of missing file references, and malformed RPC
arguments. Inline hints may be ignored under REAPI; the small-file inlining
check is regression coverage for the behavior implemented by this release.

Contract references:

- [REAPI ActionCache and ActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).
  GetActionResult SHOULD ensure availability of referenced CAS blobs.
- [Pinned app ActionCache implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go).
  ValidateActionResult checks files and Trees but omits stdout/stderr, and only
  adds output file digests with strictly positive sizes to its availability check.
- [Pinned digest validation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go).
  Negative sizes are explicitly rejected for request digests.

The findings describe the two bug targets. Missing file/Tree references are
allowed on update and rejected on read; update-time existence checking itself
is not asserted. Missing stdout/stderr blobs are a SHOULD-level conformance
finding, not a claim that every REAPI server must reject them on update.
