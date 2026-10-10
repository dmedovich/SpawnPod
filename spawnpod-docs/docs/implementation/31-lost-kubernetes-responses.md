# Implementation step 31 — lost Kubernetes Create and Delete responses

**Date:** 2026-10-10. These deterministic fault tests inject an error after
the fake Kubernetes API has already stored or deleted an object. This models
an ambiguous transport result without disrupting the local K3s control plane.

The engine test first stores an environment ConfigMap, then reports that its
Create response was lost. The caller receives an error and no environment ID,
but a later reconciliation finds the persisted `provisioning` record and
marks it `cleaned`. Another engine test models a job provider that reports a
lost Create response, then returns the matching Pod UID on lookup. The record
has no Pod UID and becomes `failed`; reconciliation obtains that UID, removes
the Pod, and records its UID in the cleaned tombstone. The provider's lookup
checks the deterministic name and ownership labels.

The Kubernetes service provider test injects the same lost-success response
at each of the four creation steps: egress NetworkPolicy, ingress
NetworkPolicy, ClusterIP Service, and service Pod. It verifies that a later
lookup recovers the UID of the hidden success and that UID-checked cleanup
removes every resource created before the error. A job Pod Delete test makes
the fake API delete the Pod and then report a transport error; retry accepts
the subsequent `NotFound` as completion.

```sh
go test ./internal/engine ./internal/provider/kubernetes -count=1
go test ./...
go vet ./...
```

These tests cover provider and engine behavior for lost responses. They do
not inject a fault into a particular encrypted request on the live K3s API.
Live quota and finalizer failure cases are recorded in
[step 26](26-live-service-failure-recovery.md) and
[step 30](30-live-delete-retry.md).
