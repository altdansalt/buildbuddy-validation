# BES ingestion

These tests call only a digest-pinned enterprise-v2.314.0 app started by the
fixture. All IDs and credentials are disposable. No executor or proxy is needed.

`regression` covers complete uploads with invocation readback, complete and
suffix retries, out-of-order uploads with ordered ACKs, competing uploads of the same invocation, empty streams,
missing Bazel start/finish payloads, an 8 MiB progress payload, sequence gaps,
duplicates, missing request envelopes, and malformed protobuf bytes followed by a healthy upload.
Missing start/finish coverage records the app's tolerant ACK behavior; it does
not assert that such invocations are complete or persisted. Container logs are
checked for Go panic/fatal-error markers after fixture cleanup.

`fresh_offset` checks that a new invocation starting at sequence 2 is rejected.
`changed_identity` checks that an RPC cannot switch invocation/stream IDs.
Both are separate bug reproducers; run explicitly with
`--test_tag_filters=bug --nocache_test_results`.

The tiny Bazel payloads are encoded using protobuf wire helpers from the pinned
release's [BuildEvent schema](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/build_event_stream.proto).
This avoids requiring changes to the shared module or generated bindings.
The service contract is in the release's
[publish_build_event.proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto):
events in one streaming RPC belong to the same stream, sequence numbers start
at one, and ACKs identify committed events. The
[Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)
does not define BES semantics; BES uses google.devtools.build.v1 instead.
