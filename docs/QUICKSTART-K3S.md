# Forgejo + K3s quickstart

This is the reference `infra` (controller/Runner) and `ci` (workloads)
installation. Use an existing K3s cluster with enforcing NetworkPolicy and
stable ValidatingAdmissionPolicy. Follow [package verification and image
preparation](INSTALL.md) first. Use official Forgejo Runner 13.1.0.

## 1. Prepare configuration and images

Copy `deploy/config.example.yaml` to private `operator.yaml`. Replace the
placeholder image digests with published manifest digests. The job image
needs `/bin/sh` and the tools used by steps, must work as UID/GID 10001, and
must support a read-only root filesystem. Select the node architecture.
Keep services disabled and the default profile isolated for this first test.

For a small `count/configmaps: 32` quota, consider
`controller.tombstoneRetention: 10m`. The default is `24h`; valid values are
`1m` through `24h`. Budget records for active jobs plus jobs completed during
retention, and leave headroom for reconciliation/API delays. For example,
two jobs per minute with ten-minute retention consume about twenty cleaned
records, plus active jobs. This estimate is not a concurrency guarantee.

```sh
./spawnpod validate --config operator.yaml
./spawnpod render-admission --config operator.yaml > admission.yaml
./spawnpod render-deployment --config operator.yaml \
  --controller-image registry.example.com/spawnpod@sha256:YOUR_RUNTIME_DIGEST \
  --runner-image data.forgejo.org/forgejo/runner@sha256:YOUR_RUNNER_DIGEST \
  > installation.yaml
```

Use complete 64-character lowercase digests. Validation prints
`configuration valid`. Rendered YAML includes resource totals as comments.
Review it before applying. Namespace/RBAC and the default-deny manifest are
maintained in `deploy/`; admission and installation YAML are generated.

## 2. Install the security baseline

```sh
kubectl apply -f deploy/security.yaml
kubectl apply -f admission.yaml
kubectl apply -f deploy/network-default-deny.yaml
kubectl -n ci get resourcequota,limitrange,networkpolicy
```

Expected: `spawnpod-namespace-budget` quota, `spawnpod-container-ceilings`
LimitRange and `spawnpod-default-deny`.
The default-deny policy selects every Pod in `ci` and denies both directions.
The original `spawnpod.dev/managed: "true"` selector is also accepted at
controller startup. Extra selector requirements or allow rules are rejected.
Policies are additive: this check cannot establish that another policy has
not granted access. Verify packet isolation separately using the existing
[acceptance suite](../tests/integration/README.md).

Do not remove admission or namespace security labels to bypass a failed
preflight. Regenerate admission after changing images, profiles or budgets.
For rollback restore the matching previous config, admission and installation
manifests together. Keep the namespaces and durable records until jobs drain.

## 3. Register Runner and create its Secret

Register the official Runner with your Forgejo instance using its interactive
registration flow. Keep the generated UUID and shared secret private. Copy
`deploy/release/runner.example.yaml` to a private location outside the repo,
with file mode 0600, and fill `server.connections.forgejo.url`, `uuid`, and
`token`. The `spawnpod` label image must equal the default profile's image.

For example, prepare a private temporary file with no credentials in shell
arguments or history:

```sh
umask 077
SPAWNPOD_RUNNER_DIR=$(mktemp -d)
install -m 600 deploy/release/runner.example.yaml "$SPAWNPOD_RUNNER_DIR/runner.yaml"
${EDITOR:-vi} "$SPAWNPOD_RUNNER_DIR/runner.yaml"
kubectl -n infra create secret generic spawnpod-runner \
  --from-file=runner.yaml="$SPAWNPOD_RUNNER_DIR/runner.yaml"
```

Contract: Secret **`infra/spawnpod-runner`**, nonempty key **`runner.yaml`**,
mounted only in Runner at **`/etc/runner/runner.yaml`**. A `token` key alone is
insufficient. Kubernetes now refuses the volume mount when `runner.yaml` is
missing. Do not print the Secret or commit the private file. Remove the
temporary private directory after the Secret is created, according to your
credential handling policy; editors may also create backup files.

To rotate, quiesce Runner scheduling, replace the Secret from the new private
file, then restart the Deployment. Revoke the old Runner registration in
Forgejo. Remove disposable test Runner registrations when finished.

## 4. Check resources before rollout

The reference Deployment uses two containers. Each requests `100m` CPU and
`128Mi` RAM and limits `1` CPU and `512Mi` RAM. The Pod totals are `200m`,
`256Mi` requests and `2`, `1Gi` limits. It uses `Recreate`, without a surge
replica. ResourceQuota includes existing namespace usage; CPU **limits** of
`2100m` plus `2000m` require at least `4100m`, even if actual CPU usage is low.
Requests affect placement; limits cap usage; quota is a namespace admission
budget. None of these proves that a node currently has free capacity.

