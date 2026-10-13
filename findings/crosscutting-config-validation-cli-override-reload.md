# Config reload loses command-line security flag override

Title: SIGHUP resets CLI-overridden CSP flag to its default
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: An operator enabling strict CSP via CLI loses that HTTP security policy after a config reload. Requires a SIGHUP reload; this does not by itself demonstrate an exploitable application vulnerability.
Status: unverified
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
Status remains unverified as requested, despite the two reproductions.

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
