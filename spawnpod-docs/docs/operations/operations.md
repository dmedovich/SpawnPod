# Deployment and operations

**Status:** The local controller, Lease election, Forgejo Unix RPC and cluster
diagnostic commands are implemented. A direct K3s smoke and a two-step local
Forgejo workflow and targeted admission, quota and egress checks passed; see
[step 22](../implementation/22-local-live-validation.md) and
[step 23](../implementation/23-live-policy-and-reconciliation.md),
[step 24](../implementation/24-live-restart-recovery.md), and
[step 25](../implementation/25-live-service-isolation.md), and
[step 26](../implementation/26-live-service-failure-recovery.md), and
[step 27](../implementation/27-live-pod-and-service-loss.md), and
[step 28](../implementation/28-live-provision-crash.md), and
[step 29](../implementation/29-live-api-outage.md), and
[step 30](../implementation/30-live-delete-retry.md), and
[step 31](../implementation/31-lost-kubernetes-responses.md), and
[step 32](../implementation/32-official-runner-v13-validation.md), and
[step 33](../implementation/33-file-transfer-and-security.md), and
[step 34](../implementation/34-source-proxy.md), and
[step 35](../implementation/35-runner-https-source-and-systemd.md).
See [implementation step 07](../implementation/07-local-rpc-and-metadata.md)
and [ADR-0006](../decisions/0006-protocol-and-operations.md).

The candidate install, probes, metrics and shutdown behavior are documented
in [INSTALL](../../../docs/INSTALL.md) and [step 37](../implementation/37-release-candidate.md). Older status paragraphs below describe their original implementation steps.

## Target single-node layout

- K3s single node on the current development machine for the first live tests.
- `prod`: Forgejo application.
- `infra`: official Forgejo Runner and spawnPod control plane.
- `ci`: ephemeral job Pods and namespace security/quotas.
- Observability: VictoriaMetrics, VictoriaLogs and Grafana. Do not publish spawnPod administration or metrics endpoints to the public internet.

## Configuration

The implemented [example configuration](../../../deploy/config.example.yaml)
has Kubernetes namespace and Lease settings, an absolute controller socket
path, reconciliation and cleanup timeouts, an agent image, and approved
execution profiles. Each profile pins a job image digest, architecture,
numeric UID/GID and resource bounds. Validation rejects unknown YAML fields.
The controller uses an in-cluster ServiceAccount or an explicit kubeconfig.
The example now places its Lease in `infra` and workloads in `ci`.
An optional `network: source` profile requires `sourceProxy` with a pinned
IP and port, a separately operated source proxy and the generated source
NetworkPolicy. The local setup and live verification are in
[step 34](../implementation/34-source-proxy.md).
The local proxy's user systemd service, route maintenance and official Runner
HTTPS checkout result are documented in
[step 35](../implementation/35-runner-https-source-and-systemd.md).
Runner job secrets are passed as per-command environment over the exec stdin
stream; no secret value belongs in a ConfigMap or PodSpec.

Reference RBAC and network manifests are in `deploy/`, documented in
[step 16](../implementation/16-kubernetes-security-manifests.md). Generate the
admission policy from the active config as described in
[step 17](../implementation/17-generated-admission.md). The generated namespace
budget is documented in [step 18](../implementation/18-namespace-resource-quota.md).
Cluster capacity measurement, logging and metrics remain design work described
in ADR-0006. The example config is for local validation;
it is not a tested production deployment.

## CLI

Implemented commands:

```sh
go run ./cmd/spawnpod validate --config deploy/config.example.yaml
go run ./cmd/spawnpod render-admission --config deploy/config.example.yaml
go run ./cmd/spawnpod doctor --config deploy/config.example.yaml
go run ./cmd/spawnpod smoke --config .local/config.yaml
go run ./cmd/spawnpod environments list --config deploy/config.example.yaml
go run ./cmd/spawnpod environments inspect --config deploy/config.example.yaml ENVIRONMENT_ID
go run ./cmd/spawnpod environments cleanup --config deploy/config.example.yaml --dry-run
go run ./cmd/spawnpod serve --config deploy/config.example.yaml
go run ./cmd/spawnpod version
```

`serve` needs Kubernetes access and a directory writable by the controller UID
for `controller.socketPath`. The Runner must use the same UID to access the
mode `0600` Unix socket. The example socket endpoint is
`unix:///run/spawnpod/backend.sock`. The controller serves after acquiring
the Lease and completing startup reconciliation; it removes its socket when
leadership ends.

`doctor` needs an installed cluster, the generated admission manifests and the
reference security manifests. It uses the configured Kubernetes identity and
does not modify workloads. It checks controller preflight, the `ci` namespace's
pinned Restricted Pod Security labels, the tokenless job ServiceAccount,
admission expression type-checking status, observed quota limits and the
controller's RBAC permissions through SelfSubjectAccessReview. It prints every
result and exits nonzero on failure. It does not prove that the CNI enforces
NetworkPolicy, that admission rejects malformed Pods, or that Runner jobs run;
those require live cluster scenarios. See [step 19](../implementation/19-doctor.md).
With an admin kubeconfig on the local host, `doctor --as-controller` and
`serve --as-controller` impersonate the restricted controller ServiceAccount
to exercise its RBAC. `smoke` performs the first real plugin RPC lifecycle
after `serve` is ready; it creates and removes a job environment. Prepare its
images and config using `deploy/smoke/prepare-local.sh`. See
[step 21](../implementation/21-local-smoke-preparation.md).
The first live results and remaining scenarios are in
[step 22](../implementation/22-local-live-validation.md).

The `environments` commands read non-secret ConfigMap records and emit JSON.
The cleanup preview uses the same record decision function as controller
reconciliation. It makes no changes and cannot see an operation currently
active inside a running controller. See [step 20](../implementation/20-environment-diagnostics.md).

A manual destructive cleanup command is not exposed yet; the controller
removes expired and interrupted environments through reconciliation.

## Operational procedures to document and test

1. Verify RBAC, admission and namespace rules with server-side negative tests.
2. Install or upgrade while respecting active jobs and drain behavior.
3. Verify readiness, Kubernetes connectivity, policy enforcement and telemetry.
4. Diagnose Pending/ImagePullBackOff/OOMKilled/evictions and API errors.
5. Inspect and clean orphan resources with ownership checks and dry-run.
6. Rotate adapter and Kubernetes credentials safely.
7. Recover from interrupted jobs, restart or failed deletion.
8. Roll back compatible releases without losing ownership metadata.

## Metrics (proposed names)

`spawnpod_environments_active`, `spawnpod_environments_created_total`, `spawnpod_environments_failed_total`, `spawnpod_environment_duration_seconds`, `spawnpod_provision_duration_seconds`, `spawnpod_exec_duration_seconds`, `spawnpod_cleanup_failures_total`, `spawnpod_policy_denials_total`.

Avoid high-cardinality environment/job IDs as metric labels. Use structured logs for per-job correlation. Health endpoints should distinguish liveness from readiness.

## Performance targets

Safety caps are set in ADR-0006. Numeric latency/availability SLOs and the
usable concurrency on the target machine require measurements on single-node K3s. The
generated ResourceQuota bounds aggregate CI objects and configured compute
resources. Node capacity reservations must still protect `prod` and
observability from CI resource contention.
