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

Coverage: image tag and digest references, missing tags, explicit numeric UID/GID,
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

Follow-up coverage: authenticated private registries, mutable-tag updates,
concurrent actions, cancellation during pulls, named image users, Podman and OCI
providers, and recycled runners. These tests deliberately use fresh runners;
they do not infer guarantees about recycled container `/tmp` state.
