# Declarative installation upgrades (rc.6)

These commands are available in rc.6 and are absent from rc.5 binaries. `plan` and `check` never apply Kubernetes resources;
`render` writes local files only. Imperative apply, automatic rollback and a
cluster phase/status store are not implemented.

## One source configuration, one bundle

Use the target release's CLI binary. `--target` must match that binary's version;
`dev` builds may use a target name for local testing. A release name does not
verify an image's provenance: verify the release's signatures/attestations and
use its published immutable runtime digest. Controller and Agent share `--image`.
Runner remains separately pinned; the existing `spawnpod-runner` Secret is reused.

```sh
spawnpod upgrade plan --config config.yaml --target v0.1.0-rc.6 \
  --image "$RUNTIME_IMAGE" --runner-image "$RUNNER_IMAGE"

# Add --kubeconfig for read-only discovery of mounted Agent, Controller and Pods.
spawnpod upgrade plan --config config.yaml --target v0.1.0-rc.6 \
  --image "$RUNTIME_IMAGE" --runner-image "$RUNNER_IMAGE" \
  --kubeconfig "$KUBECONFIG"

spawnpod upgrade render --config config.yaml --target v0.1.0-rc.6 \
  --image "$RUNTIME_IMAGE" --runner-image "$RUNNER_IMAGE" \
  --output-dir k3s/spawnpod-next

# Preview only: exit nonzero if managed files would change; writes nothing.
spawnpod upgrade render --config config.yaml --target v0.1.0-rc.6 \
  --image "$RUNTIME_IMAGE" --runner-image "$RUNNER_IMAGE" \
  --output-dir k3s/spawnpod-next --check
```

`RUNTIME_IMAGE` and `RUNNER_IMAGE` must contain `@sha256:<64 hexadecimal digits>`.
Do not use the historical diagnostics archives as either image provenance or a
complete previous installation backup.

Generated files:

| File | Contents | Phase |
| --- | --- | --- |
| `00-security.yaml` | namespaces, ServiceAccounts, RBAC | dependencies |
| `10-network.yaml` | namespace default deny | dependencies |
| `20-admission.yaml` | policy, binding, LimitRange, quota, optional source egress | policy |
| `30-deployment.yaml` | mounted config, Controller/Runner Deployment, monitor | switch |
| `spawnpod-bundle.json` | version/schema, revision, hashes, inventory, images, gates | operator metadata; **do not kubectl apply** |

The JSON revision hashes the deterministic inventory; it is not an authenticity
signature. Files have no timestamps or random values. The inventory is written
last using individual file renames; filesystem writes are not a multi-file
transaction. Retry after interruption rechecks the previous inventory and files.
Render will reject edited managed files and nonregular files/symlinks. Keep hand
maintained additions in other files. Use a new output directory when importing an
existing hand maintained installation. `--check` previews conflicts and changes
before writes. Preserve complete old and new directories in GitOps.

Workload and Controller namespaces must be separate. Custom namespace names are
supported, but admission policy/binding names are cluster-wide: this workflow
supports one coordinated SpawnPod installation per cluster. The drain check
looks for managed Pods across namespaces. Namespace/Lease migration is outside
the upgrade plan's scope.

The source YAML is never rewritten. Generated configuration sets host kubeconfig
to empty (in-cluster credentials), socket to
`/run/spawnpod/private/backend.sock`, and monitor to `:9090`. Durations, resource
quantities, network and retention defaults are normalized. These transformations
are shown in the plan and reused by mounted config checks.

## Upgrade gates: drain, stop, policy, switch

rc.6 P0 uses a drained upgrade with a maintenance interval. It does not expand
admission to two Agent generations. rc.4/rc.5 reject expanded admission; changing
policy under a running old Controller is unsafe.

1. Run the **current** binary's `doctor --installation` using the current source
   configuration. Identify the actual mounted configuration, runtime digest,
   release binary and GitOps revision. Confirm that the previous complete bundle
   can be restored, including its RBAC, network, admission, quotas, config and
   Deployment. Diagnostics and an empty Git diff do not establish that backup.
2. Pause Forgejo submissions. Wait for existing jobs and services to finish and
   for the current Controller to clean up all managed Pods. Do not delete active
   jobs or tombstones to make the gate pass. Keep Controller running for cleanup.
3. Suspend the GitOps Deployment reconciliation; then scale the current Deployment
   to zero. Wait for every Controller/Runner Pod to be deleted. Keep the Runner
   Secret and namespace contents intact.
4. Apply `00-security.yaml` and `10-network.yaml`, then `20-admission.yaml`. Use
   separate commits/sync gates when the reconciler cannot guarantee ordering.
   Do not apply the entire directory before the stop gate.
