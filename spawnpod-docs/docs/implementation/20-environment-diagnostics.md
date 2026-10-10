# Implementation step 20 — environment diagnostics

**Status:** read-only CLI commands and local tests implemented; no live K3s
run yet.

## Commands

```sh
spawnpod environments list --config CONFIG
spawnpod environments inspect --config CONFIG ENVIRONMENT_ID
spawnpod environments cleanup --config CONFIG --dry-run
```

All three commands use the configured kubeconfig or in-cluster identity and
read only spawnPod-owned ConfigMap records in the configured workload
namespace. `list` emits a JSON array, `inspect` emits one JSON record, and
`cleanup --dry-run` emits a JSON object with a note and candidate actions.
The record stores ownership, state, deadline and Kubernetes handles. Workflow
secret values are excluded by the record contract. A malformed owned record
causes an error rather than being silently skipped.

The preview and controller reconciliation call the same pure decision function:
expired or interrupted records are removal candidates; cleaned records become
tombstone expiration candidates after 24 hours. The preview is a snapshot of
stored records. It cannot see an in-flight operation held by a running
controller, which may suppress a listed action. It never deletes resources.

Use `doctor` before the first job, then inspect records after each test run.
For incomplete cleanup, compare the record's UIDs with the current Pod,
Service and NetworkPolicy UIDs. Reconciliation performs UID and ownership
checks before deletion; a manual destructive cleanup command is deferred
until the cluster behavior has been observed.
