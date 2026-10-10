# Implementation step 19 — cluster doctor

**Status:** implemented and locally tested. No K3s installation or live
scenarios have been run yet.

## Purpose and command

`spawnpod doctor --config PATH` runs after an operator applies
`deploy/security.yaml`, `deploy/network-default-deny.yaml` and the output of
`spawnpod render-admission`. It uses the same kubeconfig or in-cluster identity
as `serve`. The command reads the installed objects and makes
SelfSubjectAccessReview requests; it does not create a job or change the
workload baseline. It has a 30-second overall timeout.

Checks are reported individually as `OK` or `FAIL`, and the process returns a
nonzero status if any check fails:

1. Existing controller preflight: API reads, exact generated policy/binding,
   LimitRange and ResourceQuota specs, and exact default-deny NetworkPolicy.
2. `ci` namespace label `spawnpod.dev/role=workload` and pinned Restricted
   Pod Security labels for enforce, audit and warn.
3. `spawnpod-job` ServiceAccount exists with token automount explicitly false.
4. The current ValidatingAdmissionPolicy generation has completed CEL type
   checking and has no expression warnings.
5. The ResourceQuota controller has observed every configured hard limit.
6. The controller identity is authorized for each Pod, ConfigMap, Service,
   NetworkPolicy, Lease and named security-object operation used by the tool.
   These checks include `pods/exec` creation.

The reference RBAC manifest grants the controller named read access to the
workload namespace and job ServiceAccount so this diagnostic can run under the
controller identity. A failure here should be fixed in the manifest or cluster
installation before `serve` is started.

## Limits and first live scenarios

SelfSubjectAccessReview checks authorization but does not exercise admission
or create a Pod. The doctor verifies the default-deny object exists but cannot
prove that the installed CNI enforces it. Once K3s is installed on the current
machine and the tool is complete, run live negative tests for admission,
NetworkPolicy isolation and quota, then one minimal Forgejo Runner job without
services and one with a service. Observe scheduling, image pulls, execution,
cleanup and resource usage. Keep the first runs small enough for the measured
single-node headroom.
