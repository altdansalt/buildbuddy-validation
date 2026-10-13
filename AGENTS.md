# Released BuildBuddy validation workers

Work autonomously in `~/repo` (`altdansalt/buildbuddy-validation`). Nobody will
answer clarification questions. This is a shared scaffold for independent areas.

## Non-negotiable scope

- Test released Docker images of the enterprise app, executor and cache proxy.
  Start the components **inside the test**, on `bb remote` runners. Never build
  BuildBuddy from source or run component tests on this worker VM.
- Always run Bazel builds/tests with `bb remote <bazel command>`; never local
  `bazel`, `bb build`, or `basil build`. `basil` is an alias for `bb`.
- Never test against `app.buildbuddy.io`, `remote.buildbuddy.io`, or any other
  shared deployment. Component RPCs and HTTP calls must address fixture-local
  containers. `BUILDBUDDY_API_KEY` is solely for dispatching `bb remote`: never
  read it in tests, forward it with `--env`/`--test_env`, or put it in configs.
  The fixture creates disposable local API keys through the local app.
- Findings stay in this repo. Never file upstream issues, PRs, or send reports.
- Work on your own branch, make clear commits, push via `origin`, and open a PR
  to `main` with `GH_HOST=github.int.exe.xyz gh pr create`. Never push to main.

## Ownership

Your area is `tests/{executor,app,cacheproxy,crosscutting}/<area>/`, with its own
`BUILD.bazel`. Only edit your own area and your uniquely named finding files.
Do not edit `MODULE.bazel`, `MODULE.bazel.lock`, `go.mod`, `go.sum`, `harness/`,
root configuration, another agent's tests, or these instructions. Request shared
harness/dependency changes in your final summary instead. Do not commit generated
INDEX changes in area PRs; the coordinator regenerates it after merging findings.

## Runner checkout on exe.dev

The internal GitHub host is reachable from the worker but not the hosted runner.
The repository is public, so use public GitHub for **fetching on the runner**,
while retaining the internal `origin` for pushes and `gh`:

```sh
git remote get-url public >/dev/null 2>&1 || git remote add public https://github.com/altdansalt/buildbuddy-validation.git
git config buildbuddy.remote-bazel-remote-name public
GIT_CONFIG_GLOBAL=/dev/null bb remote test //... --config=nobug
```

`GIT_CONFIG_GLOBAL=/dev/null` bypasses the worker's GitHub URL rewrite for this
command only. Do not change global Git settings. No tokens are needed.

## Harness

Read `harness/README.md` and `docs/environment.md`. Import
`github.com/altdansalt/buildbuddy-validation/harness` and depend on
`//harness:go_default_library` (or `//harness`). Copy the example BUILD dependency
labels from `tests/cacheproxy/smoke` and `tests/executor/smoke`.

```go
f := harness.Start(t, harness.Options{
    Components: []harness.Component{harness.App, harness.Executor},
})
c := f.Clients(harness.App) // CAS, ByteStream, AC, Execution, Capabilities, BES
```

Use a bounded context for RPCs. `Start` registers cleanup immediately, waits for
readiness, provisions self-auth and a local group/API key, and preserves configs,
container logs, inspect metadata, and owned-daemon logs in
`TEST_UNDECLARED_OUTPUTS_DIR`. Components are an exact subset; external peer
targets must be another test-local fixture. Override config using nested
`harness.Config`; override flags via `Options.Args` and Docker settings via
`Options.DockerArgs`. For mock OIDC or no-login cases set `SkipAuthBootstrap`.

All component tests need `tags = ["local", "requires-network"]`, large size and
long timeout. `.bazelrc` disables sandboxing and uses local test execution inside
the remote runner. The default fixture starts an isolated dockerd if necessary;
`bb remote --runner_exec_properties=init-dockerd=true test ...` may use the runner's
prestarted daemon instead. No privileged executor container is needed for the
baseline bare runner. `/dev/kvm` is unavailable on the validated hosted runner;
do not interpret nested-Firecracker startup failures as product bugs.

Images default to digest-pinned `enterprise-v2.314.0`. Use
`--test_arg=-harness.executor_image=enterprise-v2.X.Y`, or
`--test_env=BB_EXECUTOR_IMAGE=...` for a version override. There are corresponding
app/cacheproxy options; `BB_VERSION` changes all three. Keep exact image/digest
information in every finding, including when overriding defaults.

## Images

Never pull images from Docker Hub, including unqualified names or `docker.io`.
Use `mirror.gcr.io` (for example, `mirror.gcr.io/library/alpine`) or `gcr.io`,
and pin fixture images by digest.

## Bugs and findings

- Passing coverage tests stay untagged with respect to `bug` (runner tags remain).
- A reproducer that fails until the bug is fixed gets `tags = ["bug", "local",
  "requires-network"]`. The default suite and `--config=nobug` exclude `bug`.
  Run a repro explicitly with `--test_tag_filters=bug` and `--nocache_test_results`.
- **Verify a suspected bug reproduces at least twice and rule out harness/config
  error before filing.** Do not label environment, networking, unsupported auth,
  or misconfigured isolation errors as confirmed bugs.
- Copy `findings/TEMPLATE.md` to
  `findings/<component>-<area>-<short-slug>.md`. Include severity justification,
  exact image version/digest, status (`unverified`/`confirmed`/`rejected`), repro
  target, expected/actual behavior, log excerpt, both invocation links, and notes.
- `findings/INDEX.md` is generated by `tools/findings_index`; nobody edits it by
  hand. The coordinator can run `tools/findings_index --check` to detect drift.

Finish with the change summary, branch/commit, PR URL, BuildBuddy invocation
links, anything undone, and any shared changes you need the coordinator to make.
