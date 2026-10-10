# Implementation step 18 — aggregate namespace budget

**Status:** configuration, manifest generation, startup drift check and local
tests implemented. The quota was installed on local K3s on 2026-10-09;
concurrency and missing-resource behavior have not yet been measured.

## Operator budget

`kubernetes.quota` is required in the strict YAML config. It defines maximum
counts for Pods, ConfigMaps, Services and NetworkPolicies, plus aggregate CPU,
memory and ephemeral-storage requests and limits. These values are an
administrator-selected budget, not an estimate of node capacity. The example
in [`deploy/config.example.yaml`](../../../deploy/config.example.yaml) is only
a starting point; measure available resources on the target node before use.

Validation rejects zero or inverted budgets and verifies that one complete
environment for every configured profile fits: one job Pod and ConfigMap,
every service Pod and Service, two NetworkPolicies per service, plus the
namespace default-deny policy. For compute, it sums the job and all configured
service requests and limits. This check concerns a single environment; the
quota then limits concurrent aggregate consumption in the namespace.
Cleaned environment records remain as ConfigMap tombstones for 24 hours, so
`maxConfigMaps` must cover expected daily job starts plus concurrently active
environments and headroom. The example uses 1024; this is not a measured
throughput recommendation.

`spawnpod render-admission` now emits the `ResourceQuota` after the admission
policy, binding and `LimitRange`. It uses `count/pods`, `count/configmaps`,
`count/services`, `count/networkpolicies.networking.k8s.io`, and the
`requests.*`/`limits.*` CPU, memory and ephemeral-storage keys. The controller
has named `get` permission and fails preflight if the installed hard limits
differ from the current config. It cannot edit the quota. Kubernetes documents
[object-count syntax and aggregate resource behavior](https://kubernetes.io/docs/concepts/policy/resource-quotas/).

## Cluster verification

Check that a job without services fits, a job with the maximum configured
services fits, and an additional job is rejected at the expected Pod, object
or compute threshold. Then remove environments and verify quota usage falls
back. Confirm that Pods missing ephemeral-storage requests and limits receive
the generated LimitRange defaults and are then counted or rejected by quota;
Kubernetes quota alone may ignore missing ephemeral-storage fields. Test ImagePullBackOff, terminated Pods
and interrupted cleanup as they may hold object-count quota until deletion.

The quota covers the dedicated `ci` namespace, including any unrelated
objects placed there. It does not reserve capacity for `prod` or prove node
scheduling headroom. Production sizing needs observed K3s allocatable CPU,
memory, disk pressure and peak CI concurrency. `kubectl` and `k3s` were not
available in the local development environment for this step.
