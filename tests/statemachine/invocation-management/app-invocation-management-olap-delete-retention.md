# OLAP projections retain deleted invocations

Title: DeleteInvocation removes the primary row but leaves OLAP build counts
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: A user-requested deletion leaves invocation-associated metadata in configured analytics projections. Counts/history and filter suggestions remain stale. Requires non-default OLAP configuration; no cross-org disclosure is demonstrated.
Status: confirmed
Repro test target: //tests/statemachine/invocation-management:divergences_test (TestOLAPDeleteConverges)
BuildBuddy invocations: https://app.buildbuddy.io/invocation/5d0e41e1-59e3-4497-8385-289878875af6 and https://app.buildbuddy.io/invocation/b8575cae-6ecf-4e36-a0a4-c89ff73d0f83

## Expected behavior

Once the initial BES invocation is observable in analytics and DeleteInvocation succeeds, GetTrend should eventually remove it from totalNumBuilds, agreeing with the primary-store reads. The SQL-only normal suite observes immediate removal.

## Actual behavior

Initial OLAP ingestion converged within 959.9 ms and 925.8 ms respectively. In both independent runs, GetTrend gRPC totalNumBuilds remained 1 throughout five seconds of bounded polling after successful deletion; the final assertion failed. The second run additionally observed retained HTTP GetTrend/GetInvocationStat build counts, GetTargetHistory invocation ID and GetInvocationFilterSuggestions user/host/branch values. Only trend retention is independently asserted and reproduced twice; the other retained projections are supporting second-run observations.

This appears permanent: DeleteInvocation calls the primary InvocationDB deletion path; no corresponding ClickHouse deletion/flush is present. The finite test demonstrates no convergence within five seconds, not an infinite-duration measurement.

## Logs excerpt

```text
OLAP initial convergence within 925.817256ms
OLAP deleted GetInvocationStat HTTP=map[invocationStat:[map[name:fixture-user totalNumBuilds:1 ...]]]
deleted OLAP trend elapsed=10.419343ms total=1
deleted OLAP trend elapsed=4.711601099s total=1
Should be zero, but was 1: OLAP retains deleted invocation
```

## Notes

The test starts a fixture-local ClickHouse 25.3 sidecar:
`mirror.gcr.io/clickhouse/clickhouse-server@sha256:b627d7a9bc0e0c1bac26cdbe9d2fc6316faa29c5d8a174f28f5abd57d0fa6ba2`.
It uses `CLICKHOUSE_SKIP_USER_SETUP=1` only on the private Docker network and
`olap_database.data_source: clickhouse://default:@clickhouse:9000/default`.
Target flags: `app.enable_target_tracking=true`,
`app.enable_write_test_target_statuses_to_olap_db=true`,
`app.enable_read_target_statuses_from_olap_db=true`.
Default app OLAP invocation-stat writes and reads remain enabled.
Server-admin configuration is used by the reusable restart setup; deletion itself
uses the ordinary fixture key. Initial successful ingestion and valid pre-delete
reads rule out disabled tracking or incomplete ClickHouse readiness. No shared
service or host credentials are used. The strict repro is isolated behind `bug`.
