# Invocation API conformance

These tests start only the digest-pinned enterprise app and its fixture-local
Redis sidecar. All BES, UI protolet, and public v1 API requests address the
fixture. The API key is created by the fixture; no runner credentials are used.

Contracts are taken from BuildBuddy [v2.314.0](https://github.com/buildbuddy-io/buildbuddy/tree/v2.314.0):
`proto/build_event_stream.proto`, `proto/invocation.proto`, `proto/eventlog.proto`,
`proto/target.proto`, and `proto/api/v1/{invocation,log,target}.proto`.
BES uses Google's generated PublishBuildEvent client. Small Bazel event payloads
are encoded directly using those pinned field numbers, avoiding a shared module
change or a BuildBuddy source build.

The [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)
only defines invocation correlation through RequestMetadata.tool_invocation_id;
it does not define GetInvocation, search, or build log pagination. Those APIs
are checked against BuildBuddy's own published protos and released source.

Run passing coverage:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/invocations-api/... --config=nobug
```

Run the multi-chunk log pagination reproducer:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/invocations-api:log_pagination_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

Round 2 regression coverage adds:

- A premature BES half-close and a transport cancellation, each observed as
  partial before interruption and disconnected afterwards. A sequence-1 replay
  completes the same invocation and replaces the abandoned attempt's log. A
  suffix retry after completion leaves its status and log intact.
- Four concurrent readers calling UI GetInvocation, GetTarget and
  SearchInvocation while BES uploads 257 test targets. Reads must succeed and
  GetInvocation must remain partial until the stream finishes.
- All 257 target labels across the UI's 12-target pages, checking total counts,
  duplicate labels, token progress and termination. The public v1 GetTarget
  listing must return all targets (the released implementation does not paginate
  that endpoint).

All RPC streams have bounded contexts and HTTP calls use the harness or an
explicit client timeout. No shared deployment is contacted by component calls.

Round 2 validated against the default app digest in
[the passing area suite](https://app.buildbuddy.io/invocation/761fe64e-3781-4711-ab23-4c132ecc90ec).
The runner used `bb remote --runner_exec_properties=init-dockerd=true test
//tests/app/invocations-api/... --config=nobug --test_output=all` after an owned
fixture daemon encountered Docker Hub's unauthenticated Redis pull rate limit.
No new product finding was observed in these cases; the round 1 log pagination
reproducer remains excluded by `--config=nobug`.
