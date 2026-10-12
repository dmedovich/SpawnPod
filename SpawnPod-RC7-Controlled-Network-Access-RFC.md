# RFC: SpawnPod v0.1.0-rc.7 — Controlled Network Access

- **Status:** Proposed
- **Target:** `v0.1.0-rc.7`
- **Scope:** Source Proxy packaging, deployment, configuration, security validation, release gates
- **Compatibility baseline:** `v0.1.0-rc.6`
- **Owner:** SpawnPod maintainers

## 1. Summary

RC7 turns SpawnPod's existing `source` networking capability into an operationally complete, independently deployable and security-tested feature. It must **not** introduce general internet egress, privileged build pods, Docker socket access, Kubernetes credentials in jobs, or an embedded container builder.

The existing code already contains:

- `internal/sourceproxy/proxy.go`: operator-configured forward proxy with exact `(host, port)` routes pinned to literal destination IPs; HTTP forwarding and HTTPS `CONNECT` with TLS ClientHello SNI validation.
- `cmd/spawnpod-source-proxy/main.go`: a separate executable reading a strict YAML configuration and listening on a configured TCP address.
- `internal/provider/kubernetes/network_policy.go`: `BuildSourceProxyPolicy`, selecting managed `source` job pods and allowing TCP egress only to one proxy IP/port.
- `internal/adapter/forgejo/server.go`: injects `HTTP_PROXY`, `http_proxy`, `HTTPS_PROXY`, `https_proxy` into execution environments for `source` profiles and clears `NO_PROXY`/`no_proxy`.
- `internal/config/config.go`: requires an HTTP URL with literal unicast IP and port for `sourceProxy` when a profile uses `network: source`.
- Admission validation recognizes the `isolated` and `source` labels for jobs.

**Not yet established from reviewed sources:** whether release artifacts already contain the proxy binary; whether a standalone proxy image is built; exact runner-label-to-profile mapping; the complete install renderer and upgrade mechanics; and whether the cluster CNI enforces the proposed Service-VIP policy as intended. RC7 work must inspect and verify these rather than assume absence or correctness.

## 2. Motivation and concrete use case

The current deployment runs Forgejo Runner v13.1.0 with SpawnPod on a single-node Fedora 44 / K3s v1.36.4 cluster. CI jobs run in namespace `ci` under Pod Security `restricted` and `spawnpod-default-deny` (Ingress + Egress). The current default execution profile is effectively `isolated`.

An observed CI job executed successfully but `wget https://git.dmedovich.com/` failed with `bad address`. A control pod in namespace `default` resolved `git.dmedovich.com` to `138.124.231.231`; the host fetched it successfully with HTTP 200. Thus the observed failure is consistent with the deliberate `ci` egress restriction, not a general CoreDNS outage. The initial workflow suppressed the command failure with `|| true`, so its success did not prove network connectivity.

Desired workflow, **after** RC7: source acquisition → unprivileged Hugo/static-site build → publish OCI image to Forgejo Registry → update GitOps repository → Argo CD reconciliation. RC7 provides only controlled network transport, not the image builder or GitOps automation.

## 3. Goals

1. Deliver a standalone, reproducibly built, versioned, digest-pinnable `spawnpod-source-proxy` runtime image, or explicitly document and test an existing equivalent if already present.
2. Supply reviewed Kubernetes deployment assets and an operator procedure for the proxy.
3. Preserve namespace-wide default-deny and provide exactly scoped egress for `source` jobs; `isolated` jobs must remain disconnected.
4. Preserve existing job and controller privilege boundaries, with no service account token mounted into the proxy.
5. Provide end-to-end evidence that allowlisted HTTPS traffic works and unapproved destinations remain blocked.
6. Make upgrades and rollback from RC6 predictable, including config validation, policy installation ordering and verification.
7. Keep source-proxy configuration operator-owned, auditable, and independent of untrusted workflow input.

## 4. Non-goals

- No unrestricted outbound access, DNS egress exception for job pods, or namespace-wide `allow egress`.
- No HTTP method filtering inside TLS tunnels. The proxy checks destination and SNI, **not** application-level authorization; write operations to an allowed HTTPS origin are possible.
- No Docker-in-Docker, host Docker/containerd socket, privileged jobs, cluster credentials in CI, or integrated image building.
- No dynamic route additions requested by workflow YAML, jobs, or repository content.
- No generalized transparent proxy, service mesh, or multi-tenant policy engine.
- No change to the semantics of the existing `isolated` profile.

## 5. Existing implementation: evidence and implications

### 5.1 Route validation

