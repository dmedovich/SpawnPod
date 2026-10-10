# ADR-0005 — Policy, credentials and network boundaries

**Status:** Accepted (design direction) · **Date:** 2026-10-07

## Decision

- The `ci` namespace enforces Pod Security **Restricted** at a pinned Kubernetes version. A separate admission policy restricts spawnPod-created Pods to approved image digests, fixed security contexts, resource ceilings, allowed volume types and no service-account tokens, secret mounts, host namespaces, hostPath, privileged containers or added capabilities. Admission is mandatory because namespace-scoped Pod create RBAC alone does not constrain PodSpec contents.
- The controller ServiceAccount lives in `infra`; its Role in `ci` grants only the Pod, `pods/exec`, Service, NetworkPolicy and ConfigMap operations the implementation needs. Lease coordination uses a separate named Lease Role in `infra`; PVC permissions are absent until PVC profiles exist. The runner receives no Kubernetes credentials. Job and service Pods set `automountServiceAccountToken: false` and use a dedicated, permissionless ServiceAccount. Preflight verifies the actual rendered RBAC and admission rules with negative cases.
- Only the runner can reach the adapter endpoint. In the initial co-located deployment, use a Unix-domain gRPC socket shared with the runner, owned by the controller UID and a dedicated runner group, mode `0660`; the socket directory is not mounted into job Pods. Remote runner support requires mutually authenticated TLS and an explicit identity mapping before enablement. A runner connection establishes an integration identity, **not** a trustworthy repository identity: the observed Forgejo plugin request has no cryptographic repository claim. Per-repository concurrency/policy is unavailable until a verified identity is carried by the protocol.
- Forgejo supplies job secret values as per-command environment variables where its protocol supports them. They are sent via the Kubernetes exec stdin stream and held only in process memory, then exposed to the child process as requested. They are never placed in PodSpec, ConfigMaps, command argv, logs or metrics. No implicit Kubernetes Secret mount is supported. Registry publishing credentials may be supplied by the runner to a command, or through a separate operator-approved short-lived secret facility after design/testing; the initial profile uses the runner path. A job that receives a secret can read and exfiltrate it within its permitted network profile.
- NetworkPolicy starts with default-deny ingress and egress for job/service Pods. `isolated` permits only required service Pod traffic; service aliases come from `/etc/hosts`, so DNS is unnecessary. `source` permits service traffic, DNS only to CoreDNS when required, and TCP only to an operator-managed egress proxy for Forgejo/source hosts. The proxy enforces destination hostname and blocks private/metadata/control-plane destinations. This profile supports proxy-aware HTTPS checkout; SSH checkout requires a separately approved route. `internet` is an explicit administrator profile with broader egress through a gateway that blocks cluster, node, metadata and private administrative networks. No FQDN allowlist is claimed from standard NetworkPolicy. A profile is unavailable if its proxy/gateway and enforcement tests fail.
- Service Pods accept traffic only from the matching environment job Pod; job Pods can reach only matching service Pods plus their selected egress path. Labels include an opaque environment ID. Since NetworkPolicy selectors can be changed by a party with Pod creation rights, admission prevents workflow-controlled labels and Pod creation. Egress policy enforcement by the installed K3s network controller must be tested, including Service IP translation and node/host endpoints. A policy object existing is not proof of enforcement.
- Profiles are administrator-owned, versioned, immutable snapshots at Create time. Runner options may select a named profile from an allowlist tied to the authenticated runner; any option that broadens privilege/resources/network is rejected. Default profile is non-root, no added capabilities, read-only root filesystem where the approved image allows, bounded CPU/memory/storage and `source` egress only when source checkout needs it. More permissive profiles require explicit operator configuration.

## Alternatives

Plain NetworkPolicy hostname rules; trusting repository names in label args; passing secrets through Pod environment; giving the runner Kubernetes access. These do not meet the stated trust model.

## Consequences

The proxy/gateway is a separately operated security component and a deployment prerequisite for `source`/`internet`. K3s includes a network-policy controller by default, but it can be disabled or replaced; deployment preflight must verify enforcement in the actual cluster. Pods do not provide strong isolation from hostile tenants on a shared kernel. Strong multi-tenant use requires sandboxed runtime classes or dedicated nodes and new acceptance tests.

## Sources

- [Kubernetes Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
- [Kubernetes NetworkPolicy behavior and limitations](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
- [Kubernetes RBAC resource-name limits](https://kubernetes.io/docs/reference/access-authn-authz/rbac/)
- [K3s networking services](https://docs.k3s.io/networking/networking-services)
