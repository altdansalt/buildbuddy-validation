# Execution history ignores target-label filter on primary database

Title: GetExecution ignores ExecutionLookup.target_label on the primary database path
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: An authenticated reader receives unrelated executions from the same readable invocation when requesting one target. This breaks API filtering and can misattribute results or unnecessarily return a large invocation's history. No cross-organization access or execution corruption was observed.
Status: confirmed
Repro test target: //tests/app/execution-history:target_label_filter_bug
BuildBuddy invocation: https://app.buildbuddy.io/invocation/7c55d0b4-7282-4e0d-b061-6f3b5ca93dfe

## Expected behavior

The released [ExecutionLookup.target_label contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/execution_stats.proto) says: “Only executions for this target will be returned (exact match).” Execute two distinct actions with RequestMetadata.target_id set to `//history:case_0` and `//history:case_1`. A GetExecution lookup for `//history:case_0` should return exactly the first execution.

## Actual behavior

The lookup returns both executions. Reproduced on two independent fixture-local app/executor pairs:

1. https://app.buildbuddy.io/invocation/e68f7506-6df8-425f-ab11-8f66e3bbebe8 — two-action reproducer returned two rows for a one-target lookup.
2. https://app.buildbuddy.io/invocation/7c55d0b4-7282-4e0d-b061-6f3b5ca93dfe — same failure with concise filter diagnostics.

Both runs first validated terminal Execute responses, history digest/status/exit code/worker/timing consistency, and ID/digest filters. Distinct target IDs are attached through the standard binary RequestMetadata header. No shared service, external database, source-built component, or host dispatch credential participates. The fixture uses its default supported SQLite history path; no ClickHouse is configured.

Source inspection rules out a filter spelling/configuration mistake: [getInvocationExecutionsFromPrimaryDB](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/execution_service/execution_service.go) reads invocation ID, action digest hash and execution ID but never reads target_label; the OLAP path does read it. The primary execution table also does not persist the target label in this path. Independent verification below confirms the defect.

## Logs excerpt

```text
history.seed=314159 history.iterations=2
filter=map[executionId:history/uploads/887982cc-d764-47f3-9f5c-9ba2197c6da3/blobs/ed43ac38dd3921a9dbde5a413b20d46f9db141bc81aa1f74fdb3665bb48ae81c/143 invocationId:ab8d9d6c-c942-4015-9fbf-b223c8403efa] expected_count=1 actual_count=1
filter=map[actionDigestHash:ed43ac38dd3921a9dbde5a413b20d46f9db141bc81aa1f74fdb3665bb48ae81c invocationId:ab8d9d6c-c942-4015-9fbf-b223c8403efa] expected_count=1 actual_count=1
filter=map[invocationId:ab8d9d6c-c942-4015-9fbf-b223c8403efa targetLabel://history:case_0] expected_count=1 actual_count=2
expected: 1
actual  : 2
```

## Notes

Run from this repo with its public runner-fetch remote configured:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/execution-history:target_label_filter_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

The minimal case is two sequential bare-runner shell actions, one exiting 0 and one exiting 74, under a BES invocation. The seed and iteration count are fixed by target defaults. Executor image: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8. All image overrides are unset. Full fixture configs, inspect metadata and container logs are preserved in undeclared test outputs.

Passing randomized regression coverage: https://app.buildbuddy.io/invocation/7d82b2f9-80e6-432f-932d-2ba731597417. It includes repeated timeout action digests and checks digest-filter results as a set, rather than assuming one execution per action digest. Direct app action-cache hits intentionally schedule no execution and are excluded from the history oracle based on the released execute implementation. No harness/dependency changes are needed. A caller can filter locally, but target labels are not populated by this primary-DB history path, so another source of target-to-action attribution is needed.

## Verification

Verdict: **confirmed** (independent review, 2026-10-13). Reviewed the finding and
bug-tagged test from PR #113, commit `db696514e1f78176da9b13aca3220370f1d2958f` as checked
out for both runs. No test or harness changes were made. Severity remains **low**:
the demonstrated defect returns unrelated actions from the same authorized
invocation; neither an authorization bypass nor incorrect action execution was
observed.

### Attempts to disprove

- **Contract and metadata:** The pinned
  [ExecutionLookup contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/execution_stats.proto#L282-L297)
  explicitly promises an exact target-label match without an OLAP prerequisite.
  This is a BuildBuddy API contract, not a REAPI history requirement. The
  [REAPI RequestMetadata definition used by the test](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L2610-L2645)
  defines `target_id` and permits raw serialized protobuf bytes through gRPC's
  binary metadata API; the test's header is correct and needs no manual base64
  encoding. BuildBuddy's pinned
  [execution server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go#L310-L320)
  maps `RequestMetadata.target_id` to `TargetLabel`, ruling out an incorrect
  assumption about which metadata field supplies the label.
- **Configuration and intended behavior:** The fixture starts only local app,
  executor and Redis containers, uses supported SQLite storage and provisions
  local authentication. Its JSON RPC helper preserves `targetLabel` in the
  request body. The pinned
  [OLAP documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-olap-databases.md)
  says ClickHouse is optional, describes primary-DB reads/writes as enabled by
  default, and does not restrict this filter to ClickHouse. The
  [primary query](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/execution_service/execution_service.go#L80-L133)
  applies invocation, digest and execution-ID predicates but never reads the
  target label. The
  [primary execution schema](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/tables/tables.go#L520-L575)
  has no target-label field. The OLAP path does apply the filter. This explains
  the observed primary-path failure; it is not an unsupported fixture setup.
- **Flakiness and oracle:** Both independent, uncached runs completed two
  distinct actions with distinct target IDs and digests. Before the failing
  assertion, the test checked both terminal Execute responses and both history
  rows' digest, status, exit code, worker and ordered timestamps. Execution-ID
  and digest filters each returned exactly one row; the target filter returned
  both rows. The test waits for the expected history count, so this is not a
  race waiting for a missing row. The failure occurs during a still-open BES
  invocation, after both actions have completed; the later post-BES check is
  not reached and is not claimed as independently verified.

### Independent reproductions

Both runs used seed `314159`, two iterations, and the exact app/executor digests
listed above (logged by each fixture); no image overrides were supplied.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/execution-history:target_label_filter_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

1. [Bazel invocation 23e20e9c-3153-45b9-9775-66af71ba7bf3](https://app.buildbuddy.io/invocation/23e20e9c-3153-45b9-9775-66af71ba7bf3)
   ([runner invocation](https://app.buildbuddy.io/invocation/ac949bfe-c2db-46fd-8f30-6fb20cb06b7c)):
   `targetLabel://history:case_0 expected_count=1 actual_count=2`, test failed
   at the target-filter count assertion after 35.41 seconds.
2. [Bazel invocation 0683f2e7-2852-4e7b-901a-0bec2775172c](https://app.buildbuddy.io/invocation/0683f2e7-2852-4e7b-901a-0bec2775172c)
   ([runner invocation](https://app.buildbuddy.io/invocation/8e459bc1-aa4b-404c-8f45-2ca1a5efeacf)):
   identical target-filter failure after 36.86 seconds, with fresh invocation
   and execution IDs.

No shared harness or dependency changes are needed for this verdict. The
confirmed scope is the default primary-database path on enterprise-v2.314.0;
OLAP-only filtering and later releases were not tested.
