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

## Containerized bundle upgrade and real Forgejo recovery

`BundleUpgradeAndRecovery` additionally requires:

```sh
export SPAWNPOD_ACCEPTANCE_PREVIOUS_IMAGE=localhost/spawnpod@sha256:OLD_RUNTIME_DIGEST
export SPAWNPOD_ACCEPTANCE_RUNNER_IMAGE=data.forgejo.org/forgejo/runner@sha256:RUNNER_DIGEST
export SPAWNPOD_ACCEPTANCE_RUNNER_CONFIG=/PRIVATE/PATH/runner.yaml
export SPAWNPOD_ACCEPTANCE_FORGEJO_TOKEN=/PRIVATE/PATH/forgejo-api-token
```

The main acceptance config's Agent image must be the imported current runtime
(Controller and Agent share that image). The previous image must match the
previous release binary. The Runner configuration must register a dedicated
local test Runner, use `spawnpod:spawnpod://<profile image>` and the mounted
socket `/run/spawnpod/private/backend.sock`. Match Runner timeout to profile
`maxLifetime`. Forgejo must be the local test instance at `127.0.0.1:3000`; its
URL in Runner config must also be reachable from the test Pod. The optional
`forgejo-local-proxy.py` forwards local Forgejo to the K3s node address and
accepts only that node and its pod CIDR. Stop the proxy after tests.

Run only the container scenario when its requirements are configured:

```sh
go test -v -count=1 -timeout=20m ./tests/integration/acceptance \
  -run 'TestLocalClusterAcceptance/BundleUpgradeAndRecovery$'
```

It starts rc.6 from that binary's actual rendered bundle, runs a real Forgejo workflow, drains/stops, switches to target,
runs another workflow, introduces an intentional Controller startup failure,
restores the entire previous set, runs a workflow, and repeats the target update
and workflow. Secret identity/resourceVersion/data and old records are checked.
Credentials and manifests stay in the private evidence directory; never upload
that entire directory as a CI artifact. Dedicated local Runner registration and
API token should be removed after manual tests.

`upgrade-ci.sh` automates setup only on a disposable GitHub-hosted runner; it is
not a workstation/server installer. The release workflow uses it as a gate.

## RC7 network gate

`source-network.py` needs Python/PyYAML, kubectl, a loopback local K3s API, the
current CLI, and digest-pinned runtime and Node/CA job images. It creates three
unique Restricted namespaces, runs the actual source proxy Deployment and
rendered job NetworkPolicies, and removes its owned namespaces even on failure.
It never changes cluster-wide admission and never exports Secrets or kubeconfig.
These probe Pods do not establish Runner/Agent acceptance.

```sh
python3 tests/integration/source-network.py \
  --kubeconfig /PRIVATE/PATH/kubeconfig \
  --spawnpod-binary /PATH/spawnpod \
  --runtime-image 'REGISTRY/spawnpod@sha256:RUNTIME_DIGEST' \
  --job-image 'REGISTRY/node-job@sha256:JOB_DIGEST' \
  --upstream-host git.dmedovich.com --upstream-ip VERIFIED_PUBLIC_IPV4 \
  --evidence-dir /PRIVATE/NEW/DIRECTORY
```

The upstream must serve a CA-trusted HTTPS Forgejo `/api/v1/version` response.
The default test mode is `pod-selector`; `--policy-mode ip` explicitly tests the
legacy IPBlock contract. `--diagnose-dnat` may add a temporary exact backend-IP
rule after a failure, but always fails that run; diagnostic success cannot pass
acceptance. Selector mode also replaces the proxy Pod and verifies the unchanged
Service address. Review `summary.json`, `results.json`, `proxy-replacement.json`
and `cleanup.json`. This gate rejects non-loopback APIs; the target-server checks
in the runbook must be performed separately.

`build-preview-runtime.sh` builds three static binaries and the release scratch
image. `oci-preview-registry.py` serves one verified OCI archive read-only on
loopback for a single-node K3s pull. These are local preview helpers, not a
release publication or a production registry. Stop the registry after tests.

`SourceRunnerProfiles` is a separate explicit opt-in acceptance subtest. Configure
isolated `default`, a `source` profile, services disabled, a free reserved Service
IP on port 3128, and `sourceProxyPolicy: pod-selector`. Supply the same Runner
image/config/token variables as bundle acceptance, but use both plugin aliases
from `deploy/release/runner-source.example.yaml`. Also set
`SPAWNPOD_SOURCE_UPSTREAM_HOST` and `SPAWNPOD_SOURCE_UPSTREAM_IP` to an explicitly
verified public Forgejo route. Set `SPAWNPOD_SOURCE_FORGEJO_ORIGIN` to
`http://<literal-local-node-InternalIP>:3001/`, served by the existing local
Forgejo forwarding fixture. The Node/Git/CA job image must be pinned and cached.
Run:

```sh
go test -v -count=1 -timeout=15m ./tests/integration/acceptance \
  -run 'TestLocalClusterAcceptance/SourceRunnerProfiles$'
```

This uses real Controller, Agent and official Runner containers, dispatches both
labels, checks effective proxy env and positive/negative network behavior inside
jobs, and verifies persisted selected profiles. A test-only HTTPS sidecar on
proxy loopback supplies a disposable CA and forwards to the local private Forgejo;
only this fixture receives an extra exact node-IP:3001 proxy egress rule. Production
public-route isolation is established separately by `source-network.py`.
The Runner executes pinned `actions/checkout`, checks commit/origin/shallow history,
and pushes/pulls a small ready-made OCI artifact with verified blob/manifest digests.
A separate disposable user owns the `write:package` token, which is delivered only
to that step through a unique Actions secret. Package, secret, token and user are
removed afterwards, and their absence is checked. The test then starts another
source workflow, waits for a persisted executing command, requests cancellation
through Forgejo v15's origin-protected web UI, and checks the cleaned record and
absent job Pod. A temporary repository writer is used for that UI action and
removed afterwards. On timeout/interruption the helper requests cancellation
before credential cleanup. No credential enters a rendered bundle or generated workflow.
It temporarily changes the global
admission policy and restores it during parent cleanup, like other acceptance
tests. Runner logs and registration remain private; remove dedicated temporary
Runner/API credentials after manual validation.
