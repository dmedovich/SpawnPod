# Implementation step 03 — environment engine and durable state

**Status:** engine, ConfigMap store and Kubernetes adapter implemented; local
unit and race tests pass. Real cluster behavior remains unverified.

## Package boundaries

- `internal/engine` owns lifecycle states, operation ordering, cancellation,
  deadlines, reconciliation and the small `Store`/`Jobs` contracts. It imports
  neither Forgejo protobuf messages nor Kubernetes API types.
- `internal/provider/kubernetes/store.go` stores one non-secret JSON record per
  environment in a labeled ConfigMap. `resourceVersion` is the compare-and-swap
  token; updates re-read the record and the API enforces the version on write.
- `internal/provider/kubernetes/engine_adapter.go` maps the engine's job
  handles to Kubernetes Pod names and UIDs. A changed UID is never treated as
  the same job.

## Request path

1. `Engine.Create` generates a random 128-bit ID and stores a `provisioning`
   record before requesting the Pod. It records the Pod UID returned by the
   API. `Create` is in the active-operation registry, so a concurrent `Remove`
   waits for it. If a delayed API response arrives after removal begins, the
   record stays terminating and `Remove` discovers the Pod by its deterministic
   name and ownership labels before deleting it.
2. `Engine.Start` waits for readiness under the environment deadline, then
   moves the record to `ready`. A failed readiness check marks it `failed` for
   reconciliation.
3. `Engine.Exec` records `executing` and the command ID before starting the
   stream. A process exit, including a nonzero code, returns the environment
   to `ready`. A transport or execution failure is indeterminate; the engine
   attempts bounded cleanup and never replays the command.
4. `Engine.Remove` records `terminating`, cancels an active operation without
   holding its slot, sends an explicit command cancel when applicable, waits
   for the operation to finish and removes the UID-matched Pod. Concurrent
   Remove calls share one cleanup attempt. A successful cleanup records
   `cleaned` and leaves a tombstone for 24 hours.
5. `Engine.ReconcileOnce` skips live operations, cleans interrupted executions,
   failed/terminating records and expired environments. It expires old cleaned
   tombstones with a ConfigMap resourceVersion precondition. A provisioning
   record is treated as stale after ten minutes.

## Verification

`go test -race ./internal/engine ./internal/provider/kubernetes` covers normal
lifecycle, nonzero command exit, cancellation racing with Exec, Remove racing
with a delayed Create, interrupted execution after restart, and tombstone
expiry. The Kubernetes provider tests check Pod security fields, readiness UID
and deletion UID preconditions.

## Limits before the next step

- A real K3s test is needed for ConfigMap resourceVersion conflicts, Pod
  admission, volume permissions, readiness and `pods/exec` cancellation.
- The controller process and periodic reconciliation are implemented in step
  04. Authentication, profile selection from a Forgejo request, and the RPC
  adapter are not yet wired together.
- Service Pods, file transfer and NetworkPolicy ownership are not yet part of
  the durable record. Do not expose this engine as a CI backend yet.
- Reconciliation currently scans owned ConfigMaps. Detection and quarantine of
  labeled Pods with no valid record still need implementation and a cluster
  fault test.
