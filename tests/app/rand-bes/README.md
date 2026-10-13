# Randomized BES round trips

`go_default_test` starts only the digest-pinned released enterprise app and its
fixture-local Redis. It generates seeded complete Bazel event graphs, publishes
ordered BES envelopes, checks every acknowledgement, and reads back invocation
metadata, per-target test results/summaries, and the complete console log. It then
republishes each stream with the same invocation/stream ID and verifies the same
API-visible data without duplicated results or logs.

The generator varies target labels/counts, runs per test, pass/fail outcomes,
local-cache markers, UTF-8 status details, log lengths and arbitrary progress
fragment boundaries on stdout or stderr. The first iteration guarantees 17
passing targets (crossing the 12-target API page size) and over 2 MB of console
output (crossing the stored-log chunk size). Other iterations may include empty
target sets. Status listings are traversed and checked for missing/extra labels,
duplicates and consistent totals. The app's raw event storage is forced to
use 1 KiB chunks. Console text deliberately uses short plain lines: terminal
escape processing and line wrapping are transformations, so arbitrary terminal
bytes do not have a byte-for-byte round-trip contract.

The default seed is 314159 with 12 iterations. Replay or extend on hosted runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/rand-bes/... --config=nobug \
  --nocache_test_results --test_output=all \
  --test_arg=-rand_bes.seed=42 --test_arg=-rand_bes.iterations=50
```

Iterations are limited to 200; RPCs have deadlines, chunk traversal is bounded,
and a four-minute generation/test budget prevents unbounded extended runs.
Every subtest prints its seed, iteration, invocation ID and event count.

The small wire encoder uses the released schema without a BuildBuddy source
build or a new dependency. Contract references:

- [v2.314.0 BES schema](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/build_event_stream.proto): event IDs, payloads, child announcements, `last_message`.
- [Invocation API](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/invocation.proto): target and progress events are intentionally excluded from invocation event listings.
- [Target API](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/target.proto): test result events and test summaries.
- [Event log API](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/eventlog.proto): last-chunk lookup and reverse traversal.
- [Released BES handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_handler/build_event_handler.go): finalized invocations discard replay attempts; stderr precedes stdout within a progress event.
- [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto): defines execution/cache RPCs, not BES payloads; BES correctness is governed by the schemas above.

No shared deployment is addressed by component requests. The outer dispatch
credential is never read or forwarded by these tests.

Round 2 adds 1–4 shards per target and flaky targets with 2–4 attempts for each
run/shard pair. Flaky histories contain failed attempts followed by a passing
attempt; their aggregate summary and target status must remain `FLAKY`.
Every result ID (including run, zero-based shard and attempt), payload and summary
count is checked against the generated stream. Status-filtered pages and detailed
target responses must agree with the same oracle for `PASSED`, `FAILED` and
`FLAKY`, including empty groups.

Even-numbered iterations close a seeded prefix of the stream before its finish
event, wait for the invocation to become disconnected, and reconnect by replaying
from sequence 1. Complete results and console logs must contain exactly the final
stream's data; the existing finalized-stream replay checks still apply. Prefix
lengths use the same seed, and are printed in test output. This models an orderly
transport interruption without a component-stream-finished envelope, rather than
injecting a network partition or asserting unsupported continuation at a sequence
number greater than 1.
