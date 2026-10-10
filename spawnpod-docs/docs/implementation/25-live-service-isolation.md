# Implementation step 25 — approved service on local K3s

**Date:** 2026-10-09. The controller used ServiceAccount impersonation in the
same single-node K3s cluster as the previous smoke tests.

## Local profile and policy

[`enable-local-service.py`](../../../deploy/smoke/enable-local-service.py)
adds one service named `echo` to ignored `.local/config.yaml`. It uses the
already approved Alpine digest, numeric UID/GID `10001`, a fixed TCP port
`8080`, and an operator-owned BusyBox `nc` command. The workflow cannot
choose another command, image, user, port or resource limit. The script
enables the service gate only in this local smoke config; the tracked
`deploy/config.example.yaml` still has `enableServices: false`.

The local ResourceQuota allows three Pods, one ClusterIP Service and three
NetworkPolicies. That covers two job Pods, one service Pod, the baseline
default deny and the service's ingress/egress pair. CPU, memory and ephemeral
storage budgets were increased to fit this simultaneous test. After changing
the config, we regenerated admission, applied it, and `doctor --as-controller`
passed before restarting the controller.

```sh
python3 deploy/smoke/enable-local-service.py --config .local/config.yaml
.local/bin/spawnpod validate --config .local/config.yaml
.local/bin/spawnpod render-admission --config .local/config.yaml > .local/admission.yaml
KUBECONFIG=.local/kubeconfig kubectl apply -f .local/admission.yaml
.local/bin/spawnpod doctor --as-controller --config .local/config.yaml
# Restart serve --as-controller to load the updated config.
```

## Runner workflow

[`workflow-service.yml`](../../../deploy/smoke/workflow-service.yml) requests
the approved `echo` image and port, then sends a marker through its hostname
from the job Pod and checks the echoed response. Forgejo run 24 finished
`success`. The runner used the SpawnPod plugin; service startup, ClusterIP
assignment, host alias, agent execution and both NetworkPolicies were
exercised. After `Remove`, `ci` held zero Pods and Services and only the
baseline default-deny policy. The cleanup preview had no candidates.

## Two simultaneous environments

The [recovery RPC client](../../../deploy/smoke/recovery-client.go) created a
service environment `565a164fdbfe73cd3e27b45b47a80a67` and a separate
job environment `36e1a0b562a79424730361ad23566994`. The service had
ClusterIP `10.43.163.214`. Its own job Pod received the expected echo both
before and after the second environment's probe. The second job Pod's TCP
connection to that ClusterIP was denied. Both environments were removed;
the final quota showed zero Pods and Services and one baseline policy.

This verifies effective isolation between the two tested environments. The
second job Pod also had default-deny egress, so this probe alone does not
isolate the service ingress rule from the job egress rule. Further tests
should exercise isolated ingress and egress rules, cancellation and cleanup
on controller restart. Service startup failure and partial resource creation
are covered in [step 26](26-live-service-failure-recovery.md).

`prepare-local.sh` now builds the recovery RPC client. For the simultaneous
test, use separate `-id-file` paths for the two environments, `create-service`
for the first, `create` for the second, `service-echo` for the first, and
`probe -target SERVICE_CLUSTER_IP` for the second. Run `remove` for both IDs
even if a probe fails.
