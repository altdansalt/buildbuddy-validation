# Capabilities conformance

These tests use released v2.314.0 containers and fixture-local RPCs only.

`go_default_test` checks app and proxy cache advertisements against SHA1,
SHA256, SHA384, SHA512 and BLAKE3 CAS round trips; advertised identity/ZSTD
batch uploads and ByteStream uploads/downloads; 1 MiB batch transfers when
`max_batch_total_size_bytes` is zero; and advertised action-cache updates.
It also checks the app's Execution service and configuration with remote
execution and ZSTD transcoding disabled.

`proxy_execution_bug_test` runs only the proxy execution-advertisement
reproducer. A missing action probes service availability without scheduling
work or requiring an executor. The upstream app is a control under the same
configuration: neither endpoint should return UNIMPLEMENTED if it advertises
`exec_enabled=true`.

`batch_limit_bug_test` checks a 4,000,001-byte blob against the advertised
zero batch limit, with successful BatchUpdate and ByteStream controls.

Run on hosted runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities/... --config=nobug
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:proxy_execution_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

Contract/source references:

- [Pinned REAPI capabilities contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).
- [App capabilities implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go).
- [Proxy capabilities forwarding](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/capabilities_server_proxy/capabilities_server_proxy.go).
- [Proxy service registration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/cmd/cache_proxy/cache_proxy.go).

Round 2 adds real shell actions for every execution digest function (legacy
fallback included), default and advertised boundary priorities, and rejection of
priorities immediately outside the advertised range. Output file digests must
use the requested function. A small single-chunk BLAKE3 implementation is checked
against independent empty, `abc`, and multi-block vectors; no dependency changes
are required.

The split/splice property uses seeded random data up to 2 MiB. It uploads separate
chunks, splices a previously absent blob, checks ByteStream reconstruction, and
checks the resulting split mapping. Ordinary uploads may legitimately have no
mapping and return NOT_FOUND. Replay using
`--test_arg=-capabilities_seed=31402`. Unsupported compressor choices must return
INVALID_ARGUMENT or UNIMPLEMENTED. Identity and UNKNOWN digest inference are
protocol defaults, not negative cases.

`unknown_digest_bug_test` isolates acceptance of an unknown digest function by
FindMissingBlobs. Run explicitly with `--test_tag_filters=bug
--nocache_test_results`.

This is a bounded capabilities matrix, not exhaustive coverage of every REAPI
field: scheduling order/fairness, arbitrary node properties, symlink policy,
permission-dependent Bazel metadata, and nondefault chunking configuration remain
outside this round. Streaming chunk mapping APIs require API v2.13; this release
advertises v2.11, so this round exercises its legacy split/splice RPCs.
