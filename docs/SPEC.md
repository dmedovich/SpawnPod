# SpawnPod release specification

Applies to **v0.1.0-rc.1**. This document specifies the shipped candidate;
`spawnpod-docs/docs/PRD.md` describes the broader target product. Unsupported
capabilities in that target are not promises made by this release.

## Purpose and supported boundary

SpawnPod is an execution backend, not a workflow scheduler. Forgejo Runner
chooses commands and step ordering; SpawnPod validates requests, provisions
bounded Kubernetes environments, supervises commands, transfers workspace
files, and reconciles owned resources after interruption.

| Boundary | Candidate contract |
|---|---|
| Host/Pod platform | Linux/amd64; tested on K3s v1.34.12+k3s1 |
| Adapter | Official Forgejo Runner v13.1.0, pinned `plugin.v1alpha` schema |
| Forgejo test version | v15.0.9 |
| Controller topology | One active controller, Kubernetes Lease, Unix RPC |
| Execution images | Operator-approved digest, numeric non-root UID/GID, matching architecture |
| Workspace | Per-environment bounded `emptyDir`, reused across steps |
| State storage | Non-secret ConfigMaps in `ci`; record versions 1 and 2 |
| Services | Explicitly approved separate Pods and ClusterIP Services |
| Network | Isolated or source profile; maintained explicit HTTP(S) proxy routes |
| Distribution | Static Linux/amd64 binaries and a scratch runtime image build recipe |

The official Runner source used for compatibility tests is commit
`6095cb17bfdbded5aa4ea84c18a4b69fd9574cca`. This release does not claim live
ARM64 support, HA execution, arbitrary Docker compatibility, privileged image
builds, PVC workspaces, cross-job caches or SSH checkout.

## RPC and execution contract

The authoritative wire schema is
`internal/adapter/forgejo/proto/plugin.proto`; its MIT license is preserved.
`Capabilities` reports the backend name only. Internal diagnostic operations
are not advertised as additional Forgejo RPC capabilities.

| RPC | Behavior |
|---|---|
| Create | Validate profile/image/deadline/options, persist intent, create owned resources, return opaque environment ID and workspace paths |
| Start | Wait for approved resources, obtain OS/architecture/image environment, emit StartComplete |
| Exec | One active operation per environment; argv, workdir and transient environment; stdout/stderr and terminal exit/failure event |
| CopyIn | Bounded tar extraction within the workspace; unsafe entries fail |
| CopyOut | Bounded tar archive of a workspace path |
| Remove | Cancel active work, remove owned resources using UID checks, retain a cleaned tombstone; safe to repeat |

The workspace root is `/workspace`; workdirs must remain within it. The image
entrypoint is replaced by the agent. The image supplies its own shell, Node,
Git and any other workflow tools. `Exec.user` must be empty or the configured
numeric UID. Unknown backend options, capability changes and unapproved
images/services are rejected.

Commands have opaque IDs and a maximum 30-minute execution timeout. Environment
lifetimes are bounded by their approved profile. Provisioning has a 10-minute
bound. File transfer allows at most 512 MiB archive input, 256 MiB per file and
10,000 entries; extraction is rooted and rejects traversal and unsafe links.

Terminal events distinguish exit status from infrastructure failure.
`ExecFailed` carries a nonempty message. The adapter maps invalid requests to
`InvalidArgument`, unknown environments to `NotFound`, busy/invalid lifecycle
states to `FailedPrecondition`, cancellation/deadlines to their gRPC codes,
and recognized Kubernetes timeouts/unavailability to `Unavailable`; other
backend failures receive a generic `Internal` message.

## Lifecycle, interruption and shutdown

State transitions are implemented in `internal/engine`. Ready environments
support sequential commands. Interrupted provisioning, execution or transfer
is cleaned during reconciliation; uncertain commands are never replayed.
Resource operations verify ownership and recorded UIDs. Cleaned records are
retained for at least 24 hours before expiration. API outages can postpone
cleanup; retries and local command deadlines bound the behavior available
while the control plane is unreachable.

Leadership must be held before opening the private RPC socket. Losing the
Lease stops RPCs immediately. On operator shutdown, readiness fails and new
connections stop; active RPCs may finish within `controller.drainTimeout`
(default 30s, allowed 1s–5m), after which they are cancelled. Lease renewal
continues during drain. This drains RPCs, not whole multi-step workflows.
Quiesce Runner before planned upgrades. Recovery metadata remains readable
by this release for record versions 1 and 2; unsupported record versions fail
closed. No untested previous-version rollback compatibility is claimed.

## Configuration and installation

Configuration is strict version-1 YAML, rejects unknown fields/trailing
documents, does not expand environment variables, and pins execution images
by digest. Use `deploy/config.example.yaml` as the complete field reference.
`validate` works offline. `render-admission` derives the image/shape policy,
container ceilings, quota and optional source egress policy from that config.
`doctor` verifies the installed baseline with the configured identity; live
negative tests are still required to prove admission and network enforcement.

`render-deployment` accepts an operator config plus digest-pinned controller
and official Runner images. It renders a ConfigMap, single-replica Recreate
Deployment and internal monitoring Service for the `infra`/`ci` reference
layout. It discards host kubeconfig paths. Only the controller container
mounts a projected Kubernetes token; Runner receives a separate externally
created registration Secret. Both containers use UID/GID 10001 and share a
private Unix socket. Config changes alter the Pod template checksum.

## Operational API

`controller.monitorAddress` is optional; an empty value disables HTTP.
Host installs should bind to loopback. Generated Kubernetes installs bind
`:9090` and expose only a ClusterIP Service. There is no authentication on
these endpoints; keep them on a trusted management network.

| Endpoint | Contract |
|---|---|
| `/healthz` | Local HTTP/process health; independent of Kubernetes availability |
| `/readyz` | 200 only while serving, not draining, with a recent successful API probe; otherwise 503 |
| `/metrics` | Prometheus text format; no environment, repository, token or error-text labels |

Metrics include `spawnpod_ready`, reconciliation count/failure count/last
success timestamp, and RPC count/failure/active/duration sum+count by the
bounded method set. RPC failure counts represent gRPC errors; a successfully
transported command with a nonzero exit or ExecFailed event is not necessarily
a failed RPC. Counters restart with the process. API probes run every five
seconds with a five-second timeout; readiness expires after 20 seconds.
Metrics do not provide a latency histogram or a production SLO.

## Security assumptions

The operator and Runner registration are trusted; workflow commands are not.
Execution Pods receive no Kubernetes API token, host mounts, Secret volumes
or PodSpec environment secrets. Command secrets travel in execution request
bodies. Restrictive admission, RBAC and NetworkPolicies are installation
requirements. A source profile injects proxy settings into commands and
clears NO_PROXY; the external proxy pins the destination IP/port and checks
HTTPS SNI. The Runner host's action download is a separate network path.

Job Pods share the node kernel. NetworkPolicy cannot stop a malicious job
from sending its secrets to an otherwise approved source host. The source
proxy is an operator-maintained dependency; changing public upstream IPs
requires route maintenance. Strong tenant isolation requires a separately
validated runtime or node boundary.

## Acceptance evidence

The source implementation journal records the exact environments and run IDs.
Steps 22–36 cover the local RPC lifecycle, official Runner, negative policy
checks, services, restart/API/deletion faults, tar traversal and HTTPS checkout.
Run 39 verifies private HTTPS Node-based checkout with exact commit and shallow
repository checks. Candidate-specific packaging, probes and shutdown evidence
is recorded in step 37. Outstanding production gates are listed in RELEASE.md.
