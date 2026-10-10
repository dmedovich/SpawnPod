# spawnPod — Engineering Documentation

**Status:** core and gated service code implemented; local K3s RPC and Forgejo workflows passed on 2026-10-09, as did targeted admission, quota, egress, restart, approved service and service failure recovery checks. Broader failure and compatibility scenarios remain. **Language:** Go. **Initial integration:** Forgejo Runner v13.1.0 protocol. **Execution platform:** Kubernetes/K3s.

spawnPod is a Kubernetes-native execution engine for isolated, ephemeral CI workloads. The first integration is Forgejo Runner; its core is designed to be independent of any particular CI platform.

The scoped first release candidate is documented in [SPEC](../docs/SPEC.md), [INSTALL](../docs/INSTALL.md) and [RELEASE](../docs/RELEASE.md). See [step 37](docs/implementation/37-release-candidate.md) for packaging and operational evidence.

## Reading order

1. [Product requirements](docs/PRD.md) — what the product must do.
2. [Architecture](docs/architecture/ADD.md) — boundaries, components and data flow.
3. [Environment lifecycle](docs/architecture/lifecycle.md) — states, operations, failures and recovery.
4. [Forgejo integration](docs/integrations/forgejo.md) — observed upstream protocol and adapter design.
5. [Security](docs/security/security-model.md) — trust boundaries and enforcement.
6. [Operations](docs/operations/operations.md) — configuration, deployment and monitoring.
7. [Decisions](docs/decisions/README.md) — accepted design choices and required proof before release.
8. [Implementation journal](docs/implementation/01-supervisor.md) — code steps, [first live K3s and Forgejo validation](docs/implementation/22-local-live-validation.md), [policy checks and reconciliation fix](docs/implementation/23-live-policy-and-reconciliation.md), [restart recovery](docs/implementation/24-live-restart-recovery.md), [service isolation](docs/implementation/25-live-service-isolation.md), [service failure recovery](docs/implementation/26-live-service-failure-recovery.md), [Pod and service loss](docs/implementation/27-live-pod-and-service-loss.md), [provisioning crash recovery](docs/implementation/28-live-provision-crash.md), [API outage](docs/implementation/29-live-api-outage.md), [deletion retry](docs/implementation/30-live-delete-retry.md), [lost API responses](docs/implementation/31-lost-kubernetes-responses.md), [official Runner validation](docs/implementation/32-official-runner-v13-validation.md), [file transfer and security checks](docs/implementation/33-file-transfer-and-security.md), [source proxy validation](docs/implementation/34-source-proxy.md), [Runner HTTPS checkout](docs/implementation/35-runner-https-source-and-systemd.md), and [Node action and private HTTPS checkout](docs/implementation/36-actions-checkout-private-https.md).

## Documentation rules

- **Accepted** = current project decision, not proof of implementation.
- **Proposed** = target design subject to revision.
- **Verified upstream** = supported by the uploaded `runner-main.zip`, specifically `runner/act/plugin/proto/v1alpha/plugin.proto`.
- **Verification gate** = implementation or cluster behavior that must be tested before a compatibility or security claim.
- Product requirements are not claims that spawnPod already implements the feature.

## Repository layout (target)

`cmd/spawnpod`, `internal/adapter/forgejo`, `internal/engine`, `internal/policy`, `internal/profile`, `internal/provider/kubernetes`, `internal/reconcile`, `internal/telemetry`, `deploy/`, `docs/`, `tests/`.

Directories should be created when needed rather than as empty scaffolding.
