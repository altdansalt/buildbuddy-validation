# BuildBuddy released-component validation

Shared Go/Bazel scaffold for tests against released enterprise app, executor and
cache-proxy Docker images. Each test starts isolated local components on a
BuildBuddy hosted runner. The current pins are `enterprise-v2.314.0` by digest.

Read [AGENTS.md](AGENTS.md) before contributing, [harness/README.md](harness/README.md)
for fixture usage, and [docs/environment.md](docs/environment.md) for investigation
results and runner requirements. Findings stay here; no upstream issues or PRs.

On the exe.dev worker, set up public runner checkout once (pushes still use origin):

```sh
git remote add public https://github.com/altdansalt/buildbuddy-validation.git
git config buildbuddy.remote-bazel-remote-name public
GIT_CONFIG_GLOBAL=/dev/null bb remote test //... --config=nobug
```

The ordinary suite excludes `bug`-tagged reproducers. Run a suspected bug target
with `--test_tag_filters=bug --nocache_test_results`. Test outputs preserve all
container logs. See [findings/README.md](findings/README.md) for reporting format.
