# Unfinished ByteStream writes close successfully without a response message

Title: ByteStream Write omits WriteResponse when request channel closes without finish_write
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: An interrupted-upload client cannot receive committed progress on graceful request-channel closure and instead receives a client-generated INTERNAL error. Restarting from zero recovers; no stored-content corruption or authorization failure was observed.
Status: unverified
Repro test target: //tests/crosscutting/rand-cas:unfinished_write_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/db538117-7b66-40d1-98b1-c5e332d796f0

## Expected behavior

The [ByteStream Write contract](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto)
says: "When the client closes the request channel, the service will respond
with a WriteResponse." The resource is incomplete until finish_write is true.
Send a single byte `a` for the SHA256 digest of `ab`, then half-close the
request stream without finish_write. The response should describe committed
progress without finalizing the blob. A legitimate server error should use
non-OK gRPC status rather than an OK response with no message.

## Actual behavior

Both endpoints send no response message, causing Go gRPC CloseAndRecv to return
INTERNAL: `cardinality violation: received no response message from
non-server-streaming RPC`.

Observed in two independent full property-suite invocations:
[c1912352](https://app.buildbuddy.io/invocation/c1912352-0fb1-4bfd-a654-3fc429bcc869)
and [d5a0fa1a](https://app.buildbuddy.io/invocation/d5a0fa1a-afc1-422d-ae81-f53d6d2f7d3f).
The first used the previous harness; the second used main's mirrored Redis
image. Both fixtures reached readiness, authenticated local clients, and passed
ordinary CAS agreement and concurrent rewrite checks before reporting this
failure. The isolated two-byte target also reproduces:
[db538117](https://app.buildbuddy.io/invocation/db538117-7b66-40d1-98b1-c5e332d796f0).

[Released app source](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go)
returns nil on request-stream EOF in Write, without calling SendAndClose.
This explains the missing response and distinguishes it from transport loss,
unsupported auth or fixture startup problems. Proxy behavior may originate in
forwarding the app response; this is not a claim of an independent proxy defect.
Status remains unverified pending coordinator/maintainer review.

## Logs excerpt

```text
--- FAIL: TestUnfinishedWriteReturnsResponse/app
  half-close without finish_write: rpc error: code = Internal desc = cardinality violation: received no response message from non-server-streaming RPC; want WriteResponse
--- FAIL: TestUnfinishedWriteReturnsResponse/cacheproxy
  half-close without finish_write: rpc error: code = Internal desc = cardinality violation: received no response message from non-server-streaming RPC; want WriteResponse
```

Fixture configs, container logs and inspect metadata are preserved in test
undeclared outputs.

## Notes

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-cas:unfinished_write_bug_test --test_tag_filters=bug --nocache_test_results --test_output=errors
```

Default fixture configuration, no version overrides. All calls use test-local
endpoints and disposable API keys. QueryWriteStatus is separately UNIMPLEMENTED
in this release; lack of persistent resumption is not filed as another bug.
The passing interrupted-upload test logs terminal errors, queries progress,
restarts at zero when unsupported, and checks final content through both peers.