`sourceproxy.New` accepts 1–64 routes. Each route includes `host`, `port`, `address`, and optional `allowLoopback`. Hostnames must use lowercase DNS-label syntax; mixed-case input is rejected rather than normalized. Duplicate host:port entries are rejected. Destination addresses are parsed as literal IPs and constrained to approved address categories. Private and link-local addresses are rejected by the reviewed `publicAddress` function; loopback requires explicit `allowLoopback`.

**Implication:** the example `git.dmedovich.com:443 → 138.124.231.231` matches the public-address model, but pinning to an IP creates operational responsibility for endpoint changes. A proxy must not silently fall back to DNS or arbitrary redirects outside the allowlist.

### 5.2 HTTPS and proxy semantics

`CONNECT` verifies the requested authority is allowlisted and inspects a complete initial TLS ClientHello for matching SNI before forwarding bytes. It does not decrypt HTTPS. Ordinary HTTP proxy requests must use an absolute `http://` URL and match an allowlisted host and port; port 80 is the default when omitted.

**Implication:** OCI Registry operations can traverse an approved HTTPS tunnel if the client honors proxy environment variables. This is **not** proof that any particular builder/registry client supports the setup; test each selected client. Any token supplied to the job may authorize writes to the allowed host.

### 5.3 Execution profile

`internal/adapter/forgejo/server.go` injects proxy variables into the command environment when the resolved profile has `Network == "source"`. It clears both `NO_PROXY` variants. The existing Forgejo Runner label `spawnpod` is known to execute jobs, but the precise mechanism to select a second named profile requires code inspection and a live test.

### 5.4 NetworkPolicy

`BuildSourceProxyPolicy` selects pods with `spawnpod.dev/managed=true` and `spawnpod.dev/network=source`, allowing TCP to one literal proxy IP and port. The `spawnpod-default-deny` policy continues to select all pods in `ci`. NetworkPolicies are additive: startup validation of these required policies does not prove the absence of additional broad allow rules; acceptance must inspect the complete policy set and test actual packets.

**Implication:** deploy the proxy at a stable, operator-pinned reachable endpoint. A Kubernetes Service ClusterIP is a candidate, but policy evaluation and DNAT ordering vary by CNI; test enforcement on the actual K3s networking stack before adopting it as a security invariant.

## 6. Proposed architecture

```text
Forgejo / official Runner
          |
          | existing SpawnPod adapter / exec
          v
  +----------------------------+
  | ci namespace               |
  | default-deny everywhere    |
  |                            |
  | isolated job  --X--> egress|
  | source job                 |
  |    |                       |
  +----|-----------------------+
       | TCP only to pinned proxy endpoint:3128
       v
  +----------------------------+
  | infra namespace            |
  | spawnpod-source-proxy      |
  | no Kubernetes API token    |
  | operator-owned routes      |
  +----------------------------+
       | exact host:port + pinned IP; TLS SNI checked
       v
  git.dmedovich.com:443 (Forgejo + Registry)
```

The proxy's own egress and ingress restrictions are **separate** from the job-side NetworkPolicy. If the proxy runs in `infra`, review and deploy ingress from selected `ci` job pods and egress to the pinned upstream IP/port, subject to the actual cluster CNI and namespace selectors. Do not assume the `ci` policy alone restricts the proxy.

## 7. Deliverables and implementation plan

### 7.1 Release image and binary

- Inspect `scripts/release.sh`, `deploy/release/runtime.Dockerfile`, `deploy/release/multiarch.Dockerfile` and the RC6 release manifest before changing them.
- Ensure `cmd/spawnpod-source-proxy` is built with the same reproducibility and version conventions as other release binaries.
- Publish a minimal, non-root proxy image with immutable digest and platform metadata; prefer no shell/package manager in runtime when practical.
- Use read-only root filesystem, dropped Linux capabilities, `allowPrivilegeEscalation: false`, seccomp `RuntimeDefault`, no host networking or host mounts.
- Provide artifact provenance/checksums consistent with the project's existing release process. Do not invent a second signing mechanism if the project already has one.
- Confirm container image command, flags, and mounted config path work in a live pod.

**Acceptance:** the published image starts with `--config /etc/spawnpod-source-proxy/config.yaml`, serves approved proxy requests, and can be pinned by digest on the target architecture.

### 7.2 Kubernetes assets

Add documented sample resources (exact repository path to follow existing deployment conventions):

- `ConfigMap` containing operator-owned `listen` and `routes`.
- `Deployment` with one replica initially, pinned proxy image digest, `runAsNonRoot`, read-only root FS, capabilities dropped, `automountServiceAccountToken: false`, no privilege escalation, CPU/memory requests and limits, readiness/liveness probes appropriate to the actual binary.
- Stable `ClusterIP` Service or alternative pinned endpoint, chosen only after connectivity and CNI-policy verification.

