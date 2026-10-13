# Organization state across app, proxy, web endpoints and CLI

The reference model is checked after **every** CreateGroup, UpdateGroup and
SetGroupStatus step, including repeated identical updates/status sets. It covers
three organizations, preserves unrelated organizations, and reuses a relinquished
slug for a new organization without changing the original organization's identity.
There is no DeleteGroup RPC in the release, and the proxy has no group-management
RPC. Unsupported group deletes or proxy group writes are therefore not attempted.

| Mutation | Surfaces checked after each step |
| --- | --- |
| CreateGroup | GetUser over gRPC and cookie-authenticated JSON protolet (exact membership count, selected group/access, identity, domain, every modeled policy and status); GetGroup by ID and slug over both transports; GetGroupUsers creator/count/admin role; GetApiKeys/GetUserApiKeys inventories and GetApiKey/GetUserApiKey value reads; GetBazelConfig options/credential visibility; GetExecutionNodes; GetInvocationOwner; sharing and workflow/codesearch gates; invocation/log/download projections; CLI login and organization selection; app/proxy ByteStream, CAS and AC |
| UpdateGroup | All the above, all seven booleans and all three suggestion preferences, domain ownership, repeated identical updates through both transports, exact NotFound for relinquished slugs, and later slug reuse with a distinct group ID |
| SetGroupStatus | All the above through free, enterprise, trial, usage-based, open-source, blocked, and recovery; repeated status sets; blocked API authorization and recovery with personal and organization credentials on both app and proxy |

The membership and personal-key checks from local audit commit `8cd8dd8` are
incorporated without perturbations. Config options use explicit release-derived
expectations, not a learned baseline. Key IDs/values are captured from creation;
settings inventories must omit values, while separate value-read RPCs must match
the created credentials. Disabling user-owned keys hides their value even from admin readback (NotFound),
while personal-key API access and the user-key inventory are denied. Re-enabling
the policy must restore exactly the original key ID and value. Capability order is a set, not a state change.

Additional effective policy checks:

* Sharing: a real completed BES invocation in each org; valid UpdateInvocation
  ACL changes succeed exactly when sharing is enabled, on both transports.
  GetInvocation and its ACL, GetEventLogChunk, `/file/{download,view}` for both
  `raw_json` and `buildlog`, and anonymous versus cookie reads must agree with
  the invocation ACL. Disabling new sharing does not delete an already public ACL.
* Executor ownership: released executors register with each org's local key;
  GetExecutionNodes must report exactly one worker and its `isDefault` policy on
  both transports. A real bare-runner action must execute on that org's worker
  when group-owned execution is enabled; without a shared pool, disabling it must
  return the explicit no-registered-executors denial. Worker identity and stdout
  are checked, and blocked groups must be denied before scheduling.
* Clean workflow restriction: a valid snapshot identity is invalidated using
  both the default developer credential and the web admin identity. The developer
  request must receive PermissionDenied exactly when admin restriction is enabled.
  This exercises the actual server authorization gate without Firecracker or KVM.
* Code search: a normal local in-memory namespace backend accepts idempotent
  DROP_NAMESPACE requests. The **released app** must forward a valid Index request
  over both transports exactly when CodeSearchEnabled permits it. The fixture
  verifies request namespace and forwarding counts, and injects no failures.
  This uses non-default `app.codesearch_backend`; full repository indexing/search
  and GitHub installation integration are outside this target.
* Suggestions: SuggestionPreference and BotSuggestionsEnabled are checked on
  both GetUser transports. These are frontend gates (suggestion card and AI fix
  button), not independent suggestion read RPCs. Browser rendering and downstream
  AI/workflow execution are not asserted.

Every sweep verifies app/proxy ByteStream bytes, CAS FindMissingBlobs,
BatchReadBlobs bytes/status, streamed GetTree structure and stream termination,
and ActionCache results, with both organization and personal credentials.
CAS bytes are written via proxy, AC and tree state via app; both sides read them
throughout the policy sequence. No org-isolation assumption is made about CAS
storage deduplication. Blocked proxy remote authentication may wrap the app's
Unavailable denial as Unauthenticated: only the explicit wrapped cause is accepted.

