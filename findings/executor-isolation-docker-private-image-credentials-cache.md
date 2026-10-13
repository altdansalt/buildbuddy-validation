# Cached private Docker image ignores changed registry credentials

Title: Cached private Docker image ignores changed registry credentials
Components: executor
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8
Severity: low
Severity justification: Retained low as the original impact estimate for traceability; this is rejected as a product defect. The observed reuse is intentional, bounded same-group image authorization caching, not demonstrated cross-group access.
Status: rejected
Repro test target: //tests/executor/isolation-docker:registry_credentials_cache_bug
BuildBuddy invocation: https://app.buildbuddy.io/invocation/7965a14d-f85f-47cd-a907-f32ab04131ee

## Expected behavior

A request with an invalid `container-registry-password` should not inherit the
registry authorization established by a request with valid credentials.
The release's [NewImageCacheToken comment](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/container/container.go#L763)
says the token represents the authenticated group ID, pull credentials, and
image reference. This is a suspected implementation/contract mismatch, not a
registry-authentication requirement imposed by the generic REAPI specification.
If group-wide image authorization is the intended policy even when explicit
credentials change, this finding should be rejected and that policy documented.

## Actual behavior

The reproducer owns a Docker daemon, app, executor and authenticated local
`registry:2.8.3` container. It publishes an Alpine workload image, removes the
publisher's local tag, and executes four actions in the same local group:

1. Wrong credentials on a cold image: rejected; command produces no output.
2. Valid credentials: success; command prints `private-workload`.
3. Wrong credentials while cached: success; command prints `private-workload`.
4. Remove the local image tag and repeat the wrong credentials: rejected again;
   command produces no output.

Both cold-cache controls run before the failing assertion. They distinguish
registry enforcement from cached authorization and rule out an accidentally
public registry, a publisher-preloaded tag, and successful use of the wrong
password. Docker login configuration exists only on the test runner, is never
mounted into the executor, and contains only disposable fixture credentials.
No executor registry credentials, bypass property or external deployment is used.

Minimal reproducer runs:

- https://app.buildbuddy.io/invocation/7965a14d-f85f-47cd-a907-f32ab04131ee
- https://app.buildbuddy.io/invocation/8c6cffbd-8f89-4647-a68d-131669c95849

Earlier broader tests observed the same cached acceptance in
https://app.buildbuddy.io/invocation/00075e58-88b5-4019-8666-13cb0cad1f4d and
https://app.buildbuddy.io/invocation/5e0a5fc8-066c-4fb7-8abb-5f2ad97443ec.
The original report was unverified; independent review below rejects its per-action credential expectation.

## Logs excerpt

```text
wrong_credentials_after_cache: status=<nil> exit=0
Error: Should not be zero, but was 0
```

The minimal reproducer additionally logs cold/evicted status messages, cached
stdout, and the exact fixture workload digest. Registry requests are preserved
in `round2-registry.log`; component logs and inspect/config artifacts are
preserved by the harness.

## Notes

The app also uses enterprise-v2.314.0:
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
There are no image overrides. Workload base: `alpine:3.21.3`; its pushed private
manifest digest is logged separately in each invocation. `httpd:2.4.62-alpine`
generates bcrypt htpasswd entries; no BuildBuddy image is rebuilt.

The private daemon permits HTTP registries in its fixture-owned 10/8 address
pool and uses the public Docker Hub mirror for image downloads. Only the
executor receives its Docker socket. Workload containers receive neither that
socket nor registry login configuration. Actions disable action-cache storage
and skip lookup; all RPCs use bounded contexts and the fixture's local API key.

Source inspection explains the observation: `NewImageCacheToken` accepts
`creds` but returns only `GroupID` and `ImageRef`. `pullImageIfNecessary` skips
registry authentication when that token is authorized and the image exists.
The default image-auth token lifetime is 15 minutes. This is intentionally a
same-group test and does not infer cross-group access or a token-expiry defect.
Explicit local image eviction makes the invalid request fail again.