**Local implementation finding (2026-10-12):** K3s v1.34.12 evaluates job egress
after Service DNAT: an exact ClusterIP IPBlock rejects the approved connection.
RC7 therefore adds explicit `sourceProxyPolicy: pod-selector` for this deployment.
It allows only the configured TCP port to Pods carrying both fixed proxy labels
(`app.kubernetes.io/name=spawnpod-source-proxy`,
`spawnpod.dev/component=source-proxy`) in the exact operator-owned
`kubernetes.leaseNamespace`. That namespace must differ from the workload
namespace. The Service IP remains fixed in proxy environment variables; direct
connections to these same proxy Pods are also allowed and remain governed by
the proxy allowlist. No broad egress or mutable Pod-IP pin is introduced.
The previous exact-IP behavior remains the default `ip` mode. Operators must
prevent untrusted creation/relabeling of Pods in the proxy namespace.
- Ingress policy for only authorized job pods and TCP proxy port, plus proxy-side egress restriction to the approved upstream IP:443 (and no DNS if all upstreams are pinned).
- Names, labels, selectors, and resource budgets consistent with the existing `infra` namespace conventions.

**Important:** the current binary exposes no explicit `/healthz` route. Do not specify an HTTP health probe unless implemented. A TCP probe or a deliberate minimal health endpoint are both viable; document the trade-off. `listen: 127.0.0.1:3128` from the sample is **not** usable for cross-pod access; choose a pod-reachable listen address, e.g. `0.0.0.0:3128`, with NetworkPolicy restricting access.

### 7.3 Configuration and installation integration

- Retain `profiles.default.network: isolated` explicitly for clarity.
- Introduce a distinct `source` profile for controlled-network CI; clone only the resource limits/image parameters actually needed. Avoid broadening every existing job.
- Configure `sourceProxy: http://<verified-static-IP>:3128`, where `<verified-static-IP>` is a literal address and not a DNS name.
- Determine whether the installer already generates `spawnpod-source-proxy` NetworkPolicy and includes it in rendered bundles. Add missing rendering only if inspection proves it is absent.
- Regenerate derived deployment/admission/network manifests with the project's supported tooling; do not manually edit generated YAML or bundle JSON as the source of truth.
- Verify quota `maxNetworkPolicies` and resource requests account for any additional policies and pods.
- Verify admission policies permit the approved `source` job shape while continuing to reject arbitrary PodSpec modifications.
- Determine and document **exactly** how Forgejo Runner labels select execution profiles. Provide an end-to-end `runs-on` example, not an untested label guess.

### 7.4 Upstream allowlist and credential boundaries

Initial example route:

```yaml
listen: 0.0.0.0:3128
routes:
  - host: git.dmedovich.com
    port: 443
    address: 138.124.231.231 # example observed 2026-10-12; verify before deploy
```

- Do not treat the example IP as permanent. Verify DNS and upstream ownership before deployment, and provide a documented pin-update procedure.
- Restrict tokens independently at Forgejo: checkout and registry push need only the minimum repository/package privileges. Keep tokens out of image layers, logs, rendered manifests, and shell tracing.
- Recognize that a source job may perform **any HTTPS method** to an allowed host. The proxy is not a read-only gate.
- Verify clients honor `HTTPS_PROXY` and do not bypass it via alternate transport, `NO_PROXY`, direct sockets, or sidecars. The network policy must make direct bypass fail.
- Prevent proxy access from unauthorized namespaces/pods through proxy-side ingress policy and any necessary host firewall rules.

## 8. Threat model and security invariants

**Adversary:** untrusted repository workflow steps executing as a non-root job user, potentially holding a scoped short-lived CI token.

**Trust boundaries:** workflow → Runner → SpawnPod adapter → job process; job Pod → proxy; proxy → pinned upstream. The operator, not the workflow, controls the profile, route list, destination IP, and policies.

Required invariants:

