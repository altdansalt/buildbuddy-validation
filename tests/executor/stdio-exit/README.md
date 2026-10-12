# Executor stdio and exit results

Run `bb remote test //tests/executor/stdio-exit/... --config=nobug`.
The fixture starts only disposable app, Redis, and executor containers. Actions
use the pinned release's bare runner and authenticated fixture-local RPCs.

Coverage includes every byte value, embedded NULs, empty and asymmetric streams,
concurrent stdout/stderr writers, outputs exceeding the usual 4 MiB gRPC message
size, exits 0/1/42/255, and self-delivered TERM/KILL/ABRT. Stream assertions verify
SHA256 and size, CAS readback, and any inline bytes without assuming that the
server must inline. Successful results are also fetched from the action cache
with both inline flags on and off. Signal cases verify diagnostic preservation
and the pinned bare runner's negative exit/non-OK status convention; REAPI does
not standardize POSIX signal encoding.

Contract and implementation references:

- [REAPI ActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto):
  stdout/stderr digests identify CAS blobs; inlining is optional even on request.
- [v2.314.0 workspace upload](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go):
  `UploadOutputs` independently uploads both streams and sets their digests.
- [v2.314.0 command exit handling](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil.go):
  `ExitCode` distinguishes normal exit from signal termination.
- [v2.314.0 executor finalization](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/executor/executor.go):
  outputs are uploaded even when command execution returns an error.
