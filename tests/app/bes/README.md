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

Round 2 adds `round2`, covering canceled uploads followed by a full replay
(seed `20261012`, four random prefix cut points). Readback checks completion and
exactly one copy of each progress marker after retry. No partial ACK is assumed:
the server emits ACKs only after the client closes the send side. Retrying starts
at sequence 1 because the canceled upload has no committed ACKs.

The boundary coverage measures the complete protobuf request at 49,999,999,
50,000,000 and 50,000,001 bytes, around the released default receive limit.
The first two must complete and persist a successful invocation; the last must
return ResourceExhausted without ACKs, followed by a healthy upload.

The bug-tagged `mixed_identity_persistence` extends the existing identity defect:
progress
and finish labeled with a second invocation are stored under the first ID,
while the second ID remains NotFound. This is a characterization of the same
fixture-local, same-group behavior; it demonstrates no cross-group effect.
It expects rejection and forbids second-ID events in the first invocation.
The original `changed_identity` and `fresh_offset` bug tests are unchanged.
Missing-envelope and malformed-wire coverage requires rejection without ACKs
and logs observed status codes. The current release returns FailedPrecondition
and Unknown respectively; BES does not explicitly mandate either code.

`invalid_sequence_code` is a separate bug-tagged proposed canonical-error repro:
gaps and duplicates return Unknown rather than InvalidArgument. The finding
stays unverified pending independent review; BES explicitly mandates rejection,
but does not explicitly choose a gRPC code. Run it with
`--test_tag_filters=bug --nocache_test_results`.