5. Verify the target bundle and live prerequisites:

   ```sh
   spawnpod upgrade check --config config.yaml --target v0.1.0-rc.6 \
     --image "$RUNTIME_IMAGE" --runner-image "$RUNNER_IMAGE" \
     --manifests k3s/spawnpod-next --kubeconfig "$KUBECONFIG" --phase switch
   ```

   This requires an operator kubeconfig with impersonation rights for
   `system:serviceaccount:<leaseNamespace>:spawnpod-controller`. Startup and RBAC
   checks use that identity; inventory, Secret key, node/image and installation
   quota reads use operator permissions. Controller RBAC is not broadened.
6. Apply `30-deployment.yaml`. Resume GitOps against the complete target revision,
   retaining the generated replica count of one. ConfigMap and Deployment updates
   are not atomic; failures require recovery below.
7. Wait for rollout, then repeat `upgrade check` with `--phase ready` (the default).
   It requires the observed generation, one updated available replica, both
   containers Ready and Running, target images, mounted config, security baseline
   and managed inventory. A Pod `1/2` or CrashLoop is failure.
8. Run a real Forgejo workflow with `runs-on: spawnpod` (example:
   `deploy/release/workflow-smoke.yml`). Confirm successful execution and cleanup.
   Resume submissions only after this passes. The read-only JSON report always
   sets `ciProven: false`: it does not contact Forgejo or certify registration.

The full dependency graph and recovery instructions are also in the bundle index.
The operator/GitOps revision is the phase record. There is no hidden CLI apply,
pruning, deletion or automatic rollback.

### Images and capacity

`upgrade check` inspects Ready schedulable nodes and the workload architecture.
Node-local images must appear by exact pinned reference in `Node.status.images`
on every candidate node of that architecture; absence blocks the check. Node
image lists may be truncated, so confirm imports directly in containerd when the
API cannot establish presence. Remote images missing from the node cache produce
explicit warnings: this does **not** verify registry reachability, authentication
or the OCI platform index. Verify/pre-pull each pinned image on eligible nodes
before switching. The command does not claim image provenance or platform
verification merely from a digest string. Pod scheduling and real execution are
additional required gates. Image checking covers Ready schedulable nodes with tolerable taints matching
the profile architecture.

Installation quota diagnostics show requested/used/hard/available and available
**after Recreate**. Only nonterminal Pods owned through ReplicaSets of this
Deployment receive retirement credit. Insufficient space for a second copy is a
warning when the new Pod fits after retirement. Insufficient capacity after
retirement is a blocker. Wait for quota status to catch up with Pod deletion.
CPU/memory limits cannot be recovered by deleting ConfigMap tombstones.

## Failed or interrupted upgrade

Keep submissions paused. Read the failed check's `phase`, diagnostic code and
field; inspect the actual Controller's current and previous logs. Record binary
version, image identity, mounted configuration hash and API observation time.
Do not conclude that host `doctor` and Controller saw the same configuration,
credentials, binary or API state merely because both call `Preflight`.

- **Before stop/policy:** keep the old installation running; resolve prerequisites.
- **After stop, before switch:** finish the verified target dependency/policy gates
  and repeat `--phase switch`, or restore the previous entire bundle.
- **After switch/CrashLoop:** stop new submissions and Deployment reconciliation;
  finish cleanup with a compatible Controller where possible. If active jobs
  cannot be drained, stop recovery and diagnose; do not delete their Pods. Stop
  Controller, restore old security/network/admission/limits/quota first, then old
  ConfigMap/Deployment/Service. Restore old admission before starting old Agent.
  Verify readiness and a real Forgejo job with the previous binary/config.
- **Lost CLI process:** no cluster mutations originate in plan/check/render. Inspect
  GitOps/live state and repeat the gate for the actual phase. Never assume a timed
  out apply succeeded. Rerender into a new directory if manual edits conflict.

Do not use Deployment-only `rollout undo`. Do not delete namespaces, Runner
Secret, records or artifacts. Back up manifests/metadata privately; the bundle
contains only a Secret reference, never Runner credentials. Historical incident
archives are evidence, not a coherent rollback revision.

## rc.5 incident evidence and release gates

The supplied historical snapshots contain a healthy later Controller after
multiple restarts. Their validations match rc.5 output exactly, and their API
match defaults are already accepted by rc.5. They do not include the previous
failing startup log. The historical root cause remains **unproved**; we do not
attribute it to `reflect.DeepEqual` or claim to have reproduced that exact failure.

Startup and CLI doctor report version, architecture, executable SHA256 and
normalized configuration SHA256. Mounted configuration mismatches also report
expected/actual hashes. These help compare the exact host/container observations
without exposing configuration values or credentials.

Current regression tests cover stale Agent policy with the exact failed field,
shared doctor/startup diagnostics and real API round trips. Diagnostics hash full
specs and report a field path without printing CEL/config/Secret values. Entire
admission specs are compared, including validation reasons/message expressions,
variables and audit annotations; only explicit Kubernetes match defaults are
normalized.

Release acceptance must include a target-server upgrade/recovery and a real
Forgejo smoke job. Local host Controller acceptance is not a substitute for a
complete containerized Controller/Runner installation. See `docs/VALIDATION.md`.
