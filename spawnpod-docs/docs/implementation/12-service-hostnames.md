# Implementation step 12 — service hostnames in the job Pod

**Status:** the engine and Kubernetes provider now map approved service names
to assigned ClusterIP addresses in the job Pod's `/etc/hosts` through
`HostAliases`. Local tests pass; K3s DNS, routing and NetworkPolicy behavior
remain unverified. Forgejo `Create` still rejects `services:`.

## Creation order

1. The engine persists service intent before any Kubernetes create call.
2. It creates each ClusterIP Service and service Pod and records their UIDs.
3. `ResolveServiceAddress` checks the Service UID, labels, selector and port,
   then waits for a valid API-assigned ClusterIP. The environment deadline
   bounds this wait.
4. The engine stores the IP and derives `JobSpec.ServiceAliases` itself. Any
   alias supplied in the original job spec is discarded.
5. `BuildJobPod` accepts at most eight unique DNS-label service names and
   unicast IP literals excluding loopback and link-local addresses. It sorts
   aliases for a stable PodSpec and writes them to `spec.hostAliases`. It
   rejects reserved `localhost` and `kubernetes` names.
6. `Start` waits for every service's TCP readiness and rejects an address
   change after job Pod creation, since `HostAliases` cannot be updated on an
   existing Pod.

`HostAliases` requires a concrete ClusterIP before job Pod creation. A lost
Service create response is handled by the saved intent and UID recovery; an
address lookup failure marks the environment failed for reconciliation.

## Verification and remaining work

Engine tests verify the derived alias, persisted IP and failure on an address
change. Provider tests verify deterministic Pod aliases, invalid names and
addresses, and ClusterIP resolution with matching UID. Run `go test
./internal/engine ./internal/provider/kubernetes`.

A protected channel for workflow service environment values was added in
[step 13](13-service-startup-stream.md). Service NetworkPolicy, admission rules
and real K3s routing tests are still required before enabling Forgejo
`services:`.
