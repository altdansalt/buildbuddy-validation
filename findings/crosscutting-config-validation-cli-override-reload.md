# Config reload loses command-line security flag override

Title: SIGHUP resets CLI-overridden CSP flag to its default
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: An operator enabling strict CSP via CLI loses that HTTP security policy after a config reload. Requires a SIGHUP reload; this does not by itself demonstrate an exploitable application vulnerability.
Status: confirmed
Repro test target: //tests/crosscutting/config-validation:cli_reload_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/acc4b852-9585-4801-94d0-c8e53012b762

## Expected behavior

The explicit `--app.strict_csp_enabled=true` override should remain true after
SIGHUP, even when YAML contains `app.strict_csp_enabled: false`. The pinned
[reload contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/config/config.go)
says it resets defaults, re-parses flags, and loads the config file. The
[YAML loader](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/flagutil/yaml/yaml.go)
explicitly preserves command-line precedence.

## Actual behavior

Startup returns Content-Security-Policy on GET `/`. After an accepted reload,
the same handler returns no CSP header. Reproduced independently with uncached
runs:

- https://app.buildbuddy.io/invocation/acc4b852-9585-4801-94d0-c8e53012b762
- https://app.buildbuddy.io/invocation/1332ba1a-f24e-4ef5-9eff-313ca6c18dbf

Each test starts a digest-pinned local app through the harness, verifies startup
CSP, changes only YAML `server_type` to `cli-reload-complete`, sends SIGHUP, and
waits for that marker on the local monitoring `/flagz` endpoint before checking
CSP. No container restart, remote peer, or credentials from the worker are
involved. The unchanged contradictory YAML proves CLI precedence at startup.
Independent verification is recorded below.

## Logs excerpt

```text
--- FAIL: TestSIGHUPPreservesCLIOverride
    cli_reload_test.go:30: CLI precedence after reload lost the expected CSP header
```

## Notes

Pinned source explains the observation: `config.Reload()` calls
`flagutil.ResetFlags()` and `Load()` without parsing the original arguments.
[common.ResetFlags](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/flagutil/common/common.go)
writes each `DefValue` directly without clearing the flag set's explicitly-set
tracking. `PopulateFlagsFromYAMLMap` collects that tracking using `Visit`, so
YAML also skips the reset CLI flag. For strict CSP, the result is the default
false even if the YAML value is true. The minimal repro uses false YAML to
make the intended CLI precedence explicit. Other CLI flags may be affected;
this test establishes the dynamic CSP case only. No image overrides were used.

Run on hosted runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/config-validation:cli_reload_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

Original failing suite invocation:
https://app.buildbuddy.io/invocation/739fdfbd-7102-4f50-ba45-93df9f64eb47


## Verification

Verdict: **confirmed**. Independently reviewed PR #140 and its bug-tagged
`TestSIGHUPPreservesCLIOverride` on 2026-10-13. The test correctly demonstrates
loss of a command-line override, rather than failure to apply a startup-only
setting. Medium severity is retained: a routine operator-triggered reload
silently removes an explicitly enabled browser security policy; there is no
demonstrated exploit or remote unauthenticated reload trigger.

Source reviewed at release `v2.314.0`, commit
`974929724a4c75f824ab18fbcb560e48bbf2803a`:

- [Enterprise main](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/cmd/server/main.go#L164-L179)
  parses CLI flags before loading YAML and installs the SIGHUP handler.
- [Reload and SIGHUP contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/config/config.go#L136-L184)
  explicitly promises re-parsing flags, but `Reload` only calls `ResetFlags`
  and `Load`; neither re-parses the original arguments.
- [ResetFlags](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/flagutil/common/common.go#L282-L299)
  restores `DefValue` without clearing explicitly-set tracking.
  [YAML precedence](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/flagutil/yaml/yaml.go#L706-L720)
  uses `Visit` to preserve CLI flags; therefore YAML cannot restore this flag
  after it has been reset to its default false.
- [Security header handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/http/interceptors/interceptors.go#L112-L125)
  reads `strictCspEnabled` on each request. The
  [configuration docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config.md)
  caveat that some options require restart does not explain this dynamic flag.
  [Flags docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-flags.md)
  permit configuration options as CLI flags. REAPI is not applicable to this
  process configuration and HTTP security-header contract; no REAPI assertion
  is made by the reproducer.

Harness/configuration and flakiness checks: the harness appends the explicit
CLI flag after `--config_file=/config.yaml`; the test verifies CSP at startup
with contradictory YAML, then copies the same generated config changing only
`server_type`. It signals the fixture-owned container without restarting it.
The monitoring endpoint is published on runner loopback, and the unique live
`/flagz` marker establishes that the edited YAML was read before the final
HTTP assertion. Both requests require HTTP 200 and address the same local app.
A marker could theoretically appear while loading is still in progress, but
that cannot explain loss of the CLI flag: correctly re-parsing that flag would
restore true before YAML could expose the marker. The source also predicts
persistent false after reload. No shared deployment, external credentials,
image override, or component execution on the worker was used.

Independent uncached reproductions used:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/config-validation:cli_reload_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Both runs used the app digest recorded above and failed specifically at
`cli_reload_test.go:30` with `CLI precedence after reload lost the expected CSP
header`, after passing startup CSP and reload-marker checks:

- Run 1: https://app.buildbuddy.io/invocation/ae3a176e-3c4e-4a89-9a28-6e45b171ec1b
- Run 2: https://app.buildbuddy.io/invocation/6de1c31f-b318-4902-9ebc-6f6b1d387ad8
