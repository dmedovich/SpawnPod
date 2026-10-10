# Security and production acceptance

The tagged `v0.1.0-rc.1` candidate predates the hardening below. A source
review of commit `903c9e76ba9b1d85d26da326e28de88a505804a4` on 2026-10-10
reproduced five initial issues and an additional API-outage hang. The fixes are included in `v0.1.0-rc.2`; no target-server deployment was performed. This is source review and local
regression evidence, not an external audit or proof of production readiness.

## Findings and local fixes

P0 marks a production gate; P1 marks a recovery or upgrade improvement.
These priorities are not CVSS scores. Source locations below refer to the
reviewed revision, not the modified working tree.

| ID | Priority | Finding at reviewed revision | Local fix |
|---|---|---|---|
| SP-01 | P0 | A descendant using `setsid` survives successful step completion (`internal/supervisor/server.go:138,175–179`) | Inherited seccomp guard prevents process-group/session escape; group kill happens before reaping the leader |
| SP-02 | P0 | Start errors expose executable values (`internal/supervisor/server.go:140–141`) | Fixed startup messages; private close-on-exec status pipe |
| SP-03 | P0 | Non-reading peer holds the command slot beyond timeout (`internal/supervisor/server.go:106–116,171`) | Bounded socket writes, cancellation unblocks output, bounded cleanup and shutdown |
| SP-04 | P0 | Raw agent/transport diagnostics reach controller logs (`internal/provider/kubernetes/copy.go:76–77`; `internal/engine/engine.go:308`) | Discard infrastructure stderr, fixed diagnostic messages, sanitize terminal errors and gRPC status messages |
| SP-05 | P1 | Unsupported record prevents supported-record recovery (`internal/provider/kubernetes/store.go:128–130`) | Typed partial listing; reconcile supported records, preserve rejected records, block new environments after incompatibility is detected |
| SP-06 | P1 | Failure-state persistence uses unbounded background context, holding the operation slot during an API outage (`internal/engine/engine.go:309`, also other failure paths) | Bound best-effort persistence by cleanupTimeout; release operation and recover durable intent after connectivity returns |

### Command execution contract after hardening

The agent launches each step through a private helper. Before executing the
requested program, the helper installs a seccomp filter inherited by threads,
children and subsequent execs. It denies `setsid`, `setpgid`, namespace changes,
`ptrace` and `process_vm_writev`. `clone` cannot create user or PID namespaces;
`clone3` returns `ENOSYS` so ordinary libc/runtime fork fallbacks remain usable.
The filter checks the native syscall ABI and rejects alternate ABIs.
`no_new_privs` prevents gaining privilege through exec. If the filter cannot be
installed and synchronized across helper threads, the command does not start.
The agent disables dumpability to protect its memory against same-UID procfs access.

This narrows supported command behavior: tools requiring session changes, job
control, daemonization or namespace creation are unsupported. Ordinary foreground
shell commands, Git and Node child processes passed a local non-root, read-only,
capabilities-dropped container test with no network. Linux/amd64 was executed;
Linux/arm64 was cross-compiled only. The target runtime must permit installing
the additional filter. No extra container privileges or cgroup delegation are required.

After exit or cancellation, the leader remains unreaped until the final process
group kill, avoiding PID reuse in cleanup. Unexpected wait/cleanup failure marks
the agent unavailable rather than reusing an uncertain environment. This remains
process-group supervision under a restricted syscall contract, not a cgroup
sandbox against hostile code sharing the agent's UID. Service processes run in
separate Pods and retain their foreground, container-lifetime contract.

### Diagnostics and output

Fixed messages replace executable paths, working directories, image values and
arbitrary backend status text at the corrected diagnostic boundaries. Agent
infrastructure stderr is discarded by the provider, and agent CLI failures use
a fixed message. gRPC codes and context cancellation classifications are retained
without carrying remote messages or status details. Synthetic canary regressions
cover startup errors, controller logs, adapter terminal events and status mapping.
No real credentials were used.

Workflow stdout/stderr is intentionally raw: code receiving a secret can print
it. Runner masking requires separate validation. Service processes also write
to container stdout/stderr; Kubernetes log access remains part of the trust model.

### Output stalls and cancellation

Socket writes have a grace-sized deadline and cancellation interrupts a blocked
writer. Command cleanup does not depend on delivering a terminal event to a
non-reading peer. Initial requests have a five-second deadline. Disconnects
cancel execution; trailing request whitespace does not. Server shutdown cancels
handlers and waits for their bounded cleanup. Tests cover a non-reading peer,
slot reuse, disconnect, shutdown and 100 exit/cancel races per test invocation.

### Record recovery and upgrade limits

The store accepts record versions 1 and 2 and rejects unknown fields or malformed
JSON without echoing record contents. `List` returns valid records with a typed
partial-list error for rejected records. Reconciliation cleans valid records,
preserves rejected ConfigMaps byte-for-byte and still returns an error. Other
API list errors do not authorize partial cleanup. Initial reconciliation failure
continues to prevent RPC startup. A running engine blocks new environments after
reconciliation detects incompatible records; a successful later listing clears
that gate. Detection follows the reconciliation interval, not admission-time validation.

Unit tests cover version-1/version-2 round trips and partial recovery. Local K3s
acceptance also runs the rc.1 and rc.2 controller binaries against persisted records,
with all jobs removed before each switch. Both controllers use the current agent
image. This establishes the tested quiescent controller upgrade/rollback path,
not migration of running jobs or compatibility of old agents.
Mixed-version controllers and automatic schema downgrade remain unsupported.

## Local validation

See [VALIDATION.md](VALIDATION.md) for reproducible commands, the local K3s
fault scenarios, workload measurements and their limits. Regression checks
exercise ordinary sh/Git/Node, denied session changes, safe startup failures,
output stalls, cancellation races and fail-closed seccomp installation.
Failure-state persistence now has a cleanup timeout. The command slot is released
before publishing a terminal event, and helper startup pins its OS thread while
setting thread-local exec restrictions. These changes have regression coverage.

## Trust model

Operators and Runner registration are trusted. Workflow commands are not.
Approved image digests, fixed non-root identities, restricted admission,
namespace RBAC, quotas and enforcing NetworkPolicies are installation
requirements. Job Pods have no Kubernetes API token, host mounts or Secret
volumes. Secrets needed by commands are passed in request bodies and are not
stored in environment records.

Job code and its agent share a container and UID; the in-container supervisor
is not a security boundary against that code. Pods share the node kernel.
Use a separately validated runtime or node boundary for hostile tenants.
An approved destination can still receive exfiltrated secrets from a job;
network allowlists cannot infer whether a permitted request is legitimate.

## Required validation before a target-cluster trial

1. Repeat the packaged command compatibility and failure checks in the actual runtime.
2. Reproduce admission denials and real packet-level network denials on the
   target K3s. `doctor` checks API objects and permissions, not CNI enforcement.
3. Exercise interruption in every persisted lifecycle state, API outages,
   lost responses, concurrent cancellation and resource deletion.
4. Verify record compatibility and rollback using two actual release versions.
5. Measure idle and concurrent CPU/RAM, scheduling pressure and cleanup latency
   with a budget protecting the target server's existing services.

No target-server deployment or capacity measurement was performed as part of
this review. ARM64 support remains outside the candidate contract.