1. `isolated` jobs cannot reach DNS, proxy, unrelated workloads, or arbitrary external endpoints. Explicit operator-approved service connections remain available when services are enabled; initial RC7 network acceptance runs with services disabled.
2. `source` jobs can connect only to the approved proxy endpoint and any explicitly approved per-environment services, never directly to the upstream or DNS. Initial RC7 acceptance disables services; combined source/service behavior requires separate verification.
3. The proxy rejects unlisted host:port authorities, mismatched TLS SNI, malformed CONNECT, and unsupported destinations.
4. The proxy cannot reach cluster-internal control-plane addresses, link-local metadata, or other destinations beyond its approved routes and its own network egress policy. Operators must also exclude public node/API addresses from routes; a public-address check alone cannot identify those endpoints.
5. Jobs and proxy run non-root without elevated Linux capabilities, host namespaces, privileged mode, host sockets, or Kubernetes API tokens.
6. Workflow-provided environment variables cannot override operator-enforced proxy routing in a way that enables direct egress; network isolation is the ultimate enforcement layer.
7. No secrets are written into generated bundles or logged by the proxy. The proxy forwards encrypted HTTPS and should not log authorization headers.
8. Existing admission, quota, cleanup, and timeout controls continue to apply.

**Residual risks to document:** malicious operations against an allowlisted HTTPS origin with stolen or overly broad tokens; IP changes or availability loss due to pinning; proxy as a single point of failure; DNS rebinding is reduced by pinned dials but operator must verify updates; TLS/SNI variants rejected by strict parser; proxy overload/DoS; and Kubernetes Service-VIP/NetworkPolicy interactions that must be measured rather than assumed.

## 9. Test plan

### 9.1 Unit and property tests

Extend existing proxy tests to cover:

- Empty, duplicate, invalid, mixed-case and over-limit routes.
- Public vs private/loopback/link-local/multicast/reserved destination addresses; `allowLoopback` only where explicitly permitted.
- Exact host+port matching and blocked alternative ports.
- HTTP absolute-form requests and malformed request targets.
- CONNECT request validation and TLS ClientHello SNI exact matching; absent, mismatched, fragmented, malformed, oversized and unexpected ClientHello variants fail closed.
- Connection closure, timeouts, concurrency, upstream failures and resource limits.
- No leakage of hop-by-hop headers and no sensitive data in error messages.
- Profile configuration rejects `source` without a valid literal-IP `sourceProxy`, and rejects unused `sourceProxy` without a source profile.
- Generated NetworkPolicy selectors and egress CIDR/port match the operator configuration exactly.

### 9.2 Integration tests

Use an isolated test upstream and proxy endpoint to verify:

1. `isolated` job cannot access the proxy or public destinations.
2. `source` job can make an HTTPS request through the proxy without direct DNS access.
3. Disallowed host and mismatched SNI fail.
4. Direct outbound TCP to the approved Forgejo IP fails from the job, while the same request through proxy succeeds.
5. No access to Kubernetes API, CoreDNS, RFC1918, link-local or metadata addresses from jobs.
6. The proxy cannot reach disallowed upstream IPs under its own egress policy.
7. Forgejo checkout with the selected runner/job image works; test tool availability separately (the current Alpine CI image lacks `git`).
8. An actual OCI push/pull against a disposable test repository works through the proxy using scoped credentials and a client verified to honor `HTTPS_PROXY`.
9. The job fails when network access fails; remove `|| true` from connectivity tests.
10. Jobs, policies, and resources are cleaned up on completion and cancellation; default-deny remains unchanged.

### 9.3 Live K3s acceptance gate

Record:

- Exact image digests, renderer outputs, policy manifests, CNI version and node architecture.
- Pod and Service IPs, actual egress destination observed, and successful HTTPS response through proxy.
- Negative results for direct TCP, DNS, unlisted domains and `isolated` jobs.
- A complete Forgejo Actions run with the intended profile selected.
- Controller/Runner healthy after restart, no extra permissions, no regression in RC6 smoke jobs.

Avoid declaring success solely because a workflow exits zero. Assertions must fail on denied or unexpectedly permitted network access.

## 10. Deployment and migration sequence

1. **Baseline:** save the current RC6 generated bundle/config and exact image digests; capture current default-deny, admission, quota, Runner status and a passing isolated smoke job. Preserve rollback artifacts privately; do not export Runner credentials.
2. **Build:** produce and verify proxy binary/image; validate security context and release metadata.
3. **Proxy first:** deploy proxy ConfigMap, Deployment, Service and proxy-side network restrictions in `infra`; verify readiness and upstream reachability from the proxy.
4. **Policy gate:** verify pinned endpoint behavior and CNI enforcement with disposable test pods before permitting source jobs. Never temporarily open broad namespace egress to debug.
5. **SpawnPod config:** add `sourceProxy` and the `source` profile; render and review the generated `spawnpod-source-proxy` policy and any admission changes.
6. **Apply via GitOps:** update authoritative source files, regenerate artifacts, review diffs, commit and sync in the established deployment order. Ensure policy exists before a source-profile job is admitted.
7. **Runner mapping:** configure/verify an explicit Runner label/profile mapping without disrupting the existing `spawnpod` label.
8. **Acceptance:** run positive and negative CI workflows, then OCI push/pull test using disposable scoped credentials.
9. **Enable actual CI/CD separately:** build tooling and GitOps automation are follow-on tasks, not RC7 release blockers unless specifically added to scope.
10. **Rollback:** stop source jobs, restore RC6 config and renderer outputs, restart controller/Runner as required, confirm isolated smoke test, and only then remove proxy resources if no longer needed. Do not delete default-deny.

