# Released startup configuration validation

Run on hosted runners only:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/config-validation/... --config=nobug
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/config-validation:unknown_key_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

`config_test` logs its deterministic seed (default 314). Extend valid input
sampling with `--test_arg=-config.seed=123 --test_arg=-config.iterations=12`.
The default samples two fixtures with all three released components, randomized
HTTP ports, CLI precedence, environment-expanded executor root directories,
and proxy upload/app read consistency. RPCs have bounded contexts. Invalid
configuration checks cover the common HTTP port's type and range, an empty
executor app target, and executor traffic percentages outside 0..100.

Startup failure probes first start the harness's valid local fixture, then stop,
copy a variation over `/config.yaml`, and restart that same owned container.
This bypasses the harness's unconditional readiness requirement for negative
cases without creating containers outside its ownership. They discover the
fixture's Docker socket by inspecting only the fixture's app container name;
this supports both prestarted and harness-owned Docker daemons. Original YAML,
variations, container logs and inspect metadata remain in fixture artifacts.
Logs are bounded to the current restart timestamp.

Correctness references for the pinned v2.314.0 release:

- [Config loading and string expansion](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/config/config.go)
- [Typed YAML loading, CLI precedence and unknown-key warning](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/flagutil/yaml/yaml.go)
- [Executor required target and traffic range](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/cmd/executor/executor.go)
- [REAPI CAS contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)

Unknown-key rejection is the requested suite invariant. The pinned loader
explicitly warns and drops unknown keys, so the bug target records an intentional
permissive implementation that conflicts with that invariant, rather than
claiming the warning is absent or that REAPI specifies YAML startup behavior.

Not covered yet: all feature-specific required settings, conflicting isolation
providers, external secret providers, config reload, and comprehensive typed
flag families. No host credential or cloud peer is used by the tests.
