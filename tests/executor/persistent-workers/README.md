# Released executor persistent workers

The test uploads a small statically linked Go worker as an executable CAS action
input. It needs no toolchain or interpreter inside the executor image. Both JSON
and unsigned-varint-delimited protobuf implement Bazel's worker protocol; digest
tokens are opaque, and responses echo request IDs. Ordinary execution of the same
binary is the reference model for arguments and input contents.

Coverage:

- Seeded randomized UTF-8 input contents and arguments, including empty strings,
  quotes, backslashes, whitespace, and changing file sizes. The first two cases
  force empty and 1024-rune contents.
- JSON/proto results equal ordinary execution, and equal the supplied inputs.
- Sequential actions with the same platform key reuse the PID and increment the
  request counter; a different key starts a separate worker. Automatically derived
  keys reuse workers too, and protocol properties isolate identical explicit keys.
- Exit mid-request fails the action; the next action succeeds on a fresh worker.
- Four concurrent actions per protocol keep their inputs and responses separate.
  This worker can accept and echo nonzero IDs, but the released executor sends
  singleplex ID zero. Concurrent actions do **not** prove multiplex dispatch.

Run:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/persistent-workers/... --config=nobug
```

Replay/extend with `--test_arg=-workers.seed=314` and
`--test_arg=-workers.iterations=20` (bounded to 1–50 cases per protocol). Default
is seed 314 and eight cases per protocol. Every randomized subtest logs its seed,
iteration and protocol. Action RPCs have 60-second contexts and 20-second action
timeouts. Fixtures and component logs are preserved by the harness.

Behavior was checked against released source:

- [persistentworker.go at v2.314.0](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/persistentworker/persistentworker.go)
- [runner keys at v2.314.0](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/runner/runner.go)
- [platform properties at v2.314.0](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go)
- [Bazel worker protocol](https://github.com/bazelbuild/bazel/blob/master/src/main/protobuf/worker_protocol.proto)
- [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)

The released worker implementation constructs requests without request IDs and
waits for a response before completing the action. True multiplex scheduling and
cancellation are therefore not asserted as supported features. No dependencies
or shared harness modifications are needed.

## Validation

The final area suite passed in 137.6 seconds on digest-pinned enterprise-v2.314.0
with seed 314, eight cases per protocol, and canonical REAPI platform ordering:
[full suite invocation](https://app.buildbuddy.io/invocation/8d4287dd-bc51-4655-a29d-e76bbffcbeee).
The suite executed 60 fixture-local actions, including 16 randomized cases
compared against ordinary execution. Earlier focused validation:
[reuse/input updates](https://app.buildbuddy.io/invocation/09638984-0e69-47b6-a452-86870a00348f),
[crash recovery](https://app.buildbuddy.io/invocation/186438c5-89ff-4edd-a913-f041ec293306),
and [concurrency/key isolation](https://app.buildbuddy.io/invocation/f43fd813-8a0d-4532-aaef-845739d77dcb).
No suspected defects were observed, so no bug-tagged targets or findings were added.
