# Execution state conformance

Run `bb remote test //tests/statemachine/execution-state:go_default_test`.
All component calls use disposable fixture-local app, bare executor, and proxy.
Defaults are digest-pinned enterprise-v2.314.0; harness logs record exact images.
The downloaded CLI is release 5.0.492, SHA256
`e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`.

## Mutation / read inventory

Source inspected at released v2.314.0:

| Mutation | Read surfaces | Coverage |
| --- | --- | --- |
| REAPI Execute | Execute stream; REAPI WaitExecution; BuildBuddyService WaitExecution and GetExecution gRPC; UI HTTP GetExecution | Operation identity and monotonic stages, action digest, terminal ExecuteResponse, invocation filter; terminal wait replay twice |
| executor-originated PublishOperation | same operation/read projections; app and proxy GetActionResult, FindMissingBlobs, ByteStream Read; bb download | Real released executor publishes; successful ActionResult, output path/digest/content and stdout; repeated cacheable Execute is a cache hit |
| BuildBuddyService CancelExecutions | original Execute stream; REAPI WaitExecution; BuildBuddyService WaitExecution/GetExecution gRPC and UI HTTP GetExecution | Real BES invocation, executing bare action, HTTP and gRPC cancellation requests, terminal CANCELLED and projection convergence |

The reference state consists of the submitted action digest, immutable operation
name, monotonically observed stage, invocation ID and terminal ExecuteResponse.
Terminal projections are compared with protobuf equality (including result,
status and standard REAPI executor metadata), with bounded polling for database/AC convergence.
UI HTTP checks compare selected JSON fields; gRPC checks compare standard REAPI responses. The release intentionally trims
auxiliary metadata (including AC-added invocation-origin metadata) and BuildBuddy-specific usage timelines from REAPI streams
and action cache entries; the oracle excludes auxiliary metadata and unknown
execution-metadata extension fields from the UI comparison.
The test starts a real BES stream for invocation association and authorization.
No fabricated PublishOperation is sent: the released executor is its producer.

Relevant implementations: `proto/remote_execution.proto`,
`proto/buildbuddy_service.proto`, `proto/execution_stats.proto`,
`proto/invocation.proto`,
`enterprise/server/remote_execution/execution_server/execution_server.go`,
`enterprise/server/execution_service/execution_service.go`,
`server/buildbuddy_server/buildbuddy_server.go`, and `cli/download/download.go`.
Cache proxy exposes cache reads, not execution state mutation/projection APIs.

## Limits

SearchExecution and GetExecutionTimeline require the execution search service
and an OLAP database; the baseline SQLite fixture does not configure them.
They are not treated as inconsistent surfaces. Adding an OLAP sidecar is a
shared fixture extension outside this item's scope.

Intermediate queued/executing notifications are observed on the Execute stream;
this suite does not force a stable queued state to compare every projection.
The cancellation test waits on EXECUTING and verifies it in HTTP/gRPC projections
rather than using an elapsed-time delay.
The executor's shell uses sleep solely as a cancellable workload, not as a
synchronization mechanism in the test. There is no delete execution RPC.
Cancellation of merged actions, workflow child invocations, and multiple executor
attempts are not covered. The CLI executable download needs public GitHub access.
