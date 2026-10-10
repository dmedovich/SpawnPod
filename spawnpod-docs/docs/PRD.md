# Product Requirements Document — spawnPod

**Status:** Draft 0.1 · **Date:** 2026-10-07 · **Scope:** full target product (not an MVP plan)

## 1. Product definition

spawnPod is a Kubernetes-native execution engine that provides isolated, ephemeral execution environments to CI/CD runners. It validates requests against administrator-defined policies, provisions Kubernetes resources, executes commands, transfers files, streams output, handles cancellation and failures, and cleans up resources.

**Principle:** CI decides **what** to execute; spawnPod controls **how and where** execution is permitted.

### Goals

- Execute CI workloads using Kubernetes instead of requiring privileged Docker-in-Docker.
- Keep execution jobs separate from the runner and Kubernetes control plane.
- Support a Forgejo Runner integration and a provider-independent internal engine.
- Enforce least privilege, resource budgets, network restrictions and bounded lifetimes.
- Make jobs observable, cancellable, recoverable where technically possible, and cleanable after failure.
- Run on a small single-node K3s without requiring an external database, while permitting multi-node deployment.

### Non-goals

Workflow YAML parsing, Git event processing, CI job scheduling at the pipeline level, CI web UI, repository management, replacing Forgejo Runner, building an OCI image builder from scratch, or promising drop-in compatibility with every CI platform.

## 2. Users and environments

- **Platform operator:** installs spawnPod, defines profiles/policies, diagnoses failures and manages upgrades.
- **CI integrator:** develops an adapter translating a runner's execution protocol.
- **Workflow author:** selects permitted execution labels/profiles; cannot change cluster policy.
- **CI job:** untrusted workload; must not receive spawnPod's Kubernetes credentials.

Initial deployment target: Forgejo in `prod`, Forgejo Runner and spawnPod in `infra`, execution Pods in a dedicated `ci` namespace on `shusha` K3s. No public control-plane exposure.

## 3. Functional requirements

### FR-01 Environment lifecycle

Each environment has an opaque ID, integration/source identity, ownership metadata, profile, timestamps, deadline, state and Kubernetes resource references. Support Create, Start, Inspect, Exec, CopyIn, CopyOut, Stop/Cancel, Remove, List and capability discovery internally, with adapter-specific mappings. Create/Remove must be safely repeatable where feasible. Do not equate command completion with environment removal: subsequent steps reuse workspace.

### FR-02 Command execution

Support argv, working directory, per-command environment, optional user subject to policy, streamed stdout/stderr, exit status, command timeout and cancellation. Distinguish user command failure from infrastructure failure. Design explicit process termination; a cancelled gRPC stream does not by itself guarantee the process has stopped.

### FR-03 Workspace

Persist workspace across sequential steps within one environment. Support `emptyDir` by default, size limits, optionally PVC-backed workspaces, tar-based file transfer, preservation of necessary file metadata, traversal/symlink protection, and cleanup. Cross-job artifact/cache sharing must be explicit and isolated.

### FR-04 Service containers

Support temporary dependent services, image/env/port configuration, readiness handling, name resolution/connectivity and coordinated cleanup. The accepted design uses separate service Pods and ClusterIP Services, with aliases in the job Pod; its supported port semantics are specified in [ADR-0004](decisions/0004-workload-topology-and-compatibility.md).

### FR-05 Execution profiles

Administrator-owned profiles define CPU/memory/storage requests and limits, max lifetime, concurrency, allowed images, security restrictions, network access, service support and permitted execution options. Workflow input may select only authorized profiles. Reject unsafe requests rather than silently relaxing policy.

### FR-06 Resource scheduling

Limit concurrent environments globally and by integration/repository/profile where identity is available. Let Kubernetes schedule Pods; bound Pending and image pull waits; report FailedScheduling, OOMKilled, Evicted and storage failures distinctly.

### FR-07 Container image builds

Support running compatible rootless OCI build tools and registry publishing under policy, without requiring privileged DinD. spawnPod itself does not implement an image builder.

### FR-08 Integrations

Provide Forgejo adapter for the actual `plugin.v1alpha` contract, with version compatibility tracking. Other CI systems require independently researched adapters and capability negotiation; no blanket compatibility claim.

### FR-09 Recovery and reconciliation

Track resource ownership with labels/annotations and stable IDs. Reconcile orphans, deadlines and deletion failures after restart. Distinguish reconstructing environment metadata from resuming an interrupted process stream. Avoid deleting resources not owned by spawnPod.

### FR-10 Operations and diagnostics

Provide declarative validated configuration, secure credential injection, CLI for serve/validate/doctor/list/inspect/cleanup/version (names proposed), structured logs, metrics, health/readiness, optional OpenTelemetry tracing and safe administrative dry-run.

### FR-11 Packaging and upgrades

Publish Go binary, OCI image, Kubernetes manifests and Helm chart. Support graceful shutdown, drain, documented rollback and version compatibility. Multi-replica operation requires explicit ownership coordination; single-replica operation must not depend on HA services.

## 4. Security requirements

- Namespace-scoped Kubernetes RBAC, no cluster-admin for spawnPod, no Kubernetes API credentials for runner or CI job Pods.
- Pod Security admission, no privileged/host namespace/hostPath for standard profiles, no privilege escalation, restrictive capabilities, seccomp and non-root when compatible.
- Default-deny network access, explicitly granted network profiles. Standard Kubernetes NetworkPolicy does **not** provide FQDN allowlists.
- Secret minimization, no secrets in Pod metadata/logs/metric labels, controlled injection, log redaction as defense in depth (not prevention of exfiltration).
- Protect file extraction against traversal and symlink attacks. Treat all images, arguments and service settings as untrusted.
- Explicit tenant boundaries and isolation limitations; Pods alone are not equivalent to hard multi-tenant isolation.

See [security model](security/security-model.md).

## 5. Nonfunctional requirements

Go; Linux amd64/arm64; Kubernetes/K3s; no mandatory Docker daemon or external database; low idle footprint; bounded retries/timeouts; stable observability; unit/integration/E2E testing; documented interfaces and supported versions. Numeric SLOs are **open pending measurement** on representative clusters.

## 6. Acceptance criteria

- Full end-to-end execution of sequential steps, streaming, exit codes, file transfer, service dependencies and cleanup.
- Cancellation actually terminates execution; deadlines cannot leave indefinitely running workloads without observable failure.
- Policy and Kubernetes admission reject privilege escalation, unsafe mounts, excessive resources and unauthorized networks.
- Crash/restart, API outages, repeated requests, scheduling failures and deletion failures covered by E2E tests.
- No unbounded orphan resources; ownership and reconciliation are auditable.
- Operators can install, upgrade, diagnose and roll back from documentation.
- Forgejo protocol compatibility verified against selected runner releases.

## 7. Design decisions and verification gates

Execution supervision, workload topology, state/recovery, controller coordination, image compatibility, secret transport and egress architecture are decided in [ADR-0003](decisions/0003-execution-contract.md) through [ADR-0006](decisions/0006-protocol-and-operations.md). The initial deployment is single-replica; multi-replica execution requires a separate release gate. Numeric SLOs and the exact capacity of `shusha` must be measured; they cannot be established by design alone.
