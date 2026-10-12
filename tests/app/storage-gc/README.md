# Local disk cache storage coverage

Run `bb remote test //tests/app/storage-gc/... --config=nobug`.

The tests select `cache.disk.root_directory=/data/disk-cache`, explicitly disable
Pebble, and cover both legacy and v2 disk layouts. Each fixture owns its app,
Redis, network, and volume; lifecycle commands name only that fixture's app.

- Eviction: twelve 64 KiB blobs exceed a 256 KiB cache limit. After the janitor
  settles, at most four can remain. FindMissingBlobs, ByteStream.Read, and
  BatchReadBlobs must agree about each entry, with exact bytes or NOT_FOUND.
  Reuploading an evicted digest must restore readable data.
- Concurrent pressure: three writers/readers churn a small cache; reads may
  miss but must return exact bytes when successful, never partial data or INTERNAL.
- Persistence: committed data survives SIGKILL and a graceful restart; an open,
  unfinished ByteStream upload is absent before and after SIGKILL. Completing
  a fresh upload of the same digest succeeds and survives the next restart.

Contract references:
- [released disk cache](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/backends/disk_cache/disk_cache.go):
  filesystem-block size accounting, 100 ms janitor, 90% target, in-place LRU updates.
- [released disk writer](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/disk/disk.go):
  commit publishes the temporary file; default sync-on-commit is enabled.
- [REAPI CAS](https://github.com/bazelbuild/remote-apis/blob/master/build/bazel/remote/execution/v2/remote_execution.proto):
  FindMissingBlobs and per-entry BatchReadBlobs status semantics.
- [REAPI ByteStream conventions](https://github.com/bazelbuild/remote-apis/blob/master/build/bazel/remote/execution/v2/remote_execution.proto):
  unfinished writes are not committed blobs.

The crash test allows one second for the partial stream to reach the server;
there is no acknowledgement of uncommitted bytes in ByteStream.Write, so it
checks visibility rather than asserting a particular temporary-file layout.
It does not simulate host power loss or verify the 15-minute stale-file janitor.
The fixture has no lifecycle API; the area helper locates the daemon by inspecting
only its unique app name, then kills/restarts that container. A harness-provided
Docker command or restart method would remove this workaround.
