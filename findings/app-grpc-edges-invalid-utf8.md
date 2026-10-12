# App accepts invalid UTF-8 in a proto3 gRPC request string

Title: GetCapabilities accepts invalid UTF-8 in instance_name
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: A client can submit a malformed proto3 string that the app accepts as a successful request. This violates the protobuf wire contract and differs from the standard Go decoder. The reproduced capabilities call is read-only; no crash, authorization bypass, persistence corruption, or broader service impact is demonstrated.
Status: unverified
Repro test target: //tests/app/grpc-edges:invalid_utf8_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/f9c26300-2a84-4d7c-b562-d5d8caa688dd

## Expected behavior

Proto3 `string` fields must contain UTF-8 or ASCII text, according to the
[protobuf scalar type contract](https://protobuf.dev/programming-guides/proto3/#scalar).
The released [Remote Execution API proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/remote_execution.proto)
declares `GetCapabilitiesRequest.instance_name` as proto3 `string`, field 1;
the [upstream REAPI](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)
agrees. The request payload `0a 01 ff` is therefore malformed. The
[gRPC status mapping](https://github.com/grpc/grpc/blob/master/doc/statuscodes.md)
specifies INTERNAL for an error parsing the request proto.

## Actual behavior

The app returns gRPC OK with populated cache capabilities for `0a 01 ff`.
The reproducer sends the bytes with a raw client codec named `proto`, bypassing
client-side validation while retaining the ordinary gRPC protobuf content type.
The standard Go protobuf decoder rejects these exact bytes in a local control
inside the test. A valid `0a 01 61` request succeeds against the same connection,
method, credentials, and raw codec before the malformed request is sent.

Two independent uncached executions of the isolated bug target reproduce the
acceptance: [run 1](https://app.buildbuddy.io/invocation/f9c26300-2a84-4d7c-b562-d5d8caa688dd)
and [run 2](https://app.buildbuddy.io/invocation/5423ef05-236d-4feb-91cf-53e8e117bcae). Earlier exploratory coverage also reproduced it in
[e5bf270f-2a12-4f04-9229-94c691e6dc2f](https://app.buildbuddy.io/invocation/e5bf270f-2a12-4f04-9229-94c691e6dc2f)
and [aa99ca0f-64ff-4ac4-90bb-bd5af5331cb4](https://app.buildbuddy.io/invocation/aa99ca0f-64ff-4ac4-90bb-bd5af5331cb4).

Harness/config problems were ruled out by the valid-wire control, standard-decoder
negative control, exact digest reported by the fixture, and the independently
passing transport suite, which rejects invalid tags and truncated protobufs.
Only the fixture's published loopback gRPC endpoint and disposable API key are used.
No receive-size, codec, or authentication overrides are configured on the server.
Status remains unverified for maintainer review as requested.

## Logs excerpt

Test output from the isolated reproducer:

```text
--- FAIL: TestRejectInvalidUTF8 (16.17s)
invalid UTF-8 instance_name: got status OK (<nil>), response cache_capabilities=true; want INTERNAL request-proto parsing failure
```

This is a successful-response conformance defect, not an inferred panic from
an INTERNAL error. The fixture preserves container logs and inspect metadata
in the test's undeclared outputs.

## Notes

Reproduce with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/grpc-edges:invalid_utf8_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

Uses default `harness.Options{Components: []harness.Component{harness.App}}`,
including local Redis, SQLite, Pebble, self-auth and a fixture-local API key.
No version overrides. The ordinary regression suite passes in
[1ad1c3f3-042a-4da7-90a8-23a92004d997](https://app.buildbuddy.io/invocation/1ad1c3f3-042a-4da7-90a8-23a92004d997).

The released [VT gRPC codec](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/vtprotocodec/vtprotocodec.go)
delegates decoding to `UnmarshalVT` for supported messages. This is a possible
cause; the generated decoder was not separately built or tested. Validate known
string fields in the server decoder to restore consistent protobuf behavior.
Other methods, invalid-string persistence, and cross-language effects remain
follow-ups. Findings stay in this repository; no upstream report was sent.
