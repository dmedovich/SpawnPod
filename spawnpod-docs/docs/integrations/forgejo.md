# Forgejo Runner adapter

**Evidence:** local source tree `/home/dmedovich/runner-main/runner`, inspected 2026-10-07: `act/plugin/proto/v1alpha/plugin.proto`, `act/plugin/adapter.go`, `act/plugin/client.go`, `act/runner/run_context.go`; plus the [Forgejo v13.1.0 plugin announcement](https://forgejo.org/2026-09-runner-release-v131/) and [published Go API](https://pkg.go.dev/code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha). The archive has no `.git` metadata and differs from the official v13.1.0 tag. The tagged source and local integration results are recorded in [step 32](../implementation/32-official-runner-v13-validation.md).

## Verified upstream contract

The protobuf package is `plugin.v1alpha`, Go package path `code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha`. The gRPC service is `BackendPlugin`; the runner connects to a standalone plugin server. Plugin servers must also expose standard `grpc.health.v1`.

| RPC | Upstream semantics |
|---|---|
| `Capabilities` | Backend identity; currently returns a `name` |
| `Create` | Provisions environment; image, name, cap_add/drop, service containers, backend options, label arg, environment timeout |
| `Start` | Starts environment, streams output; completion includes image env |
| `Exec` | Executes argv with env/user/workdir, streams stdout/stderr, final exit code or execution failure |
| `CopyIn` | Client-streamed tar archive, first chunk specifies environment ID and destination |
| `CopyOut` | Server-streamed tar archive |
| `Remove` | Destroys environment and resources |

`CreateResponse` includes `environment_id`, `root_path`, `act_path`, `tool_cache_path`, `temp_path`, and optional OS/PATH metadata. Service container message includes name/image/env/ports. `Exec` request has **no stdin field in this observed contract**; generic spawnPod stdin support must not be presented as Forgejo plugin capability without an additional mechanism.

## Verified client behavior in this source tree

- `act/runner/run_context.go` passes `container.image` as `CreateRequest.image` and the plugin-label suffix as `label_arg`. When no job-level container image is set, `image` is empty; spawnPod must resolve an approved default image from `label_arg` or runner configuration. The workflow image takes precedence only after both values are validated.
- The `CreateRequest.name` comment calls it unique per job, but `jobContainerName()` derives it from workflow name and job name, without a run ID in the inspected code. Treat it as a human-readable handle, **not** a globally unique idempotency key. The adapter here calls `Create` once and does not itself retry that RPC; a lost response still needs orphan reconciliation.
- `Start` must send `StartComplete`; ending its stream without this message is an error. `image_env` may be empty. `Exec` must send `ExecComplete` or `ExecFailed`; ending without one is an error. `ExecFailed.error_message` **must be non-empty** for this client: `adapter.go` otherwise returns success when `exit_code` is zero.
- The client builds each `ExecRequest.env` by layering runner variables, container-level variables and command variables in that order. It passes explicit `user` and `workdir`. `CopyIn` sends a first metadata chunk even for an empty archive; subsequent chunks contain data only. `CopyOut` streams tar bytes until EOF. `Remove` uses a fresh cleanup context after job cancellation.
- `pluginEnvironment.SupportsDockerContainerActions()` returns false, so Docker container actions fail before reaching spawnPod. `ManagesOwnNetworking()` returns true. `IsHealthy()` is a no-op for the environment; the runner does not poll job Pod health through this protocol.
- `client.go` supports Unix sockets and checks gRPC health before `Capabilities`. The inspected runner configuration currently calls `NewClient(..., WithAllowPlainTCP())` and has a TODO for wiring TLS material; remote mutual-TLS deployment is a future runner integration, not a current compatibility claim. The default gRPC per-message limit here is 32 MiB.

Upstream explicitly says the plugin API is **alpha** and can change incompatibly between releases. It relies on gRPC `NotFound`, `InvalidArgument` and `Unavailable` statuses. There are currently no structured error details. Upstream warns that after cancelling streaming RPCs, `Remove` may overlap with teardown of the old handler and recommends per-environment locking.

## Adapter mapping (core methods implemented locally)

- `Capabilities` → report backend name; the observed protocol has no feature-negotiation fields. Implemented.
- `Create` → validate options/profile, provision Kubernetes-backed environment and return paths. Local workflows on the tagged Runner passed sequential job steps and approved service startup.
  Service requests map to approved profile entries only when `controller.enableServices`
  is set. The example config keeps this gate off until network and cluster tests pass.
- `Start` → wait for readiness, check Pod architecture and send effective
  agent environment in `StartComplete`. Implemented locally; exact image ENV
  equivalence remains an integration test.
- `Exec` → invoke execution engine and translate stdout/stderr/exit/failure to protocol stream. Implemented in process.
- `CopyIn` / `CopyOut` → validated tar streaming. The direct local K3s smoke
  passed both transfers through real `pods/exec`.
- `Remove` → terminate execution and remove owned resources idempotently. Implemented in process.

The adapter is registered on a private Unix socket while the controller holds
the Lease. Five workflows on the tagged Runner passed their expected outcomes
on 2026-10-10; broader compatibility remains unverified. The
[adapter step](../implementation/05-forgejo-adapter.md) and
[transfer step](../implementation/06-file-transfer.md) and
[local RPC step](../implementation/07-local-rpc-and-metadata.md) record code behavior
and remaining compatibility work. [Step 22](../implementation/22-local-live-validation.md)
records the live result and workdir requirement for workflows without checkout.

## Compatibility boundaries and verification

- The current observed protobuf does not offer arbitrary `Inspect`, `List`, `Stop` or stdin RPCs; those are **internal product concepts**, not Forgejo plugin methods.
- Pin initial support to Runner v13.1.0. [Step 32](../implementation/32-official-runner-v13-validation.md) records the tagged-source build and initial local workflows; [step 33](../implementation/33-file-transfer-and-security.md) verifies Runner file transfer and the default working directory. The archive name alone does not establish a release tag.
- The runner connects to spawnPod over a local Unix-domain socket with filesystem permissions. A remote runner requires mutual TLS and identity mapping before enablement.
- Plugin options select only operator-approved profiles. Runner label images and workflow `container.image` overrides are both validated against the image allowlist. Unsupported Docker-like options fail explicitly.
- Lost `Create` responses cannot be guaranteed exactly-once without a stable upstream idempotency key. Interrupted `Exec` streams are never replayed. See [ADR-0003](../decisions/0003-execution-contract.md) and [ADR-0006](../decisions/0006-protocol-and-operations.md).
