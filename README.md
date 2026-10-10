# SpawnPod

SpawnPod is a Kubernetes execution backend for **Forgejo Runner v13.1.0**.
It provisions bounded non-root job environments, supervises commands, streams
results, transfers workspace files and reconciles cleanup after interruptions.
Forgejo Runner controls workflow execution; SpawnPod controls the permitted
execution environment.

**Current version: v0.1.0-rc.1 — Linux/amd64 release candidate.** Local K3s
and official-Runner integration tests passed, including private HTTPS
`actions/checkout@v4`. Production hardware capacity, real ARM64 execution and
future-version upgrade compatibility remain separate gates.

- [Release specification](docs/SPEC.md): shipped contract and limitations.
- [Installation and operations](docs/INSTALL.md): binaries, images, manifests,
  credentials, verification, upgrades and rollback.
- [Release notes](docs/releases/v0.1.0-rc.1.md) and [release checklist](docs/RELEASE.md).
- [Architecture and implementation evidence](spawnpod-docs/README.md): full
  target PRD, decisions and dated live-test journal. The PRD is broader than
  this candidate's supported scope.

## What is included

- Strict execution profiles with pinned images and resource/deadline caps.
- Reusable per-environment workspaces, command output/exit status, cancellation
  and bounded rooted tar transfer.
- Durable ConfigMap records, UID-scoped resource ownership, Lease election
  and reconciliation after controller/API/resource failures.
- Generated admission, namespace RBAC/quotas and default-deny policies;
  gated approved service containers and explicit HTTP(S) source egress.
- A private Unix gRPC adapter, readiness/liveness, low-cardinality metrics,
  bounded RPC drain, diagnostic commands and installation renderer.
- Versioned static binaries, a scratch runtime image recipe, checksums and
  dependency inventory. The official Runner is installed separately.

The job image supplies its own shell/tools. Privileged Docker-in-Docker,
unsafe mounts, arbitrary images/users and unrestricted internet access are
unsupported. Job Pods share the host kernel; see [security assumptions](docs/SPEC.md#security-assumptions).

## Local build

Source minimum: Go 1.25 or newer; the candidate is built/tested with Go 1.27.1.

```sh
make check
./scripts/release.sh
./scripts/release-image.sh
```

The release builder requires committed sources. For a local preview only,
use `SPAWNPOD_ALLOW_DIRTY=1 ./scripts/release.sh`; provenance then records
`dirty: true`. Image export uses Podman. Neither script publishes or deploys.
GitHub's tag-triggered workflow runs checks and publishes candidate artifacts.

```sh
go run ./cmd/spawnpod validate --config deploy/config.example.yaml
go run ./cmd/spawnpod render-admission --config deploy/config.example.yaml
```

The example contains placeholder image digests and budgets. Replace them
with approved images and measured limits before installation. Follow
[INSTALL.md](docs/INSTALL.md) for the operator-created Runner Secret and
in-cluster deployment. The controller's Kubernetes token is mounted only in
the controller container; Runner and job Pods receive no Kubernetes token.

## Code map

| Path | Role |
|---|---|
| `cmd/spawnpod` | Controller, configuration/rendering, diagnostics and smoke CLI |
| `cmd/spawnpod-agent`, `internal/supervisor` | In-Pod process supervision and cancellation |
| `internal/workspace` | Rooted, bounded file transfer |
| `internal/engine` | Lifecycle, ordering, durable state and recovery |
| `internal/provider/kubernetes` | Job/service resources, exec transport and ConfigMap store |
| `internal/adapter/forgejo` | Pinned protocol, validation and RPC mapping |
| `internal/config`, `internal/admission` | Operator policy and generated admission/quotas |
| `internal/controller`, `internal/transport/unixrpc` | Preflight, Lease, reconciliation and private serving |
| `internal/telemetry`, `internal/deployment` | Probes/metrics and installation rendering |
| `cmd/spawnpod-source-proxy`, `internal/sourceproxy` | Explicit HTTP(S) routes and TLS SNI validation |
| `deploy/smoke` | Development-only fixtures and live verification scenarios |

Licensed under [Apache-2.0](LICENSE). The copied Forgejo plugin schema and
generated types retain their upstream MIT notice.
