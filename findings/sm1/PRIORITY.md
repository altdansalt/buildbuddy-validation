# sm1 state-machine / cross-surface consistency: prioritized defects

Run: sm1 conformance against released enterprise `v2.314.0` app and cache proxy.
Kept: defects where at least one of three judges scored interest >= 6 and confidence >= 7,
deduplicated by root cause, minus anything a judge called intended behavior or a harness
artifact. Judge scores are listed as interest/confidence for each judge. This file lives in
a subdirectory so `tools/findings_index` (which globs `findings/*.md`) is unaffected;
`INDEX.md` stays generated and will list the finding files once they merge.

| # | Id | Finding | Defect | Judges | Permanence / config |
| --- | --- | --- | --- | --- | --- |
| 1 | ip-rules | `app-statemachine-ip-rules-file-download-bypass.md` (PR #153) | `/file/download` and `/file/view` keep serving private logs, JSON and CAS blobs after an IP rule denies the client, while gRPC and `/rpc` deny. | 8/9, 8/6, 8/6 | Permanent: `AuthorizeHTTPRequest` only enforces `/rpc/` and `/api/` paths (`ip_rules_enforcer.go:562-567`). IP rules enabled. |
| 2 | org-api-keys | [app-org-api-keys-modern-visibility.md](../app-org-api-keys-modern-visibility.md) | `CreateApiKey`/`UpdateApiKey` ignore the repeated `visibility` field (only the deprecated bool is read); `ExpiryUsec` is also missing from create/list projections. | 4/9, 7/7, 8/8 | Permanent, default config (`buildbuddy_server.go:1173,1225`). |
| 3 | cas-chunk-state | `app-cas-splice-find-missing-threshold.md` (PR #154) | A spliced 2-4 MiB blob is readable but `FindMissingBlobs` reports it missing. | 6/8, 7/7, 6/7 | Permanent, default config (`content_addressable_storage_server.go:148-178` vs `chunking.go:130`). |
| 4 | execution-state | [app-execution-state-cached-wait.md](../app-execution-state-cached-wait.md) | `WaitExecution` on a cache-hit `Execute` operation never answers (client deadline hit). | 5/8, 7/6, 7/7 | At least 10s, source says permanent; default config (`execution_server.go:1182-1192`, `:1324-1403`). |
| 5 | public-cache-delete | `cacheproxy-statemachine-warmed-proxy-serves-deleted-blob.md` (PR #155) | A warmed proxy serves a deleted CAS blob while its own `FindMissingBlobs` says it is missing. | 5/9, 6/7, 6/8 | At least 60s, until eviction; needs `enable_cache_delete_api`; read-through design may be partly intended (`byte_stream_server_proxy.go:265`, `content_addressable_storage_server_proxy.go:222-236,399`). |

Duplicates folded into #5 by the judges: convergence-deletes, convergence-cas, target-projections,
action-cache-state, distributed-cache-state, cas-state.

## Considered and not recorded

- convergence-ac (compressed `GetFileRange` HTTP 500, interest 5/5/5): below the interest bar, though
  judges rated it well proven (`api_server.go:630-640`, `pebble_cache.go:3812-3816`; invocation fdf39dbb).
  Related to existing `*-compressed-read-limit` findings.
- invocation-management (OLAP trends keep deleted invocations; deleted download 500; owner lookup Unknown):
  OLAP part needs ClickHouse and was judged arguably intended; the 500 duplicates
  `app-http-endpoints-missing-invocation-500.md` / bes-lifecycle. Notes remain on `cand/sm1/invocation-management`.
- users-membership (removed-member key accepted ~59s by proxy; anonymous download): matches the documented 60s
  `jwt_cache_ttl` (`remoteauth.go:46`) and anonymous-usage config; judged intended.
- bes-lifecycle, github-link-state, sso-config and the rest scored interest <= 6 with low confidence or were
  duplicates; their branches `cand/sm1/<id>` hold the evidence.
