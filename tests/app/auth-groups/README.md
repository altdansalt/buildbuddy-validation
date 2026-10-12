# App auth and groups

Runs the digest-pinned enterprise v2.314.0 app and executor in disposable local
fixtures. Self-auth has one user, who owns two groups; these tests check **API key
group boundaries**, not separation between independently authenticated users.

- `go_default_test`: same-digest action cache isolation, read-only and CAS-only
  keys, advertised AC update permissions, CAS reads across groups, read-only
  BatchUpdate/ByteStream writes that acknowledge but discard data, invalid API
  key rejection, and authorized executor registration in the key's group.
- `wait_execution_cross_group_bug`: a completed operation owned by group A must
  not be returned by WaitExecution to a group B key. Runs a successful action
  and verifies the owning group's WaitExecution first.

The defaults intentionally deduplicate CAS across groups; CAS sharing is not
an isolation finding. Restricted keys intentionally acknowledge unauthorized
cache writes for Bazel compatibility; the regression checks storage effects,
not a PermissionDenied response. These choices follow the released source:

- [Capability definitions](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/capability.proto)
- [AC authorization and write suppression](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go)
- [CAS write suppression](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go)
- [ByteStream write suppression](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go)
- [Executor authorization and group assignment](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/scheduling/scheduler_server/scheduler_server.go)
- [WaitExecution implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go)
- [REAPI Execute/WaitExecution and ActionCache contracts](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)

Executor assignment uses a second executor-only fixture joined to the app's
private network and passes a disposable REGISTER_EXECUTOR key, with
`remote_execution.require_executor_authorization: true` on the app. No shared
endpoint or host dispatch credential is used.
