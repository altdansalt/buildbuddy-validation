# Unknown YAML settings only warn instead of rejecting startup

Title: App, executor and cache proxy start successfully after ignoring a misspelled YAML setting
Components: app / executor / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: Low is retained as the potential operational impact of a typo, not the severity of a confirmed defect. The released loader intentionally warns and ignores unknown settings; strict rejection is a feature request. The reproduction establishes no cross-tenant impact or security bypass.
Status: rejected
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

Independent review rejects the defect classification: the runtime behavior is
reproducible, but strict rejection contradicts the pinned loader's deliberately
permissive contract and its unit tests. See Verification below.

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

## Verification

Verdict: **rejected** (independent review, 2026-10-13). The observation is real,
but `TestUnknownKeyMustRejectStartup` asserts a suite policy that the released
product does not promise. Strict YAML validation would be a feature request.

Source and contract checks, all against `v2.314.0`:

- [`RetypeAndFilterYAMLMap`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/flagutil/yaml/yaml.go#L610-L654)
  documents filtering values absent from the registered flags. Lines 617-621
  deliberately warn, delete the unknown key, and continue without an error.
  `getYAMLMapAndNodeFromData` invokes this filter before populating flags.
- [`TestRetypeAndFilterYAMLMap`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/flagutil/yaml/yaml_test.go#L121-L198)
  supplies an unknown subtree, requires no error, and checks that it is removed.
  [`TestPopulateFlagsFromYAMLMap`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/flagutil/yaml/yaml_test.go#L333-L339)
  explicitly says keys with no corresponding flag name should be ignored.
  These tests establish intent beyond merely observing the implementation.
- [Configuration overview](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config.md)
  describes required and optional options but does not promise rejection of
  unknown keys. [Flag documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-flags.md)
  specifies the monitoring port default as 9090. Its description of port options
  as CLI-only is older than the generic YAML loader; it supplies no strict
  validation contract either.
- [REAPI](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
  governs remote execution/cache RPCs, not server YAML parsing or startup policy.
  No REAPI requirement supports this test's rejection invariant.

Harness/test review: the helper stops only fixture-owned containers, copies the
baseline plus one unknown key into the loaded `/config.yaml`, and restarts the
same pinned image. It refreshes the published HTTP port after restart and uses
component-specific readiness URLs with a bounded HTTP timeout. The executor's
correct `monitoring_port: 9090` remains alongside the misspelled key. All peers
are fixture-local. Baseline startup/auth succeeded, and restart logs identify
both the loaded file and the exact ignored key, ruling out stale ports, wrong
config paths, and unrelated startup failures as explanations. No randomized
input or readiness timeout caused the observed failures.

Independent runtime verification used PR #94's test (commit
`342fffbb14afc840b4c860d192f9609730a43064`) and the three exact pinned digests
listed above, without image overrides:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/config-validation:unknown_key_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Run 1](https://app.buildbuddy.io/invocation/326816d6-6318-4b10-a758-63324be807e6)
  ([runner](https://app.buildbuddy.io/invocation/a19c38cf-4980-410b-ba89-11dad08f66d3)):
  all three subtests fail at the intended assertion after successful readiness;
  each logs `No flags correspond to YAML input at 'monitoring_prt'.` and enables
  monitoring on 9090. The binaries identify release v2.314.0, source commit
  `974929724a4c75f824ab18fbcb560e48bbf2803a`.
- [Run 2](https://app.buildbuddy.io/invocation/9a0ec304-5efc-4d68-a56d-3b208d3238b5)
  ([runner](https://app.buildbuddy.io/invocation/1e0824f9-5516-4c19-a4fa-646b1e615c2d)):
  all three subtests fail at the same readiness assertion, with the same warning,
  monitoring port, release, and image digests.

The identical behavior in both independent runs confirms a deterministic
implementation choice, not a flaky test or confirmed correctness defect.
Severity stays low for the hypothetical operational concern; status is rejected.
Only this finding is changed in the verification PR. The coordinator should
remove the `bug` classification or convert the test to passing coverage of
warning-and-ignore behavior unless a strict-validation contract is adopted.