## 11. Release gates / Definition of Done

RC7 is releasable only when all of the following are demonstrated:

- [ ] Proxy binary/image built reproducibly, versioned, checksum/digest verified and published for supported architectures.
- [ ] A standalone non-root Kubernetes deployment example works with no mounted API token.
- [ ] Proxy configuration is strictly validated and owned by the operator.
- [ ] `source` job traffic is restricted to a pinned proxy endpoint by an enforced NetworkPolicy.
- [ ] Proxy-side ingress/egress restrictions are verified on the target CNI.
- [ ] `isolated` jobs remain fully isolated; RC6 smoke tests pass.
- [ ] Allowlisted HTTPS works; blocked host, port, SNI and direct egress tests fail closed.
- [ ] The Runner label/profile selection mechanism is documented and tested end-to-end.
- [ ] OCI Registry push/pull works via a verified proxy-aware client and least-privilege credentials.
- [ ] Generated manifests and installation/upgrade validation are consistent; no hand-edited generated artifacts.
- [ ] Operational docs cover deployment, route IP rotation, logs/metrics, timeouts, rollback and known limitations.
- [ ] Release notes explicitly state that HTTPS allowlisting is **not** HTTP-method or authorization enforcement.

## 12. Open questions requiring code/cluster verification

1. Does `scripts/release.sh` already compile and package `cmd/spawnpod-source-proxy`? If so, what exact artifacts and images are produced?
2. Does the existing installation renderer include the source-proxy NetworkPolicy automatically when `sourceProxy` is configured? In which output file and with which ownership rules?
3. How does the Forgejo adapter map Runner labels or execution requests to named SpawnPod profiles? Can `source` be selected without replacing the existing default label?
4. What stable proxy endpoint is best for the actual K3s CNI: ClusterIP, stable dedicated IP, or a different operator-managed address? How does policy enforcement interact with Service DNAT?
5. Which upstream endpoints does the selected OCI push client actually contact? Are any redirects to different hosts involved?
6. Is the pinned Forgejo public IP reachable from the proxy Pod, and is hairpin routing via the VPS acceptable operationally?
7. Are readiness checks and observability already exposed by the proxy? If not, is a TCP probe sufficient for RC7?
8. Does the current admission and resource quota configuration need adjustment for the additional source profile or policy?

## 13. Suggested work breakdown

| Work item | Deliverable | Gate |
|---|---|---|
| RC7-01 | Audit existing release outputs and profile mapping | No duplicate implementation |
| RC7-02 | Build/package standalone proxy image | Pinned digest, non-root start |
| RC7-03 | Kubernetes deployment and network policies | Live enforcement checks |
| RC7-04 | Installer/config/render integration | Deterministic output, safe upgrade |
| RC7-05 | Unit/security/integration tests | Positive + negative coverage |
| RC7-06 | Forgejo + OCI registry acceptance | End-to-end passing workflow |
| RC7-07 | Documentation and rollback runbook | Reproducible operator procedure |
| RC7-08 | Release notes and signed/verified artifacts per existing project process | Release checklist complete |

## 14. Post-RC7 follow-up: home-site CI/CD

After RC7 is accepted, independently design the build pipeline: create a pinned, non-root CI tool image; choose a daemonless OCI build/assembly mechanism compatible with Pod Security `restricted`; account for the existing Caddy binary capability-removal requirement; publish immutable image digests to Forgejo Registry; update `shusha-infra` using least-privilege credentials; enable Argo CD self-heal with pruning policy chosen explicitly; and verify rollback. None of these responsibilities belong in the SpawnPod execution backend.

---

### Source basis

This RFC is based on user-provided command outputs and excerpts from the SpawnPod source tree, including `internal/sourceproxy/proxy.go`, `cmd/spawnpod-source-proxy/main.go`, `internal/provider/kubernetes/network_policy.go`, `internal/adapter/forgejo/server.go`, `internal/config/config.go`, current K3s NetworkPolicy and configuration, and Forgejo CI network tests. Unreviewed release and profile-mapping behavior is explicitly listed as an open question rather than asserted as fact.
