# App HTTP surface

All component requests address the app fixture. CAS data is written through the
fixture-authenticated ByteStream client and downloaded from the same app using a
disposable API key in the bytestream URL (the server's internal gRPC dial needs
credentials independently of the browser cookie).

`http_test` covers health/readiness server-type selectors, Prometheus text
metrics, exact 8 MiB downloads and attachment headers, four concurrent downloads,
Range headers, malformed query parameters, missing CAS objects, malformed JSON/
protobuf/framed HTTP RPCs, unknown RPC methods, and a successful framed GetUser
request with a gRPC-web-format success trailer. Saved container logs are checked
for panic markers after harness cleanup.

The released app's UI bridge uses `application/proto+prefixed`; this is the
actual UI protocol rather than assuming an `application/grpc-web` endpoint.
Range may be ignored per RFC 9110 section 14.2; a 200 must return the complete
object, while a 206 must provide a consistent Content-Range and exact slice.
Both health and readiness require `server-type=app`. The CAS miss fallback
without invocation_id returns 400 because the artifact fallback requires that
parameter; that existing source behavior is covered separately from the
nonexistent-invocation 500 repro.

## References

- [Pinned release source](https://github.com/buildbuddy-io/buildbuddy/tree/v2.314.0)
- [Download handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/buildbuddy_server/buildbuddy_server.go)
- [HTTP RPC bridge](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/http/protolet/protolet.go)
- [Health selectors](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/healthcheck/healthcheck.go)
- [Monitoring](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/monitoring/monitoring.go)
- [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
- [HTTP Range](https://www.rfc-editor.org/rfc/rfc9110.html#section-14.2)

## Run

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote --runner_exec_properties=init-dockerd=true test //tests/app/http-endpoints/... --config=nobug
GIT_CONFIG_GLOBAL=/dev/null bb remote --runner_exec_properties=init-dockerd=true test //tests/app/http-endpoints/... --test_tag_filters=bug --nocache_test_results --test_output=all
```

Metrics uses a test-reserved loopback port published with DockerArgs because the
harness does not expose monitoring endpoints. A startup port collision is an
environment failure. The prestarted daemon avoids repeatedly pulling Redis into
private daemons; one private-daemon attempt hit Docker Hub's anonymous rate limit.

Follow-ups: successful persisted-invocation artifact downloads, ZIP member
selection, inline MIME types, HEAD, cancellation, and monitoring authentication.
