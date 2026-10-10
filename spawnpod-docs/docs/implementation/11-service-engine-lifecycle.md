# Implementation step 11 — service lifecycle in the engine

**Status at step 11:** the engine coordinated approved service resources
through the Kubernetes provider adapter. Job hostname aliases were added in
[step 12](12-service-hostnames.md). The Forgejo RPC still rejects `services:`;
secret-safe service environment startup and network policy are not implemented.

## Persistent intent

`engine.CreateRequest.Services` contains at most eight unique, approved,
non-secret `ServiceSpec` values. The engine assigns the environment ID and
stores one `ServiceRecord` per service in the same ConfigMap as the job intent
before any Kubernetes create call. Each record contains the approved spec,
deterministic resource name and later the Pod and ClusterIP Service UIDs. A
lost API response can therefore be reconciled using the saved name and labels.
Records with services use schema version 2; version 1 remains readable for
existing job-only environments. An older controller rejects version 2 instead
of silently ignoring service resources during rollback.

`Create` creates service resources before the job Pod. It records each returned
UID pair. A partial service creation marks the environment failed; the next
reconciliation removes any resource already created. `Start` waits for the job
agent and all service readiness probes, then records each assigned ClusterIP
and transitions to Ready. An error marks the environment failed.

`Remove` requests cancellation of an active operation and attempts job and
service cleanup independently. It leaves the record in Terminating if any
resource cannot be deleted, allowing a later retry. After a controller
restart, an idle Provisioning record is treated as abandoned immediately;
reconciliation no longer waits ten minutes before cleaning it.

## Boundaries and verification

`internal/provider/kubernetes.EngineServices` translates the engine's string
UIDs to Kubernetes types. The controller provides both job and service
adapters to the engine. Tests cover persisted service intent, readiness and
ClusterIP recording, partial create recovery, and continued service cleanup
when job Pod deletion fails. Run `go test ./internal/engine
./internal/provider/kubernetes`.

The job Pod receives aliases as of step 12. Service environment variables
from Forgejo can contain secrets;
neither these records nor the current PodSpec carry them. The adapter keeps
rejecting `services:` until aliasing, protected startup transport and network
enforcement are complete and tested in K3s.
