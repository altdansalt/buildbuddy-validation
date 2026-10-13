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

## Round 2

`download_test` adds fixture-local persisted artifact downloads: it publishes a
three-event BES invocation, closes the stream, then checks raw JSON event count,
raw protobuf event fields (accounting for option redaction and progress moved
to the log), and progress text in the persisted build log.
ZIP coverage downloads the entire archive and selects empty/stored/deflated
members with release-compatible manifest references, comparing exact bytes.
It also compares HEAD status and representation headers against GET, checks
inline MIME/disposition headers, and cancels three 8 MiB transfers after reading
1 KiB before checking a complete download and readiness. Cancellation coverage
checks recovery and data integrity, not server-side resource reclamation.
Random payloads use the logged fixed seed `20261013` for replay.

The released MIME allowlist intentionally defaults text, HTML and JavaScript to
`application/octet-stream`; image filenames retain image MIME types. ZIP
manifest references include the local file header offset; omitting that offset
is a malformed test input. Neither behavior is recorded as a product finding.
No new suspected defects were identified in round 2.

Follow-ups: monitoring authentication, persisted CAS fallback after eviction,
and server-side resource accounting after cancellation.

Round 2 validation (pinned enterprise-v2.314.0):
- [Download follow-ups](https://app.buildbuddy.io/invocation/fb22517c-7201-4a7c-89da-0f517e82926c).
- [Final area suite, both passing targets](https://app.buildbuddy.io/invocation/9b5e98da-34e7-4574-aa4d-cf690ffad21e), with `--config=nobug --nocache_test_results`.
