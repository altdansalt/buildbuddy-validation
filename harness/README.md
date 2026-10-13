# Go fixture for released images

`//harness` aliases `//harness:go_default_library`. Component tests run on the
hosted VM using `bb remote`, with `local` and `requires-network` Bazel tags.
The default fixture creates a Docker network, starts local Redis when starting
an app, and starts the requested released containers. SQLite, blob storage,
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

## Browser UI checks

Browser fixtures must use an HTTP origin under `.test`: Chromium's HSTS preload
list upgrades the default `http://app:8080` hostname to HTTPS. Configure the app
and its Docker alias together before starting the fixture:

```go
f := harness.Start(t, harness.Options{
    Components: []harness.Component{harness.App},
    Config: map[harness.Component]harness.Config{
        harness.App: {"app": harness.Config{"build_buddy_url": "http://buildbuddy.test:8080"}},
    },
    DockerArgs: map[harness.Component][]string{
        harness.App: {"--network-alias", "buildbuddy.test"},
    },
})
```

The released app serves its bundled JavaScript UI on this same HTTP origin.
`f.Browser(ctx, f.Web, groupID)` starts digest-pinned headless Chromium inside
the fixture network and imports the selected WebSession's login cookies. It uses
the released app's real JavaScript UI and the `Selected-Group-ID` cookie; API keys
alone do not establish a web login. Self-auth has one fixed user. For other users,
supply a WebSession with cookies obtained from a test-local OIDC login. The
session must use this fixture's self-auth origin, reachable inside its network.

Use a bounded context, then `browser.Open("/settings/org/api-keys",
".api-keys-list")` to navigate and wait for a page-specific visible element.
`browser.Text(selector)` reads rendered text; `browser.Run(...)` accepts chromedp
actions for element queries, clicks and waits. Choose readiness selectors that
appear after data loads, and retain a positive assertion when checking absence.
See `//tests/statemachine/ui-smoke:go_default_test` for gRPC create/delete checked
through the settings UI after navigation. No automatic push refresh is assumed.
Browser containers share normal fixture cleanup/log/inspect capture; failed tests
additionally attempt to save a screenshot and rendered body text. Keep the
browser context alive through cleanup if failure artifacts are needed.

Validated twice with
`bb remote test //tests/statemachine/ui-smoke/... --test_output=errors --nocache_test_results --zip_undeclared_test_outputs`:
[run 1](https://app.buildbuddy.io/invocation/6040da4a-16f8-4875-b321-d48ac04bc373),
[run 2](https://app.buildbuddy.io/invocation/4cd7f931-c940-4bfd-9d2e-212ab575cb8a).
