# Implementation step 04 — controller process

**Status:** strict configuration, Kubernetes client wiring, startup API checks,
Lease election and periodic recovery implemented. This process does not yet
accept CI requests. Cluster deployment remains unverified.

## Packages and entry point

- `internal/config` owns the versioned YAML contract. It rejects unknown keys,
  extra YAML documents, invalid DNS labels, unpinned images, root UID/GID,
  nonpositive resources, requests above limits, and durations outside bounds.
  Each profile is rendered through `BuildJobPod` during validation, so the
  profile and Pod builder share the same security checks. The configuration
  is operator owned and contains no workflow substitutions.
- `internal/controller` checks access to job ConfigMaps and Pods and reads the
  Lease before election. It creates one engine from the ConfigMap store and
  Kubernetes provider. The Lease holder runs `ReconcileOnce` immediately and
  then at the configured interval; each sweep uses `cleanupTimeout` as a
  deadline. Errors are logged and retried on the next tick.
- `cmd/spawnpod` has `validate`, `serve` and `version` commands. `serve` uses
  in cluster credentials unless `kubernetes.kubeconfig` is set. SIGINT/SIGTERM
  cancel the leader election and reconcile loop.

## Start sequence

1. Parse and validate `--config` without contacting Kubernetes.
2. Load the service account or explicit kubeconfig and construct client-go.
3. Confirm `list` on ConfigMaps and Pods in the job namespace, `list` on
   Leases in the Lease namespace, and `get` on the named Lease. A missing
   Lease is expected on first boot.
4. Compete for the Lease with a per-process random identity. Only its holder
   runs recovery. If leadership is lost, `serve` exits with an error so the
   process manager starts a fresh instance. A graceful shutdown does not
   release the Lease early; another process can take over after expiry.
5. Reconcile durable environment records. Interrupted executions are cleaned
   without replaying commands; expired records and tombstones are processed
   according to the engine rules in step 03.

## Configuration and run commands

`deploy/config.example.yaml` is syntactically valid but uses **placeholder**
image digests. Replace them with approved built images before deployment.

```sh
go run ./cmd/spawnpod validate --config deploy/config.example.yaml
go run ./cmd/spawnpod serve --config deploy/config.example.yaml
```

The controller service account needs `list`, `get`, `create`, `update`, and
`delete` on ConfigMaps and Pods in the job namespace, `create` on `pods/exec`,
and `list`, `get`, `create`, `update` on Leases in the Lease namespace. Exact RBAC and
admission behavior require a cluster test; this is a permission outline, not
deployment manifests.

## Verification and remaining work

Unit tests cover strict config parsing, the sample configuration, immediate
and repeating reconciliation, cancellation, and API preflight failure. Run
`go test ./...` and `go vet ./...` after changes. The Forgejo RPC mapping and
file transfer were added in steps 05 and 06; its local listener was added in
step 07. Before release, test leader handoff,
recovery, admission, and Pod cleanup on the intended K3s cluster. Orphan Pod
scanning is still absent, as described in step 03.
