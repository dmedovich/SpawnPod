# Security model

**Status:** Accepted target controls; must be validated by negative tests and cluster configuration. See [ADR-0005](../decisions/0005-security-and-network.md).

## Threat model

Adversaries may control workflow commands, image references, environment variables, service images and selected backend options. A compromised job must not be able to read cluster credentials, other job workspaces, host files or spawnPod control-plane secrets. Treat adapter inputs as untrusted even when originating from an authenticated runner.

## Trust boundaries

1. CI platform → runner → adapter: authentication and input validation.
2. Adapter → engine: convert protocol-specific data to validated domain types.
3. Engine → Kubernetes provider: policy-approved immutable spec only.
4. Kubernetes API → execution Pod: admission and namespace restrictions.
5. Execution Pod → network/registry: explicit egress controls and credential scope.

## Mandatory defenses

- Dedicated `ci` namespace; `ResourceQuota`, `LimitRange`, default-deny NetworkPolicies.
- spawnPod ServiceAccount with minimal namespace RBAC, not cluster-admin; separate runner identity without K8s credentials.
- No automatic ServiceAccount token in job Pods; deny arbitrary SA references, secret mounts and hostPath.
- Pod Security Restricted where compatible; no privileged, host namespaces, privilege escalation or added dangerous capabilities.
- Enforce requests/limits, ephemeral storage and max duration; do not rely on app-side validation alone.
- Administrator allowlisted image digests and profile-fixed non-root UID/GID; incompatible images fail validation/readiness.
- Reject unsafe tar paths, symlink escapes, excessive archive expansion and attempts to write outside workspace.
- Secret redaction and audit hygiene; never expose secret values in metrics or metadata.

## Network profiles

`isolated`, `source` and `internet` are the chosen egress profiles; per-environment service access is an additional permission. Default-deny applies to job and service Pods. `source` reaches only a controlled egress proxy for approved source hosts, plus CoreDNS where required; `internet` uses an explicit gateway that blocks internal destinations. Standard NetworkPolicy uses IP/namespace/pod selectors, **not DNS hostnames**. A profile remains unavailable until its gateway, K3s NetworkPolicy enforcement and negative egress tests pass.

## Limitations

Pod isolation is not a strong boundary for hostile mutually untrusted tenants sharing a kernel. Stronger isolation requires sandboxed runtimes or separate nodes. A job that legitimately receives a secret can still exfiltrate it if outbound connectivity allows it. A role that creates Pods may indirectly access namespace capabilities unless admission prevents it. The in-container supervisor is also not a security boundary against job code.

## Security acceptance tests

Attempt privileged Pod, hostPath, hostNetwork, arbitrary SA/secret reference, privilege escalation, CPU/memory limit bypass, disallowed egress, cross-job workspace read, tar traversal, expired environment execution and unauthorized profile selection. All must be denied or documented as a consciously accepted limitation.
