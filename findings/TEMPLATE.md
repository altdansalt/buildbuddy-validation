# Finding title

Title: Concise description of the violated behavior
Components: app / executor / cacheproxy
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/...@sha256:...
Severity: low
Severity justification: Explain impact, prerequisites, and affected trust boundaries.
Status: unverified
Repro test target: //tests/<component>/<area>:<test>
BuildBuddy invocation: https://app.buildbuddy.io/invocation/<id>

## Expected behavior

Describe the protocol or documented contract. Link relevant specifications.

## Actual behavior

Describe the observed difference and impact. Record both reproduction runs and
explain how harness/configuration error was ruled out. Status becomes confirmed
only after the same suspected bug reproduces at least twice.

## Logs excerpt

```text
Include the minimal relevant excerpt. Test-local keys are disposable; redact any
unrelated credentials. Full container logs belong in undeclared test outputs.
```

## Notes

Include exact config/flags, version overrides, limitations, mitigation, and links
to additional invocations. Findings remain in this repository; no upstream report.
