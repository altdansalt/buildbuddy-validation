# Go fixture for released images

`//harness` aliases `//harness:go_default_library`. Component tests run on the
hosted VM using `bb remote`, with `local` and `requires-network` Bazel tags.
The default fixture creates a Docker network, starts local Redis when starting
an app (the pinned Redis digest is fetched from `mirror.gcr.io` to avoid
Docker Hub anonymous pull limits), and starts the requested released containers. SQLite, blob storage,
Pebble and executor workspace/file cache live under each container's `/data`.
That directory is a per-container anonymous Docker volume on local disk;
persistence lasts for the test.
Cleanup removes all containers, volumes and the network, including on failure.

```go
f := harness.Start(t, harness.Options{
    Components: []harness.Component{harness.App, harness.CacheProxy},
    Config: map[harness.Component]harness.Config{
        harness.App: {"cache": harness.Config{"max_size_bytes": int64(64 << 20)}},
    },
})
ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
defer cancel()
c := f.Clients(harness.CacheProxy)
d, err := c.Upload(ctx, "instance", []byte("hello"))
// Assert err, then c.Read(ctx, "instance", d).
```

Options select an exact component subset. To start only an executor or proxy,
provide `AppTarget` pointing to an app in another fixture and connect the Docker
networks through `DockerArgs`; alternatively override all peer targets in Config.
`Args` appends component command-line flags. `DockerArgs` supports devices,
privileged mode and additional mounts when an area requires them. Default tests
need no privileged containers. There is no KVM on the validated hosted runner.

`Endpoint(component)` exposes the published loopback HTTP/gRPC addresses, the
configured ServerType for health probes, and
internal gRPC address. `Clients(component)` provides CAS (batch/find/tree),
ByteStream, AC, Execution, Capabilities, and BES clients, automatically using the
fixture's locally provisioned API key. `Dial(t, endpoint, key)` and
`WithAPIKey(ctx, key)` let tests select other groups or exercise raw auth behavior.
`Digest`, `MarshalDigest`, `Compress`, `Decompress`, `ResourceName`, and
`UploadResourceName` implement SHA256/protobuf/zstd and ByteStream naming helpers.
`Upload`, `UploadProto`, `Read`, and `Write` are convenience RPC helpers.

The app uses built-in single-user self-auth. `f.Web` is a cookie-authenticated
HTTP session using the real app's JSON protolet API. `CreateGroup` and
`CreateAPIKey` create additional local organizations/keys; `RPC` calls other UI
APIs. `GroupID` identifies the initial group. `SkipAuthBootstrap` disables automatic
login (useful for anonymous/auth-negative tests or mock OIDC). With alternate
OIDC config, use your own local cookie login client. No host credentials enter
containers. The default self-auth URL is `http://app:8080`: the WebSession
transport maps that origin to the published port, while the app resolves its own
Docker alias. Port overrides update peer targets and the self-auth URL automatically. A custom
`app.build_buddy_url` is also supported by the WebSession transport. Use
`--port=...` / `--grpc_port=...` (the equals form) for CLI overrides.

Image override precedence is Options.Images, `-harness.<component>_image`,
`BB_<COMPONENT>_IMAGE`, `BB_VERSION`, pinned digest. Full image references or short
release tags are accepted (`v2.314.0` expands to `enterprise-v2.314.0`). Examples:

```sh
bb remote test //tests/executor/smoke:go_default_test --test_arg=-harness.executor_image=enterprise-v2.314.0
bb remote test //... --test_env=BB_VERSION=enterprise-v2.314.0
```

For exe.dev checkout, prepend `GIT_CONFIG_GLOBAL=/dev/null` after configuring the
public fetch remote as described in root `AGENTS.md`. An optional runner exec
property `init-dockerd=true` starts Docker before Bazel. Plain `bb remote test`
works because the fixture starts an owned private daemon if Docker is stopped.
The fallback requires passwordless sudo (available on the tested hosted image),
saves its logs, stops it and removes its private data directory on cleanup.

Each fixture saves YAML configs, container stdout/stderr, full Docker inspect
metadata (including actual image IDs), and owned-daemon logs under its unique
subdirectory of `TEST_UNDECLARED_OUTPUTS_DIR`. Test-local keys in these outputs
are disposable. Use Bazel test artifacts on the BuildBuddy invocation to inspect
them. Images remain cached only when using a prestarted/shared Docker daemon.

## Lifecycle and scheduling readiness

Fixture lifecycle methods operate only on owned containers and preserve their
`/data` volumes. Methods report failures through the fixture's `testing.TB`:

```go
func (f *Fixture) Stop(c Component)
func (f *Fixture) Start(c Component) *Endpoint
func (f *Fixture) Restart(c Component) *Endpoint
func (f *Fixture) Kill(c Component, signal string)
func (f *Fixture) AddExecutor(opts Options) Component
func (f *Fixture) WaitExit(c Component, unsuccessful bool) int
func (f *Fixture) WaitExecutionReady()
func (f *Fixture) Logs(c Component) string
func (f *Fixture) Exec(c Component, args ...string) string
func (f *Fixture) CopyConfig(c Component, path string)
```

`Stop` and `Restart` allow 40 seconds for graceful drain. `Kill` sends a signal
without waiting; use `WaitExit` to await termination. `Start` and `Restart`
refresh published ports in the existing Endpoint object and wait for HTTP
readiness. Recreate gRPC clients afterwards. The app's WebSession follows the
refreshed HTTP endpoint while preserving its cookies and local group/key.
Methods are intended for sequential test use, not concurrent fixture mutation.

`AddExecutor` returns an independent component ID usable with all lifecycle
methods. Its nil Images, Config, Args and DockerArgs maps inherit the fixture's
options; supplied maps replace those maps for the new executor. It uses the
same local app target and network, a unique config file and a fresh data volume,
and is automatically removed during cleanup.

Set `Options.ExpectStartupFailure[component] = true` to wait for a nonzero exit
instead of readiness during initial startup (including an added executor).
A timeout or successful exit fails the test. Inspect `Logs`, replace a stopped
container's configuration with `CopyConfig`, then `Start` it to test recovery.
An expected-failure app skips auth provisioning; dependent components must be
omitted or independently configured.

For an app + executor fixture with auth bootstrap, `Start` also waits for a real
uncached `/bin/true` action to complete. This exercises app execution RPCs,
internal scheduler dispatch, executor registration and result delivery, rather
than relying on HTTP health alone. It requires the default bare runner. Fixtures
with `SkipAuthBootstrap` skip this action; `WaitExecutionReady` can be called
explicitly after installing an execution-capable local key. After lifecycle
changes, call `WaitExecutionReady` when the test needs scheduling readiness.
