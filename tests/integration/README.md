# Integration tests

These fixtures exercise SpawnPod with local K3s and the official Forgejo Runner.
They are separate from the installation manifests in `deploy/`.

| Files | Purpose |
|---|---|
| `acceptance/cluster_test.go`, `acceptance.config.yaml` | Admission, networking, recovery, ConfigMap retention, drained upgrades and resource measurements |
| `workflow*.yml`, `file-transfer-*` | Runner workflow and file-transfer scenarios |
| `agent.Dockerfile`, `checkout-job.Dockerfile` | Test agent and job images |
| `prepare-local.sh`, `install-host-k3s.sh` | Local build and cluster setup helpers |
| `forgejo-*`, `bootstrap-forgejo.py`, `configure-local-runner.py` | Local Forgejo configuration, TLS and workflow helpers |
| `check-*.py`, `make-*.py`, `enable-local-service.py`, `service-quota-fail.yaml` | Policy checks and failure injection |
| `source-proxy.local.example.yaml`, `spawnpod-source-proxy.service.example` | Local source-proxy examples |
| `recovery-client.go` | RPC client for restart and recovery scenarios |

`prepare-local.sh` can also build Runner from a checkout supplied through
`SPAWNPOD_RUNNER_SOURCE`. The proxy unit is a user-service example; its binary
and config paths must match the test installation. It is not installed by the helpers.

## Run K3s acceptance

Requirements: a disposable local K3s installation with the SpawnPod admission
policy, admin kubeconfig, `kubectl`, metrics-server, Go, rc.1 and rc.2 controller
binaries, and imported digest-pinned runtime and job images. Build the job
image with `checkout-job.Dockerfile`; the suite uses Node, Git and BusyBox.
Stop other SpawnPod controllers and jobs before running.

From the repository root:

```sh
export SPAWNPOD_TEST_DIR=$(mktemp -d)
install -m 600 tests/integration/acceptance.config.yaml "$SPAWNPOD_TEST_DIR/config.yaml"
```

Edit the config: use unique `sp-acceptance-*` namespaces, absolute kubeconfig
and socket paths, and the imported image digests. Keep the kubeconfig mode 0600.
Set the binary paths, validate the config and run:

```sh
export SPAWNPOD_ACCEPTANCE_CONFIG="$SPAWNPOD_TEST_DIR/config.yaml"
export SPAWNPOD_CONTROLLER_CURRENT=/ABSOLUTE/PATH/TO/rc.2/spawnpod
export SPAWNPOD_CONTROLLER_PREVIOUS=/ABSOLUTE/PATH/TO/rc.1/spawnpod
"$SPAWNPOD_CONTROLLER_CURRENT" validate --config "$SPAWNPOD_ACCEPTANCE_CONFIG"
go test -v -count=1 -timeout=15m ./tests/integration/acceptance
```

The suite temporarily replaces the cluster-wide SpawnPod admission policy.
It refuses non-loopback APIs, existing test namespaces and active SpawnPod
workloads. It saves `original-admission.json` beside the config, creates test
namespaces and RBAC, then restores the policy during cleanup.

If the process or machine is killed, inspect its test resources and restore
the saved policy spec manually, preserving the current UID/resource version.
Existing `ci` records are excluded from the test. Ordinary `go test` skips
this suite unless `SPAWNPOD_ACCEPTANCE_CONFIG` is set.

Logs, controller output and `capacity.txt` remain in the temporary directory.
Review them before sharing. Results and test limitations are recorded in
[VALIDATION.md](../../docs/VALIDATION.md).

`ConfigMapRetention` checks real Pod removal and resourceVersion-protected
ConfigMap expiration after exit codes 0 and 7. It backdates only the test
tombstone's `cleaned_at` and reconciles with a fresh Engine and one-minute TTL;
it does not wait one real minute. The upgrade/rollback scenario switches the
test default-deny selector while drained: managed Pods for older controllers,
namespace-wide for the current controller. Configs read by older binaries must
omit `tombstoneRetention`, which they do not recognize.
