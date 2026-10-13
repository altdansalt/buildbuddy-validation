# Docker isolation coverage

Run on hosted runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/isolation-docker/... --config=nobug
```

The test starts a private, test-owned Docker daemon before calling the shared
fixture. `DOCKER_HOST` selects that daemon for the fixture; the executor receives
its socket and a workspace bind mount. The executor and daemon see identical
workspace paths, and filecache is on the same filesystem. The daemon disables
iptables and IP masquerading and has no default bridge. Actions use a dedicated
bridge network for the network-on check; network-off checks require only loopback.
No network assertion depends on Internet connectivity. Image pulls use the runner's
network. The socket is not mounted into action containers.

Coverage: image tag and digest references, missing tags, unauthenticated
private-registry denial, explicit numeric UID/GID,
working directories, environment values, CAS inputs and outputs through workspace
mounts, configured read-only mounts, network on/off and property precedence,
invalid network status, nonzero process exits, filesystem cleanup between actions,
container cleanup, and executor panic/fatal log checks.

Behavioral references at the pinned release:

- [Docker provider](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/containers/docker/docker.go):
  user selection, workspace bind mounts, network translation, fresh container lifecycle.
- [Platform properties](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go):
  `GetEffectiveDockerNetwork`, `dockerUser`, and precedence of `network` over `dockerNetwork`.
- [Runner](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/runner/runner.go):
  `executor.host_root_directory`.
- [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto):
  `Command.working_directory`, `environment_variables`, `output_paths`, and
  `ActionResult.exit_code`.

Missing image errors must prevent command execution. The test does not impose a
specific gRPC code for Docker registry failures: the REAPI does not standardize
an image registry error mapping. Invalid network values, in contrast, are
explicitly rejected as INVALID_ARGUMENT by the released platform parser.

Round 2 adds a real fixture-local authenticated `mirror.gcr.io/library/registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373` container.
`mirror.gcr.io/library/httpd:2.4.62-alpine@sha256:88c55a4fcc1df6cf9e7b38360fb6a6feb4d5c5cf5a27a9ff38096ae585af707f` generates a bcrypt htpasswd entry for disposable credentials.
The private daemon permits HTTP registries in its test-owned 10/8 address pool;
no daemon settings outside this fixture change. Docker login configuration stays
in a test temporary directory and is never mounted into the executor. Action
credentials are supplied through the documented `container-registry-username`
and `container-registry-password` properties. Checks cover denied anonymous
pulls, successful authenticated pulls,
and named user/group resolution from the image's `/etc/passwd` and `/etc/group`.

Two locally built Alpine variants publish different content under the same tag.
The test restores the old local tag to model a registry update independently of
the executor cache, then checks complete old-or-new content. The release permits
cached authorized images until its image-auth token expires; immediate tag
refresh is not assumed. After explicit local tag eviction, a fresh pull must
return the new content. Both immutable digests must continue to return their
original content. Docker image builds occur inside the test on the remote runner
and build only small workload images, never BuildBuddy itself.

Concurrent actions use replay seed `20261012` to generate distinct markers and
check private `/tmp` contents and intact CAS inputs. A short Action timeout
cancels a running Docker shell with a child process; the test requires
DEADLINE_EXCEEDED, eventual container removal, and successful subsequent work.
Released-image runtime probes record Podman/runc/crun availability without
installing software into the product image. Separate Podman and OCI fixtures
enable their providers, grant the executor container privilege and host cgroup
access, and use a registry sharing the executor's network namespace. All nested
pulls address `localhost:5000`; the runner pushes the same workload through the
executor's fixture-local IP. Podman uses vfs storage and an explicit insecure
localhost registry entry; OCI explicitly allows loopback registry addresses.
Both providers check CAS input access and fresh container filesystem state.
All external workload and registry helper images use digest-pinned
`mirror.gcr.io/library` references. The deliberately nonexistent Alpine tag also
addresses the mirror. No pull in this area addresses Docker Hub directly.

Follow-up coverage: explicit user cancellation and cancellation during pulls,
image-auth token expiry, and recycled runners. These tests use fresh runners;
they do not infer guarantees about recycled container `/tmp` state.

`network=off` and its precedence over `dockerNetwork` are covered by the separate
bug-tagged `:network_off_bug` target. See
[the finding](../../../findings/executor-isolation-docker-network-off-ignored.md).
Run it explicitly with `--test_tag_filters=bug --nocache_test_results`.
The ordinary suite retains the passing legacy `dockerNetwork=off` case.

Validation: [green nobug suite](https://app.buildbuddy.io/invocation/73dbab49-1d53-4a7f-b508-63f8fc9b6179),
[expected failure 1](https://app.buildbuddy.io/invocation/100da40a-c8be-460a-aa8f-1c0e700e65d1),
and [expected failure 2](https://app.buildbuddy.io/invocation/d8eb319f-9336-47b0-a74d-f45d978d9b07).

No shared changes are required. An optional harness helper exposing its selected
Docker socket and planning matching executor workspace mounts would remove the
need to duplicate private-daemon startup in this area. No dependency changes are
needed.

The passing `:registry_credentials_cache` target checks the accepted same-group
image authorization policy: wrong registry credentials fail on a cold image,
succeed while the image is cached after a valid pull, and fail after eviction.
Authorization tokens intentionally omit registry credentials. This regression
coverage runs in the ordinary `--config=nobug` suite. See
[the rejected finding](../../../findings/executor-isolation-docker-private-image-credentials-cache.md)
for the independent review and upstream rationale. No cross-group access is claimed.
