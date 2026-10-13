# Seeded Bazel workspace agreement

Runs Bazel 7.4.1 (downloaded from releases.bazel.build with a fixed SHA256)
inside the hosted test runner, against the harness's pinned v2.314.0 app,
executor and cache proxy. Nested Bazel does not read rc files or inherit the
outer dispatch key. All cache/execution endpoints are fixture-local.

The seed chooses source contents, graph size, chain/diamond edges and edits.
Each generated workspace includes genrules, a Starlark tree artifact containing
a relative symlink, and native sh_binary/sh_test targets using data runfiles.
Five independent Bazel output bases retain incremental state: local, app remote
cache, a second client replaying that cache, direct remote execution, and app
execution with cache traffic through the proxy. Distinct
instance names prevent execution modes from warming one another; the cache
replay client deliberately shares the cache instance and must report remote
cache hits for every graph action. Execution modes disable
remote action-cache reads to exercise the released executor.

The released proxy does not register the Execution service; scheduling always
addresses the local app, while proxy-mode Bazel CAS/AC traffic addresses the proxy.

After the initial build, each edit, and a final no-change rebuild, compare every
genrule output, tree file,
symlink referent and link target. Check the execution log's Genrule/WorkspaceTree
labels against the exact source-dependent transitive closure. Shell tests also
check the binary's tree artifact data runfiles. Bazel's own test caching can
avoid rerunning unchanged shell tests; those are excluded from the rebuild
oracle. Execution logs are saved as undeclared outputs.

Replay or expand (iterations bounded to 12):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-workspaces/... --config=nobug --nocache_test_results --test_arg=-workspaces.seed=314 --test_arg=-workspaces.iterations=2
```

Each nested invocation has a 75-second deadline. Default coverage is one graph
with two edits and a no-change rebuild; more seeds are opt-in to keep fixture
runtime bounded. A failed
property prints the seed, edit and mode; no product finding is inferred from a
Bazel setup error. Shrinking and fixed bug repros are required if a property
failure is discovered.

Contracts inspected: released executor delegates platform interpretation to
[`server/util/platform`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go)
(`workload-isolation-type=none`, `OSFamily`, `Arch`), and the
[REAPI](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
Action input-root and ActionResult output-directory/symlink contracts. Content
and symlink targets must survive remote execution and cache round trips.
