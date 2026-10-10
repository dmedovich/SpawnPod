# Implementation step 16 — Kubernetes security manifests

**Status:** reference manifests and local structural checks are in the
repository. They have not been applied to K3s or validated by the API server.
Keep `controller.enableServices: false` until the release gates below pass.

## Layout and access

[`deploy/security.yaml`](../../../deploy/security.yaml) declares `infra` and
`ci`, a controller ServiceAccount in `infra`, and a tokenless job ServiceAccount
in `ci`. The example config now stores the controller Lease in `infra`.

The `ci` Role grants only operations currently called by the provider:
ConfigMap get/list/create/update/delete; Pod get/list/create/delete;
`pods/exec` create; Service get/create/delete; NetworkPolicy get/create/delete;
named LimitRange get.
The `infra` Role grants Lease list/create plus named get/update for
`spawnpod-controller`. Create cannot be restricted by `resourceNames`, so this
is the narrowest Role shape available without precreating the Lease and
removing create permission. The runner and job ServiceAccount receive no Role.

`ci` pins Pod Security `restricted` to v1.34. Match this version to the actual
K3s release before applying; an unsupported pin must fail deployment review.
The additional ValidatingAdmissionPolicy and its `Deny` binding are generated
from the operator config in [step 17](17-generated-admission.md). Kubernetes
requires both a policy and binding for enforcement; merely applying the policy
object is insufficient. See the [official admission policy documentation](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/)
and [namespace Pod Security labels](https://kubernetes.io/docs/tasks/configure-pod-container/enforce-standards-namespace-labels/).

## Installation and verification order

1. Confirm the actual K3s API and Pod Security version. Apply the namespace,
   ServiceAccounts, Roles and bindings in `security.yaml`. Generate and apply
   the admission policy from the same config the controller will use.
2. Apply [`deploy/network-default-deny.yaml`](../../../deploy/network-default-deny.yaml)
   to `ci`. Do this before the first job or service Pod.
3. Give the controller Pod the `infra/spawnpod-controller` ServiceAccount.
   Its only ClusterRole grants named read access to the generated admission
   policy and binding. Verify `kubectl auth can-i` for each documented
   operation, including `pods/exec` create and `networkpolicies` create/delete.
4. Use server-side dry runs to reject privileged Pods, hostPath and secret
   volumes, PodSpec environment, host ports, arbitrary ServiceAccounts,
   mutable image tags and unbounded containers. Verify that the exact Pods
   built by spawnPod are admitted. Inspect admission policy type-checking
   warnings and the policy binding's scope.
5. Run NetworkPolicy negative and positive tests from [step 15](15-service-network-policy.md),
   then a real Runner service job and cleanup. Enable services only after all
   checks pass.

The local test parses every base manifest document and rejects broad Role
resources/verbs. The generated admission package tests the policy and binding
structure. These tests do **not** execute CEL or prove API-server admission.
Generated per-container ceilings are described in [step 17](17-generated-admission.md);
aggregate quota and cluster tests remain before production use.
