# Executor input-tree coverage

These tests run actions on fixture-local enterprise-v2.314.0 app/executor
containers with the harness's bare Linux runner. No shared service is an action
or CAS endpoint.

The regression target covers relative, directory, absolute (when advertised),
dangling, self-referential and outside-root symlinks; empty directories; Unicode,
newline, shell metacharacter and 255-byte names; shared file digests with different
executable bits on cold and warm file-cache paths; 4,096 sibling files and a
128-level tree. Every valid Directory is sorted by UTF-8 name.

The collision target supplies one file and one directory with the same name and
expects INVALID_ARGUMENT before command execution.

References inspected:

- [REAPI Directory and SymlinkNode contracts](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)
- [Released materializer](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go): DownloadTree / inputTreeWrangler, digest+executable fetch keys.
- [Released advertised capabilities](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go): absolute symlinks allowed.

Run regression coverage with `bb remote test //tests/executor/input-tree/... --config=nobug`.
Run the malformed-tree probe explicitly with
`bb remote test //tests/executor/input-tree:file_directory_collision_test --test_tag_filters=bug --nocache_test_results`.

Limits: this is bounded stress coverage, not an exhaustive maximum-size test.
It checks `is_executable`; optional Unix mode node properties are not assumed
supported. Docker/Firecracker isolation, runner recycling, and symlink traversal during
output upload are outside this coverage.

Round 2 adds eight simultaneous actions in each of two cold/warm waves, sharing
256-file subtrees and checking executable bits. Inputs share hardlinked inodes:
commands must unlink a path before replacing its content, rather than mutate an
input inode in place. Optional file/directory properties are accepted without
assuming their modes or timestamps are honored; the passing case uses a nonempty
directory.

`//tests/executor/input-tree:input_tree_limits_test` is bug-tagged and contains
two independent failure assertions: materializing 65,536 identical files across
256 directories exceeds the filesystem hardlink limit, and an empty Directory
with NodeProperties is reported missing despite successful CAS upload/readback.
Run it explicitly with `--test_tag_filters=bug --nocache_test_results`. Findings
record both reproductions and remain unverified pending review.