Apply only the generated ConfigMap first (first YAML document), so doctor can
compare the mounted configuration without starting the Deployment:

```sh
kubectl apply --dry-run=client -f installation.yaml
sed '/^---$/,$d' installation.yaml | kubectl apply -f -
```

Set `kubernetes.kubeconfig` in a local copy of operator.yaml to your admin
kubeconfig path; the renderer omits it from the in-cluster configuration.
Run the controller identity check and the operator installation check:

```sh
./spawnpod doctor --config operator.yaml --as-controller --strict
./spawnpod doctor --config operator.yaml --installation --strict
```

Doctor reports `OK`, `WARN`, `FAIL` and a summary. Exit 0 means no failures;
exit 1 means a failed check, invalid invocation/config, or, with `--strict`,
any warning. Installation checks need operator permissions for infra
Secret/ConfigMap/quota reads; the controller's Role intentionally lacks Secret
access. These two commands use different identities. `--installation` verifies
the required nonempty Secret key, never prints its data, and checks the
normalized `config.yaml`. It does not validate Runner credentials with Forgejo.

Quota warnings compare a **new complete environment** and a **new installation
Pod** against currently available quota. Already running objects are included
in usage: a pre-upgrade warning can clear after the old Pod is drained and
deleted. Scoped quotas need manual evaluation. Doctor does not reserve quota,
check physical node capacity, or prove image availability. An unobserved usage
value produces a warning; wait for the quota controller before repeating.

## 5. Start and verify

```sh
kubectl apply -f installation.yaml
kubectl -n infra rollout status deployment/spawnpod --timeout=180s
kubectl -n infra logs deployment/spawnpod -c controller --tail=100
kubectl -n infra logs deployment/spawnpod -c runner --tail=100
kubectl -n infra exec deployment/spawnpod -c controller -- \
  /spawnpod doctor --config /etc/spawnpod/config.yaml
```

Expected: Deployment `1/1`, controller acquires Lease and serves the plugin
socket, Runner appears online in Forgejo. A missing `runner.yaml` produces a
volume mount error: recreate the Secret with the required key. Admission drift
requires regenerating matching policy. A quota failure requires waiting for
cleanup or reviewing the budget, not disabling security checks.

## 6. Run a real workflow and check cleanup

Copy `deploy/release/workflow-smoke.yml` to `.forgejo/workflows/spawnpod-smoke.yml`
in a test repository with Actions enabled. Dispatch it in Forgejo. It uses no
checkout action, Node or external network dependency. Watch resources in a
second terminal while it runs:

```sh
kubectl -n ci get pods -l spawnpod.dev/managed=true --watch
kubectl -n infra logs deployment/spawnpod -c controller --follow
```

Check three results independently:

1. Forgejo reports the workflow **successful**, with the expected command output.
2. A CI Pod was created and lifecycle logs include `environment.created`,
   `environment.ready`, `execution.started`, `execution.finished` with
   `exit_code=0`, and `environment.cleanup.completed`, correlated by `environment_id`.
3. The Pod is removed, its record becomes `cleaned`, and the ConfigMap expires
   after `cleaned_at + tombstoneRetention` on a successful reconciliation sweep.

```sh
./spawnpod environments list --config operator.yaml
./spawnpod environments inspect --config operator.yaml ENVIRONMENT_ID
./spawnpod environments cleanup --config operator.yaml --dry-run
kubectl -n ci get configmaps -l spawnpod.dev/managed=true
```

Inspect includes Pod presence, identity match, cleanup decision, and retention
deadline. Preview never mutates objects and cannot see active controller
operations or terminal service loss. Record expiration is safe to retry; API
outages delay it. Do not delete records while their resources may still exist.
Repeat the smoke several times, then exercise a failing step and cancellation.
Confirm no old cleaned records remain beyond retention once the API is healthy.

## Image locality and pull troubleshooting

Job and service builders explicitly use `IfNotPresent`; digest-pinned
installation containers also use the Kubernetes default `IfNotPresent`.
For a single-node offline test, import and tag each exact image reference in
K3s containerd as described in [INSTALL](INSTALL.md). A Podman image on the
host is not automatically a containerd image. `localhost/...@sha256:...`
only works where that reference was imported; another node may try its own
loopback registry. Doctor warns on these references. For multiple nodes,
publish approved images to a reachable registry and use its manifest digest,
or import the exact reference on every eligible node. These checks also apply
to the runtime image used by the agent init container.

For `ImagePullBackOff`, inspect Pod events with `kubectl -n ci describe pod NAME`
and check registry reachability, trust/authentication, exact imported reference
and architecture. SpawnPod job admission forbids `imagePullSecrets`; registry
authentication must be provided through operator-managed node/runtime setup.
Changing generated manifests by hand can violate admission; change the
generator/configuration and rerender when introducing a new supported behavior.
