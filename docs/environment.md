# Released-component test environment

Investigation date: 2026-10-12. This document distinguishes source/configuration
inspection from runner observations; runner results and links are recorded below
as validation completes. No component requests go to BuildBuddy Cloud.

## Release and registries

Latest GitHub release: [v2.314.0](https://github.com/buildbuddy-io/buildbuddy/releases/tag/v2.314.0).
The canonical [Helm charts](https://github.com/buildbuddy-io/buildbuddy-helm/tree/master/charts)
now use `buildbuddy.bbcr.io/public/` with these repository basenames and the tag
`enterprise-v2.314.0`. The corresponding public GCR releases are available, and
are the images used by this repository (linux/amd64 manifests):

| Component | Image | Manifest digest |
| --- | --- | --- |
| app | `gcr.io/flame-public/buildbuddy-app-enterprise:enterprise-v2.314.0` | `sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` |
| executor | `gcr.io/flame-public/buildbuddy-executor-enterprise:enterprise-v2.314.0` | `sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8` |
| cache proxy | `gcr.io/flame-public/buildbuddy-proxy-enterprise:enterprise-v2.314.0` | `sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906` |

Digests were obtained from the GCR registry manifest API, not inferred from tags.
Defaults use `repository@sha256:...`; overrides deliberately permit other releases.

## Configuration investigation

The enterprise app supports `database.data_source: sqlite3:///data/app.db` and
local `storage.disk.root_directory` and `cache.pebble.root_directory`. Remote
execution additionally requires Redis for scheduler state and pubsub: SQLite is
not a replacement for Redis. The fixture will run Redis locally in its private
Docker network; no external database or service is used.

The app exposes HTTP 8080, gRPC 1985 and monitoring 9090 by default. The executor
needs `executor.app_target: grpc://app:1985`, local root/file-cache directories,
and an enabled isolation provider. Baseline actions use the bare runner inside
the executor container (`enable_bare_runner: true`, `default_isolation_type: none`),
so nested virtualization is not required. The cache proxy needs
`cache_proxy.remote_cache: grpc://app:1985` and a local Pebble cache; remote auth
can delegate to that same app. Each component has `/healthz` and `/readyz` HTTP
handlers (executor on HTTP 8080, separate from monitoring 9090).

No licensing flag/check was found in enterprise server source. The pinned release image started successfully without a license key, and core
cache proxy forwarding, app caching, execution, and local organization/API key
provisioning succeeded. No runtime license gate was
found in source; configuration/auth/group capability checks still apply. This
is not an exhaustive validation of every enterprise integration.

## Authentication investigation

`auth.enable_self_auth: true` enables a built-in OIDC provider with one fixed
identity (`buildbuddy@example.com`); no external OIDC is needed. It is explicitly
intended for trusted development environments and grants powerful access.
`app.no_default_user_group`, `app.create_group_per_user`, and the app's group/API
key APIs support organization setup after login. Multiple distinct identities
require a local mock OIDC provider configured in `auth.oauth_providers` (issuer
URL, client ID, client secret); the built-in provider itself is single-user.
The default fixture enables self-auth alongside anonymous usage, logs in as
the fixed user, and provisions a local organization and API key. The proxy
example also creates a second organization and key.

## Runner observations

Reproduce explicitly with
`bb remote --runner_exec_properties=init-dockerd=true test //tools:runner_probe --test_output=all`.

## Verified Docker runner properties

[Probe invocation](https://app.buildbuddy.io/invocation/dbd30132-858f-42e7-a627-1ed4ed3fb31f):
Ubuntu 20.04.4 guest, Linux 5.15, amd64, Docker 27.5.1, overlay2, cgroup v2,
6 visible CPUs and about 7.1 GiB RAM. The runner user is UID 1000, in the Docker
and sudo groups, with passwordless sudo. `/sys/fs/cgroup` is mounted read/write;
controllers include cpuset, cpu, io, memory, hugetlb and pids. A privileged Alpine
container successfully mounted tmpfs and created/removed a cgroup. `/dev/kvm`
was absent. Privilege does not create nested KVM support; Firecracker-enabled
executor tests need a different runner with KVM exposed, and are not supported
by this baseline. There was no need for privileged mode for the app/proxy or
bare-runner executor.

Docker is installed but **not started by default**. On the outer hosted runner,
`bb remote --runner_exec_properties=init-dockerd=true test ...` initializes it.
The fixture also starts an owned private daemon via sudo when `docker info`
fails, so a plain `bb remote test //...` is supported. Owned daemons use separate
sockets, state directories and IPAM pools; an interprocess file lock serializes
iptables chain initialization. Do not start two daemons simultaneously without
that lock (the initial probe hit an iptables DOCKER-chain creation race). These
are harness/environment constraints, not released-component bugs.

Use `local` and `requires-network` tags on each component test, and
`--spawn_strategy=local --strategy=TestRunner=local` (set in `.bazelrc`). This runs
the test on the hosted VM outside Bazel sandboxing, with the VM's Docker socket
and network available. `init-dockerd` is an **outer runner** property; setting a
Bazel target exec property will not initialize a daemon for a locally run test.
Keep fixture concurrency bounded (`--local_test_jobs=2` is the repository default).

The exe.dev GitHub integration URL is not reachable by hosted runners. Initial
checkout failed in [this invocation](https://app.buildbuddy.io/invocation/b9c9978a-409c-4851-b123-68e362e7bdba).
Use a public GitHub fetch remote plus `GIT_CONFIG_GLOBAL=/dev/null` for `bb remote`
to bypass the worker's global URL rewrite; keep internal origin for pushes.
See root AGENTS.md for exact setup commands. The dispatch key belongs to this
outer infrastructure only, never to test-local app/executor/proxy requests.

## Minimal local configuration

These are the fixture defaults, with paths on per-container anonymous Docker
volumes backed by the runner's local disk. Each test uses its own bridge network
and random published loopback ports. Internal peer addresses are Docker names.
The self-auth issuer `http://app:8080` resolves inside that network; the fixture's
HTTP client maps it to the published port without changing the issuer origin.

App (`8080` HTTP, `1985` public gRPC, `1987` internal gRPC, `9090` monitoring):

```yaml
server_type: app
disable_telemetry: true
telemetry_port: -1
app:
  build_buddy_url: http://app:8080
  default_redis_target: redis:6379
  no_default_user_group: true
  create_group_per_user: true
database:
  data_source: sqlite3:///data/app.db
storage:
  disk:
    root_directory: /data/storage
cache:
  max_size_bytes: 1073741824
  pebble:
    root_directory: /data/cache
auth:
  enable_anonymous_usage: true
  enable_self_auth: true
  jwt_key: fixture-only-hmac-key
  jwt_es256_private_key: <PEM EC P-256 private key generated by the fixture>
remote_execution:
  enable_remote_exec: true
  require_executor_authorization: false
ssl:
  enable_ssl: false
```

Redis runs as `redis:7.4.2-alpine@sha256:02419de7eddf55aa5bcf49efb74e88fa8d931b4d77c07eff8a6b2144472b6952`
on internal port 6379 with `redis-server --save '' --appendonly no`. It is a
local scheduling/pubsub sidecar, not an external SQL database.

Executor (health HTTP `8080`, metrics HTTP `9090`; no public gRPC client API):

```yaml
server_type: executor
monitoring_port: 9090
executor:
  app_target: grpc://app:1985
  root_directory: /data/executor
  local_cache_directory: /data/filecache
  local_cache_size_bytes: 1073741824
  enable_bare_runner: true
  default_isolation_type: none
```

The executor's cache target defaults to its app target. Actions specify
`workload-isolation-type=none`, `OSFamily=linux`, `Arch=amd64`; commands run inside
the released executor container. Tests of Docker/OCI isolation must explicitly
configure the corresponding provider and mount/socket/cgroup access.

Cache proxy (`8080` HTTP, `1985` public gRPC, `1987` internal gRPC, `9090` monitoring):

```yaml
server_type: cacheproxy
cache_proxy:
  remote_cache: grpc://app:1985
  proxy_type: customer
cache:
  max_size_bytes: 1073741824
  pebble:
    root_directory: /data/cache
auth:
  remote:
    target: grpc://app:1985
    use_es256_jwts: true
  reparse_jwts: false
ssl:
  enable_ssl: false
```

The cache proxy's remote authenticator requires a local app API key on client
requests; `auth.enable_anonymous_usage` is not a proxy substitute for that.
No Kubernetes/distributed cache is needed for this single-proxy fixture. The
Helm chart enables distributed discovery for a cluster; that is deliberately
omitted here. Proxy registration in the UI is optional and requires
`cache_proxy.app_target` and `cache_proxy.api_key`; it does not affect data serving.

Readiness requests must include `?server-type=<configured server_type>` (or the
`server-type` HTTP header). A bare `/readyz` returns 500 even for a healthy app.
The fixture checks readiness, not just whether the process or TCP port exists.

Executor health is served on its ordinary HTTP port **8080**, not on metrics
port 9090. A probe on the metrics port returned 404 even while the executor was
registered and running. Both ports can be overridden in config; the fixture
publishes the HTTP health port and leaves metrics internal.

Default Pebble CAS keys deduplicate across groups in the same partition (v5
keys omit group ID for CAS; AC keys include it). A cross-group FindMissingBlobs
check found a previously uploaded CAS blob. Source inspection explained this
behavior, so it is not recorded as a confirmed bug. Do not assume default CAS
storage provides an org-isolation oracle; configure explicit partitions or the
relevant key version for isolation-focused tests. Group/API key provisioning
itself is supported without external OIDC; distinct user identities require a
local mock OIDC provider.

## End-to-end validation

[Green suite](https://app.buildbuddy.io/invocation/af3045c7-c408-4510-a99d-c5e1998d4286)
ran both examples with `bb remote test //...`: proxy ByteStream/CAS upload and
readback from both proxy and app, and an app-scheduled shell action on the
released executor with fetched output file/stdout. Both used an isolated app,
SQLite/local disk and local Redis; no license key, external DB or external OIDC.
The proxy example successfully provisioned a second organization with a distinct
API key using the built-in self-auth identity. Core features in these examples
were not license-gated. Other features remain controlled by configuration,
permissions/capabilities (e.g. cache writes, executor registration and org admin),
and integration prerequisites (e.g. GitHub/OIDC/SAML or ClickHouse). The scaffold
has not exhaustively tested every such feature or implemented a mock OIDC issuer.

There are no confirmed product findings from the scaffold investigation.
