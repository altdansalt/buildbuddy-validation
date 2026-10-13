# Docker input-tree conformance

All actions use fixture-local enterprise-v2.314.0 app/executor containers and
`workload-isolation-type=docker`. The area-owned daemon and aligned workspace
bind mounts follow the isolation-docker fixture, without touching shared Docker
containers. The action image is Alpine 3.21 from `mirror.gcr.io/library/alpine`,
pinned to OCI index digest
`sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507`.
No Docker Hub image pulls or shared component endpoints are used.

Ported coverage from input-tree (PRs #2/#29) includes symlinks, empty directories,
Unicode/newline/metacharacter/255-byte names, executable bits, cold/warm duplicate
content, 128-level trees, 4,096 siblings, concurrent actions with immutable input
replacement, and nonempty-directory metadata controls. The malformed-name
collision checks rejection, without assuming a mandatory INVALID_ARGUMENT code:
the original status-code finding was rejected by independent review.

The generated-tree property checks every entry against a Go reference model:
file content/type/executable bit, directory type, symlink type/target, and exact
NUL-delimited entry count. Six deterministic seeds run twice each by default.
Trees contain hundreds of repeated file digests with mixed executable bits,
Unicode and awkward names, nested directories, empty leaves, dangling and
self-referential symlinks. REAPI does not encode hardlinks; repeated content
exercises the executor's internal hardlink deduplication without requiring it.

Replay or extend using `--test_arg=-inputs-docker.seed=314159` and
`--test_arg=-inputs-docker.iterations=6`. Every generated subtest logs its seed.
Default runtime is bounded; long exploratory runs can increase iterations.
No random-property failure has yet been reduced to a new fixed reproducer.

Run passing coverage:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/inputs-docker/... --config=nobug
```

Run the two known bare-isolation defect probes under Docker:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/inputs-docker:input_tree_limits_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

References inspected:

- [REAPI canonical Directory and executable-bit contracts](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L978).
- [Released input materializer](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go), including digest/executable fetch keys and hardlink copying.
- [Released Docker mounts](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/docker/docker.go#L401).

Optional metadata application is server-defined; the metadata probe checks
successful retrieval/materialization, not mandatory Unix mode or mtime support.
Limits: no runner recycling, nested virtualization, arbitrary mutation of
hardlinked input inodes, or exhaustive resource-limit coverage.
