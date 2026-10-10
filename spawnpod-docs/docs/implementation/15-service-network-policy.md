# Implementation step 15 — service NetworkPolicy lifecycle

**Status:** policy rendering, API lifecycle and local tests implemented. A
local K3s job reached its approved service, while a second environment could
not reach that service ClusterIP. See [step 25](25-live-service-isolation.md)
for evidence and limits. The example config keeps services disabled.

## Baseline and selectors

Apply [`deploy/network-default-deny.yaml`](../../../deploy/network-default-deny.yaml)
in the workload namespace before enabling services. It selects every Pod with
`spawnpod.dev/managed=true` and denies ingress and egress. The controller
preflight always checks that exact selector, both policy directions and empty
rule lists before serving any job. Presence of the API object
does **not** prove that the CNI enforces it.

For each approved service, the provider creates two namespace-local policies:

- `e-<environment-id>-<service>` selects only the matching job Pod and permits
  TCP egress to the matching service Pod on its approved port.
- `i-<environment-id>-<service>` selects only that service Pod and permits TCP
  ingress from the matching job Pod on the same port.

The job Pod has no `spawnpod.dev/service` label; service Pods do. Both selectors
include the opaque environment ID and managed label. No service egress, job
ingress, DNS, proxy or internet route is granted here. Multiple services add
one pair of policies each. Responses to allowed connections depend on normal
stateful NetworkPolicy handling by the installed CNI.

## Creation and cleanup

The engine records service intent first. The provider creates both policies
before the ClusterIP Service and service Pod. Their UIDs join the persisted
service handle. Lost or partial create responses are recovered by deterministic
name and checked ownership labels. Cleanup stops the Pod, removes the Service,
then deletes the two policies using current UID preconditions; a recorded UID
mismatch is an error. Old service records without policy UIDs can recover them
by name during cleanup.

Local fake-client tests cover selectors and ports, partial resource creation,
UID-constrained deletion and refusal to adopt a foreign policy. Targeted K3s
tests now cover default deny to the API Service, same-environment connectivity
via ClusterIP and rejection from another environment. DNS, node, metadata,
individual ingress/egress rules and failure paths remain. Source checkout will need
a separately designed proxy route; with this policy alone it is blocked.
