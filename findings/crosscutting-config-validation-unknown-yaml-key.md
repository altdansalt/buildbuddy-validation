# Unknown YAML settings only warn instead of rejecting startup

Title: App, executor and cache proxy start successfully after ignoring a misspelled YAML setting
Components: app / executor / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: An operator typo leaves the default monitoring port active while the deployment reports ready. This is an operational configuration correctness issue requiring control over deployment configuration; the reproduction establishes no cross-tenant impact or security bypass. A warning names the setting, reducing the risk.
Status: unverified
Repro test target: //tests/crosscutting/config-validation:unknown_key_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/a73eb7a0-1e57-4378-a5f4-c6e3b4a1ed1a

## Expected behavior

The config-validation area's requested invariant is that a bad setting produces
a clear startup error naming the setting and does not fall back to a default.
Appending the typo `monitoring_prt: 9191` to a valid YAML file should therefore
reject startup with a diagnostic identifying `monitoring_prt`.

This is a suite policy expectation, not a Remote Execution API requirement.
The [pinned YAML loader](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/flagutil/yaml/yaml.go)
explicitly implements permissive behavior: `RetypeAndFilterYAMLMap` logs a warning
and deletes keys absent from the flag type map. Maintainers should decide whether
strict startup validation, potentially as an opt-in setting, is appropriate.

## Actual behavior

All three released components warn about the unknown key, ignore it, and return
HTTP 200 from fixture-local `/readyz?server-type=<component>`. App and cache proxy
logs explicitly show monitoring starting on the default port 9090 instead of 9191.
The implementation is not silent in logs, but successful startup does not enforce
the requested rejection invariant.

The same fixed minimal typo reproduced independently twice:

1. [First run](https://app.buildbuddy.io/invocation/a73eb7a0-1e57-4378-a5f4-c6e3b4a1ed1a): all three subtests fail on successful readiness after the typo.
2. [Second run](https://app.buildbuddy.io/invocation/ef75fa23-598a-4f71-8b93-07ad947cf780): all three subtests fail identically.

Both runs used the pinned digest images above and test-local app/Redis peers.
Each fixture first reached readiness with baseline config and provisioned local
auth. The test stopped its existing owned container, copied baseline YAML plus
the single typo to `/config.yaml`, and restarted it. It refreshed Docker's random
host port assignment before checking readiness. Logs captured from the restart
show that the varied file was loaded and the precise key was ignored. No component
RPC used a shared deployment. Source inspection agrees with these observations.

Status remains `unverified` as requested for area submissions: the runtime
behavior is twice reproduced, but classifying an explicitly permissive loader as
a product defect requires policy review.

## Logs excerpt

```text
app:
Reading buildbuddy config from '/config.yaml'
WRN No flags correspond to YAML input at 'monitoring_prt'.
Enabling monitoring (pprof/prometheus) interface on http://0.0.0.0:9090
unknown monitoring_prt accepted; component ready with typo ignored

executor:
Reading buildbuddy config from '/config.yaml'
WRN No flags correspond to YAML input at 'monitoring_prt'.
unknown monitoring_prt accepted; component ready with typo ignored

cacheproxy:
Reading buildbuddy config from '/config.yaml'
WRN No flags correspond to YAML input at 'monitoring_prt'.
Enabling monitoring (pprof/prometheus) interface on http://0.0.0.0:9090
unknown monitoring_prt accepted; component ready with typo ignored
```

## Notes

Reproduce from this branch with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote --run_from_branch=area/crosscutting-config-validation test //tests/crosscutting/config-validation:unknown_key_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

No image override or component CLI override is used. The baseline executor YAML
already contains the correctly spelled `monitoring_port: 9090`; the added typo
is a separate unknown key. App/proxy use that setting's default. Correct the typo
to apply the desired setting; review config warnings as a mitigation.

The [passing regression suite](https://app.buildbuddy.io/invocation/f0c0f273-c583-48ee-ac73-c81ec4173779)
checks valid seeded configs, CLI precedence, environment expansion, wrong port
types/ranges, and required/ranged executor settings. Its run used
`--runner_exec_properties=init-dockerd=true` to share cached images across fixtures.
A previous regression run was blocked by Docker Hub's Redis rate limit; this is
an infrastructure dependency issue and not part of the finding.

The helper works around the harness's lack of expected-startup-failure support
inside the owned area. Original/variation YAML, logs and inspect metadata are
preserved in undeclared outputs. An earlier probe used stale Docker host ports;
that run is excluded from the evidence above.