A SHA256-pinned released `bb` 5.0.492 binary runs in a disposable git repository.
`bb login --check` must return the modeled exit code for each personal credential.
A real PTY runs `bb ui` against the local app, checks the complete organization
picker, selects the modeled organization and waits for that org's build marker.
Child process environments are allowlisted; dispatch credentials are never read
or inherited by CLI commands.

Convergence is bounded at 20 seconds with 100ms polling. InvalidArgument,
Unimplemented, Internal and transport failures fail immediately. After the first
successful sweep, CLI checks and three further full sweeps spaced 400ms apart
must pass without retry. The test logs each observed convergence duration and
writes `group-convergence.json` beside fixture artifacts. Both auth caches are
configured with 1-second TTLs (production defaults are longer). Observed timings
include a complete sweep after the mutation acknowledgment, not just RPC latency.

Source inventory at v2.314.0:

| Surface / policy | Source traced |
| --- | --- |
| Group writes and reads, user/key/config/owner projections | `proto/{grp,user,api_key,bazel_config,invocation,eventlog}.proto`; `server/buildbuddy_server/buildbuddy_server.go`; `enterprise/server/backends/{userdb,authdb}`; `server/capabilities_filter`; `server/util/{claims,groupstatus}` |
| Actual settings and member UI calls | `enterprise/app/settings/settings.tsx`, `enterprise/app/api_keys/api_keys.tsx`, `enterprise/app/org/{org_members,org_join_requests,org_user_list}.tsx`, `app/docs/setup_code.tsx` |
| Sharing, logs and downloads | `server/backends/invocationdb/invocationdb.go`, `app/invocation/{invocation_share_button,invocation_raw_logs_card}.tsx`, `app/service/rpc_service.ts` |
| Executor default policy | `proto/scheduler.proto`; `enterprise/server/scheduling/scheduler_server/scheduler_server.go`; `enterprise/app/executors/executors.tsx` |
| Suggestions and AI fix visibility | `app/invocation/{invocation_suggestion_card,ai_fix_button}.tsx` |
| Workflow clean runs | `proto/workflow.proto`; `app/invocation/workflow_rerun_button.tsx`; `server/buildbuddy_server/buildbuddy_server.go`; `enterprise/server/remote_execution/snaploader` |
| Code-search UI gate and indexing gate | `app/router/router.tsx`; `enterprise/server/backends/codesearch/codesearch.go`; `proto/{index,codesearch_service}.proto` |
| CLI organization state | `cli/login/login.go`, `cli/ui/{ui,orgpicker,model,api}.go` |

Run only this area, using a shared runner daemon so the separately provisioned
executors can join the app fixture network:

```sh
bb remote --runner_exec_properties=init-dockerd=true test //tests/statemachine/groups/... --test_output=all
```

The harness's fallback starts separate private daemons for separate fixtures, so
plain runners cannot share their networks. No harness files are modified. A shared
harness option to reuse a fixture daemon would remove this runner requirement.

Pinned enterprise-v2.314.0 images:

* App: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
* Proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
* Executor: `gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8`
* CLI: GitHub release `buildbuddy-io/bazel` 5.0.492, linux-x86_64;
  SHA256 `e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`.

Validation:

* [Final full suite](https://app.buildbuddy.io/invocation/dca3d545-798d-46bd-936a-5dff662b627d):
  30 modeled steps and 90 subsequent stability sweeps passed in 108.32 seconds.
  Observed full-sweep convergence ranged from 94.033ms to 1.128481191s.
  UpdateGroup's maximum was 1.026861272s; SetGroupStatus's maximum was
  1.128481191s (recovery); creation/slug reuse completed within 173.612ms.
* [Independent expanded run](https://app.buildbuddy.io/invocation/58dbddc5-137b-401f-ae9c-99481451ced8):
  all 30 steps and stability sweeps passed, including blocked denial on every
  authenticated RPC covered here and actual executor routing. The final run
  additionally checks the HTTP old-slug error's exact code and message.

No confirmed product divergence was found. The approximately one-second
propagation of executor selection, personal-key policy and blocked/recovered API
access matches the configured auth-cache TTL; converged state did not regress.
All coverage remains enabled in the normal suite (no `bug` tags or skips).
