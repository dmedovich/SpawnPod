# Architecture and lifecycle

SpawnPod is an execution backend for the official Forgejo Runner. It does not
schedule workflow steps, evaluate expressions or replace Runner's interaction
with Forgejo.

## Components

| Component | Responsibility |
|---|---|
| Forgejo Runner | Accept jobs, interpret workflows and invoke the backend plugin |
| Forgejo adapter | Validate RPC requests against approved execution profiles |
| Lifecycle engine | Serialize environment operations and persist recovery state |
| Kubernetes provider | Create and delete owned Pods, Services and network permissions |
| In-Pod agent | Supervise processes and transfer files inside the workspace |
| Source proxy | Optional operator-maintained HTTP(S) routes for source access |

Runner and controller share a private Unix socket. The installation renderer
places them in one Pod and mounts a projected Kubernetes token only in the
controller. The Runner registration Secret is mounted only in Runner.
Execution Pods receive neither credential.

## Environment lifecycle

`Create` persists intent before provisioning resources. `Start` waits for the
job and approved services, then checks runtime metadata. A ready environment
permits sequential command execution and file transfer.

| State | Meaning after a controller restart |
|---|---|
| Provisioning | Creation may be incomplete; reconcile cleanup |
| Ready | Retain until its deadline, subject to service checks |
| Executing | The command result is uncertain; reconcile cleanup |
| Transferring | The workspace may be incomplete; reconcile cleanup |
| Indeterminate / failed / terminating | Retry cleanup |
| Cleaned | Keep a tombstone for at least 24 hours, then expire it |

Commands with uncertain results are never automatically replayed. Per-environment
operation slots prevent simultaneous commands. Removal cancels active work and
uses resource UIDs to avoid deleting a replacement object with the same name.
API outages can postpone cleanup; stored intent supports later retries.

One active controller per namespace is supported. A Kubernetes Lease controls
RPC serving. Lease loss stops RPCs immediately. Planned shutdown drains active
RPCs for a bounded interval, not a complete multi-step workflow; stop Runner
from accepting work before upgrades.

## Execution and storage

Each job gets an `emptyDir` workspace reused between its steps. The approved
image supplies the shell, Git, Node and other workflow tools. An init container
installs the agent; the job container runs it as its main process.

Resource limits apply to the container. The command helper installs an
inherited seccomp guard that prevents commands from changing session/process
group. The supervisor kills the group before reaping the leader, and bounded
writes keep a stalled output reader from blocking cleanup. Commands must
follow the supported syscall restrictions.
See [security](SECURITY.md) for command restrictions.

ConfigMaps contain management records, not command environments or service
credentials. Record versions 1 and 2 are supported. The store returns valid
records alongside a typed partial-list error, allowing cleanup while preserving
unsupported records. Incompatible records prevent controller startup. A running
engine blocks new environments after reconciliation detects them. Mixed-version
controllers and automatic rollback across schema changes are unsupported.
