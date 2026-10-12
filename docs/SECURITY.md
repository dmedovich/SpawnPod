# Security model

## Trust and isolation

Operators, cluster administrators and Runner registration are trusted.
Workflow commands are untrusted. The operator controls approved images,
resource budgets, service definitions and network destinations.

Job code and its agent share a container and UID. The supervisor controls
process lifecycle; it is not an isolation boundary against code in that
container. Pods share the node kernel. Mutually hostile tenants require a
separately validated container runtime or dedicated nodes.

## Installation requirements

- Pin runtime, Runner, job and service images by digest. Use job images that
  support the configured numeric non-root UID and a read-only root filesystem.
- Enforce Restricted Pod Security, SpawnPod admission policies, namespace RBAC,
  quotas and default-deny NetworkPolicies. Verify admission rejection and
  packet-level network denial on the installed cluster.
- Keep the Runner/controller Unix socket private. Only the controller receives
  a Kubernetes API token; only Runner receives its registration credentials.
  Job Pods receive no API token, host mounts or Kubernetes Secret volumes.
- Restrict access to the unauthenticated health and metrics endpoints to the
  management network. Protect operator configuration and kubeconfig files.
- Use a Linux/amd64 runtime that permits installing the command seccomp filter.
  No additional container privileges or cgroup delegation are required.

See [INSTALL.md](INSTALL.md) for configuration and installation commands.

## Command restrictions

The command helper installs an inherited seccomp filter before execution.
It denies session/process-group changes, namespace changes, tracing and
`process_vm_writev`. `clone` cannot create user or PID namespaces; `clone3`
returns `ENOSYS` to allow libc/runtime fallbacks. Alternate syscall ABIs are
rejected. `no_new_privs` prevents gaining privilege through exec. Commands
fail to start if the filter cannot be installed and synchronized across threads.
The agent disables dumpability to restrict same-UID procfs access to its memory.

Tools requiring job control, daemonization or namespace creation are unsupported.
Services run in separate Pods as foreground processes.

Cleanup kills the process group before reaping its leader. Unexpected process
wait or cleanup failures make the agent unavailable. Socket writes and shutdown
cleanup are bounded; disconnecting the command client cancels execution.

## Secrets and output

Command secrets travel in execution request bodies and are not stored in
environment records. Infrastructure failures use fixed diagnostic messages;
the provider discards agent infrastructure stderr.

Workflow stdout/stderr is passed through without redaction. A command receiving
a secret can print it. Verify Runner masking separately and restrict access to
workflow logs. Service output goes to Kubernetes container logs; restrict log
access through cluster RBAC.

## Network access

The isolated profile denies job egress. A source profile permits access through
an operator-maintained HTTP(S) proxy with explicit destination routes. The proxy
pins destination IP/port and checks HTTPS SNI. Maintain routes and CA trust as
upstream addresses and certificates change. Runner action downloads use a
separate network path.

A permitted destination can receive secrets sent by a malicious job. Network
allowlists do not prevent exfiltration to approved hosts.

HTTPS tunnels allow any application method, including registry writes. Scope
tokens independently of the destination allowlist. Jobs can overwrite their own
process environment; NetworkPolicy is the enforcement layer against direct
bypass. Policies are additive, so required-policy validation alone cannot prove
the absence of another broad allow rule. Review the complete policy set and
verify actual packet denials on the target CNI. Operator-approved per-environment
services are explicit additional connections when services are enabled.

For Kubernetes proxies behind Service DNAT, explicit `sourceProxyPolicy:
pod-selector` grants the configured TCP port only to the two fixed proxy labels
in the exact operator-owned Lease namespace. The Service IP stays pinned in
proxy env; direct access to these same backend Pods is also allowed. Restrict
Pod creation/relabeling in that namespace to trusted operators. Default `ip`
mode retains the exact-IP rule and requires verification on the actual CNI.

## Recovery and upgrades

Environment records contain management state. The store accepts record versions
1 and 2, preserves malformed or unsupported records and reports incompatibility.
Initial reconciliation failure prevents RPC startup. During operation, detected
record incompatibility blocks new environments until a successful listing.
Detection follows the reconciliation interval.

API outages can delay resource cleanup. Interrupted commands are not replayed.
Stop Runner scheduling and drain jobs before changing controller or agent versions.
Running-job migration, mixed-version controllers and automatic schema downgrade
are unsupported. Preserve records and use the [upgrade procedure](INSTALL.md).

[VALIDATION.md](VALIDATION.md) documents test coverage and target-cluster checks.
