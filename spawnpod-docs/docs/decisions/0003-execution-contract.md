# ADR-0003 — Execution and recovery contract

**Status:** Accepted (design direction) · **Date:** 2026-10-07

## Context

Kubernetes `pods/exec` is a transport, not a process supervisor. A lost RPC must not leave an indefinitely running command. An environment must also survive sequential steps without silently replaying an uncertain command after a controller restart.

## Decision

### Job container and process control

- One job Pod per environment, `restartPolicy: Never`, one job container. Its command is an architecture-matched, statically linked spawnPod supervisor, copied by a restricted init container into a read-only mounted tools directory. The job image supplies the userland and image environment; its original entrypoint is **not** run. The supervisor is PID 1 in that container and reaps child processes.
- The controller calls `pods/exec` to launch the same binary in client mode inside the job container. The client communicates with PID 1 over a Unix socket in an environment-private `emptyDir`. Requests, including per-command environment values, travel over the exec **stdin stream**, not argv, PodSpec or Kubernetes metadata. The client relays separate stdout/stderr frames and a terminal result.
- The supervisor starts each command in its own process group, enforces a monotonic deadline, and sends TERM then KILL to the group on cancel/timeout. A command is complete only after the group is reaped or the job Pod is terminated. The controller sends an explicit cancel request over a fresh exec session; if cancellation cannot be confirmed within the cleanup bound, it deletes the Pod and marks the environment unusable. Pod deletion is also the backstop if the supervisor dies or cannot be contacted.
- Only one command or file operation is admitted per environment at a time. The operation slot allows `Remove` to set a terminating flag and send cancellation while `Exec` is active; it must never hold a mutex that blocks cancellation behind the active stream. Each command has an opaque ID. The supervisor rejects a duplicate ID with a different request digest and returns a completed result only while that supervisor remains alive. It never starts an uncertain command again after a transport failure.
- When the exec client detects its transport has closed without a clean terminal result, it disconnects from the supervisor; the supervisor applies a short disconnect grace and then cancels the command. Transport loss that is not detectable locally is bounded by the hard in-Pod command deadline. The controller reports **indeterminate execution** if it cannot establish whether the command finished; it never converts that to exit code zero. Output is streamed with bounded queues and backpressure. Excess output terminates the command with an explicit limit error; no silent truncation.
- The supervisor is operational control within an untrusted job container, **not** a security boundary against its own workload. A hostile job may kill or impersonate local processes. Any supervisor integrity failure fails the environment and triggers Pod deletion; no later step reuses it.

### Identity, state, recovery

- The controller generates a random environment ID. A ConfigMap in `ci`, named from that ID, stores a versioned, non-secret record: owner/integration, approved profile revision, desired state, deadline, resource names and UIDs, and last operation ID/status. Pod and service resources carry owner labels plus the same ID. The ConfigMap is the management record; Kubernetes Pods/Services/PVCs are the resource source of truth. All mutations use `resourceVersion` compare-and-swap and check resource UID before deletion.
- State progression is `Requested → Provisioning → Ready ↔ Executing → Terminating → Cleaned`, with `Rejected`, `Failed` and `Indeterminate` as observable outcomes. Cleanup status is separate from execution outcome. `Remove` first records desired termination, then cancels and deletes owned resources, and finally retains a small tombstone for 24 hours before garbage collection. Repeated `Remove` for a tombstone succeeds; an unknown ID returns `NotFound`.
- At startup and periodically, reconciliation lists only labeled spawnPod resources in `ci`, checks ConfigMap identity/UID and deadlines, adopts management of idle ready Pods, and retries cleanup. An interrupted command/output stream cannot be resumed. After controller restart, an environment recorded as executing becomes indeterminate and is terminated; the runner receives a transport/infrastructure failure if its RPC is still connected. Orphan resources without a valid record are quarantined for a bounded grace, logged, then deleted only if their ownership label and immutable annotation agree.
- Internal Create supports an explicit caller idempotency key plus request digest. The observed Forgejo `Create` contract does not provide a guaranteed stable retry key, so its adapter cannot promise exactly-once creation if a response is lost. Such Pods are bounded by their deadline and reconciled. Do not infer uniqueness from a human-readable name without an upstream guarantee.
- Single active controller replica is the supported deployment. A second replica is refused by readiness unless it owns a Kubernetes Lease; the initial deployment sets replicas to one. Future standby/failover uses a Lease and fencing by Lease epoch plus ConfigMap CAS. No active-active command execution is promised. A failover does not replay streams.

## Alternatives

Raw `pods/exec` per command; long-running commands in separate Kubernetes Jobs; a network listener in each job Pod; external SQL state store; active-active controllers. Raw exec cannot prove process cancellation. Jobs lose the same-container workspace/process context. A network agent adds a job-to-controller attack surface. SQL and active-active coordination are unnecessary for the initial single-node deployment.

## Consequences and proof obligations

The supervisor must run on both amd64 and arm64 and be executable in approved images. The workspace and tools volume permissions must work with the profile UID. Tests must cover disconnect, cancelled stream followed immediately by `Remove`, supervisor death, child process trees, output pressure, API outage, restart, resource UID replacement, and duplicate request IDs. If process-group termination leaves descendants in another group, delete the entire Pod before returning a successful cancellation. A node partition can delay controller-driven deletion; the in-Pod command deadline is the local limit, while Kubernetes reconciliation resumes after connectivity returns.