Reproduce twice on remote runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-docker:registry_credentials_cache_bug --test_tag_filters=bug --nocache_test_results --test_output=all --test_arg=-test.v
```

No shared harness/dependency changes are required. Findings stay in this repo.

## Verification

Independent review on 2026-10-13: **rejected**. The observation is real, but
requiring changed registry credentials to invalidate an existing same-group
image authorization contradicts an intentional implementation change. The
credential-sensitive comment cited above is stale; it is not sufficient evidence
of a regression.

### Source, documentation, and specification

- BuildBuddy [PR #967](https://github.com/buildbuddy-io/buildbuddy/pull/967)
  introduced short-lived registry credentials through remote headers. Its
  [commit 34a4e6d](https://github.com/buildbuddy-io/buildbuddy/commit/34a4e6d12a8110081c32eee656644c8d75c745f8)
  explicitly removes `UserHash` and `PasswordHash` from `ImageCacheToken` and
  from its construction. The PR describes avoiding invalidation on every build
  when credentials are short-lived. This predates the reviewed release and
  establishes intent more strongly than the leftover function comment.
- In [v2.314.0 container.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/container/container.go#L716),
  a successful pull refreshes authorization keyed by authenticated group and
  image reference. If that token is live and the image is on disk, the pull is
  skipped. The [token definition](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/interfaces/interfaces.go#L1832)
  likewise has only `GroupID` and `ImageRef`. The default lifetime is 15 minutes;
  the test exercises reuse within that lifetime, not use after expiry.
- The pinned [platform documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-platforms.md#L353)
  describes these properties as credentials **to pull** private images. The
  [image-caching documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-container-image-caching.md)
  describes avoiding upstream fetches for images already on the executor's
  disk. Neither promises per-action validation of changed credentials. The
  public documentation could state the same-group authorization lifetime more
  clearly, but that omission does not overturn the deliberate source change.
- [REAPI Platform.Property](https://github.com/bazelbuild/remote-apis/blob/master/build/bazel/remote/execution/v2/remote_execution.proto#L934)
  assigns interpretation of property names and values to the server. It does
  not specify registry credentials or require reauthentication on every action.
  `SkipCacheLookup` and `DoNotCache` govern action-result caching, not Docker
  image authorization caching.

### Test review and independent execution

Reviewed PR #139 at `44fd688ffa26ab46d065f72efd9f7d82a3602c7d`, including the
bug test, Docker/registry helpers, and BUILD target. The private daemon, registry,
app, executor, login config, and API key are fixture-local. The login config is
not mounted into the executor. Workload and auxiliary images use digest-pinned
mirror references. Each action has a bounded RPC context; action-result cache
lookup and storage are disabled. Sorted platform properties and explicit
credentials reach the normal Docker pull path. There is no apparent harness,
network, or credential-helper explanation for the cached acceptance.

Both independent runs used the original PR checkout and the command below,
without changing the test or overriding release images:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-docker:registry_credentials_cache_bug --config=nobug --test_tag_filters= --nocache_test_results --test_output=all --test_arg=-test.v
```

- [Run 1](https://app.buildbuddy.io/invocation/7149807a-9c16-470c-929b-821bb647039a)
  ([remote runner](https://app.buildbuddy.io/invocation/dffa6391-db6c-4884-a8ad-06322183e6cc)):
  failed at the final cached-credential assertion after 74.14 seconds.
- [Run 2](https://app.buildbuddy.io/invocation/b6a75824-3934-4012-8f0a-3681dca95dfd)
  ([remote runner](https://app.buildbuddy.io/invocation/9a64ec8f-9e26-48ea-b2c1-81b0280fef24)):
  failed at the same assertion after 71.48 seconds.

In both runs, wrong credentials before caching and after explicit image eviction
returned status 14 with `unauthorized: authentication required`, and no stdout.
The valid request and the cached wrong-credential request both returned status
OK with `private-workload\n`. Both logged the exact app/executor digests listed
above and workload manifest
`sha256:ec1b05d1eac264d9204a57f4ad9d4dc35e9e756e9fedaea0674aefc7edb1d6a4`.
The identical control outcomes rule out a flaky registry or accidental public
image. They establish reproducible intended behavior, not correctness of the
final assertion.

Severity remains **low** only as the original report's bounded impact estimate;
there is no confirmed vulnerability here. No cross-group bypass, expired-token
reuse, or registry credential revocation failure outside the authorization
lifetime was tested. Within this review's finding-only scope, no test is changed.
The originating PR should withdraw this bug assertion or convert it to coverage
for the intended same-group authorization cache. A separate documentation cleanup
could remove the stale credential-sensitive comment and explain the lifetime.
No shared harness or dependency changes are needed.

## Passing regression coverage

PR #139 converts the original bug reproducer to
`//tests/executor/isolation-docker:registry_credentials_cache`, included in
`--config=nobug`. It asserts the accepted same-group cached-image reuse with
changed credentials, while retaining cold-image and explicit-eviction rejection
controls. The original report and independent rejection are retained above.
