# Missing command errors identify the uploaded action instead

Title: Execute reports the action digest as missing when only the command is absent
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: A client repairing CAS inputs from structured MISSING violations uploads the already-present action instead of the absent command, so retries cannot repair the execution. This requires a missing/evicted command blob; there is no demonstrated security boundary impact.
Status: confirmed
Repro test target: //tests/app/execution-api:missing_command_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/9edae560-5330-49aa-bcb0-c1e7730ef180

## Expected behavior

The [REAPI Execution contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)
requires FAILED_PRECONDITION for missing inputs/commands and recommends a
PreconditionFailure with MISSING violations naming each absent blob. Once the
server emits this detail, its subject must identify the absent command:
`blobs/7ebbf16769265803a4ce811ff293c60b82f61e061dade999843cf9299dda5745/33`.

## Actual behavior

Execute returns FAILED_PRECONDITION with a MISSING violation naming the uploaded
Action: `blobs/2b69dd99eb501d936e61aeed1c8ef18f94d46051cc831a15c623b2c6b86fb0b1/138`.
The human-readable error also incorrectly says this action is not found.

The reproducer uploads the input root and Action, intentionally omits the
Command, then calls FindMissingBlobs on all three digests. The CAS response
confirms that only the Command is missing before Execute is called. All RPCs
use the same fixture-local app, instance and disposable group/API key. No
executor is needed: rejection happens while dispatch fetches the command.

Reproduction 1: [9edae560](https://app.buildbuddy.io/invocation/9edae560-5330-49aa-bcb0-c1e7730ef180).
Reproduction 2: [dbcc58a4](https://app.buildbuddy.io/invocation/dbcc58a4-cf9e-4c21-8bab-01579f82eab9).
Both fresh fixtures fail at the same incorrect MISSING subject assertion. Independent review confirms the defect; see Verification below.

## Logs excerpt

```text
action=2b69dd99eb501d936e61aeed1c8ef18f94d46051cc831a15c623b2c6b86fb0b1/138
command=7ebbf16769265803a4ce811ff293c60b82f61e061dade999843cf9299dda5745/33
error=rpc error: code = FailedPrecondition desc = Digest hash:"2b69dd99eb501d936e61aeed1c8ef18f94d46051cc831a15c623b2c6b86fb0b1" size_bytes:138 not found
expected: ["blobs/7ebbf16769265803a4ce811ff293c60b82f61e061dade999843cf9299dda5745/33"]
actual:   ["blobs/2b69dd99eb501d936e61aeed1c8ef18f94d46051cc831a15c623b2c6b86fb0b1/138"]
```

## Notes

Uses default harness app configuration, SQLite, Pebble and local Redis, with no
image overrides or shared services. Container logs/config/inspect are saved by
the harness in test undeclared outputs. The excerpt above is the client-observed
RPC response and assertion, not a server log.

[Released source](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go#L1997)
corroborates the behavior: fetchCommand converts NotFound using
`digest.MissingDigestError(actionResourceName.GetDigest())` rather than
`cmdDigest`. A client workaround is independently checking command presence
rather than trusting this repair hint. Findings remain in this repository.

## Verification

Independent review on 2026-10-12: **confirmed**. Medium severity is retained:
a structured repair hint identifies the wrong blob and can prevent targeted
re-upload/retry from repairing execution after a missing command. The test
proves the incorrect hint, not a production retry loop or security impact.

### Spec, source and alternative explanations

The pinned [REAPI Execute contract, lines 87–111](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L87)
describes FAILED_PRECONDITION for missing commands and says the server SHOULD
include MISSING violations identifying absent blobs. Details are recommended,
not universally mandatory; the defect here is that the detail BuildBuddy does
emit falsely identifies a present blob. SHA256 subjects correctly omit the
digest-function and instance-name prefixes, so the expected `blobs/hash/size`
format is not a spec-reading error.

In [v2.314.0 fetchCommand, lines 1997–2008](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go#L1997),
the CAS lookup uses `cmdDigest`, but the NotFound branch calls
`digest.MissingDigestError(actionResourceName.GetDigest())`. That helper
[constructs the MISSING subject directly from its argument](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go#L729).
Thus the wrong subject and error text follow directly from the released source,
without depending on scheduling or an executor. The [pinned troubleshooting
docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/troubleshooting-rbe.md#L74)
explain genuine missing/evicted blobs and repair/retry, but do not document
reporting a different, present blob as missing.

The test uses one client, instance, app and disposable local group key for
uploads, FindMissingBlobs and Execute. It checks both hash and size of the sole
missing blob before Execute; the action and root are present. Auth and CAS
failures would fail earlier assertions. The digest is SHA256 and can be inferred
when Execute omits DigestFunction. The deliberately absent command digest is
computed from placeholder bytes rather than a serialized Command; those bytes
are never uploaded or parsed, so this does not explain substituting the action
digest on a failed CAS lookup. A serialized command would improve realism but
is unnecessary for this missing-blob assertion. No executor is required because
the app fails fetching the command before dispatch.

### Independent reproductions

Reviewed and ran the current PR #8 head
`31f88729e7b119c6eacf079c2d35f6a050433e52` in a separate checkout, without
changing tests or harness. Only the finding is copied into this verification PR.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/execution-api:missing_command_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Both uncached runs start fresh local app fixtures using the exact app digest
listed above. Each reaches FAILED_PRECONDITION with the same incorrect MISSING
subject: action `2b69dd99eb501d936e61aeed1c8ef18f94d46051cc831a15c623b2c6b86fb0b1/138`
instead of command
`7ebbf16769265803a4ce811ff293c60b82f61e061dade999843cf9299dda5745/33`.
The pre-Execute presence checks pass in both runs. This is deterministic source
behavior, not fixture readiness, network, auth or scheduling flakiness.

- Run 1: [eabbb5ec](https://app.buildbuddy.io/invocation/eabbb5ec-3b80-426a-b7d7-c6718232e796).
- Run 2: [e8f5612e](https://app.buildbuddy.io/invocation/e8f5612e-879a-4524-b15b-53be0cb3dec8).
