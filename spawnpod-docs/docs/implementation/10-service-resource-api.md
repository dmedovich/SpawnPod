# Implementation step 10 — service resource API

**Status at step 10:** Kubernetes provider methods for service resource
creation, recovery, readiness and UID-scoped cleanup were implemented and
tested against the client-go fake. The engine was connected in
[step 11](11-service-engine-lifecycle.md); the Forgejo adapter still rejects
`services:`.

## Resource lifecycle

`internal/provider/kubernetes.CreateServiceResources` creates the ClusterIP
Service, then its Pod, from the approved `ServiceSpec`. It returns both API
assigned UIDs. A caller must record the deterministic resource name before
calling Create: a lost API response may leave one or both resources present.

`FindServiceResources` looks up that name and verifies the managed,
environment and service labels on each object before returning UIDs. This
allows recovery from a partial create without adopting a differently labeled
resource. `WaitServiceReady` requires both recorded UIDs, the approved
ClusterIP selector and port, an assigned ClusterIP, and a Pod Ready condition
from its TCP probe. It rejects a terminal Pod or changed identity.

`RemoveServiceResources` rechecks current labels and recorded UIDs, deletes
the Pod and Service with Kubernetes UID preconditions, then waits until both
are absent. It is repeatable after a partial cleanup. A changed UID or foreign
labels cause an error instead of deleting another resource.

## Remaining integration

The engine now persists service intents and UIDs and calls these methods.
The job Pod still needs a validated service alias. Forgejo `Create` still
rejects `services:` because workflow service environment values can be secret
and there is no protected service startup channel yet. NetworkPolicy and
admission enforcement remain necessary before exposing this feature.

## Verification

`go test ./internal/provider/kubernetes` covers creation, recovery, readiness,
repeatable cleanup, UID preconditions and refusal to adopt a foreign resource.
The fake API does not prove K3s ClusterIP routing or TCP readiness behavior.
