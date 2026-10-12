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
