# Implementation step 17 — config-derived workload admission

**Status:** generation, CLI rendering, startup drift check and local tests are
implemented. The policy was applied to local K3s on 2026-10-09. Its first
version produced a CEL type-check warning for `resources.requests` and
`resources.limits`; the resource-field expression was replaced by LimitRange
defaults. Live admission scenarios remain pending.

## Generation

`internal/admission` builds one `ValidatingAdmissionPolicy`, one `Deny`
binding, one container `LimitRange` and one namespace `ResourceQuota` from the
strict operator configuration. The aggregate budget is described in
[step 18](18-namespace-resource-quota.md).
Run:

```sh
go run ./cmd/spawnpod render-admission --config deploy/config.example.yaml > /tmp/spawnpod-admission.yaml
```

An administrator applies the output after `deploy/security.yaml` and before
starting the controller. The policy matches Pod create/update in namespaces
labeled `spawnpod.dev/role=workload`. It requires spawnPod ownership labels,
the tokenless job ServiceAccount, one init and one job or service container,
agent entry points, emptyDir-only volumes, and no PodSpec environment or host
ports. The generated final rule permits only
the configured agent image in the init container, configured job images in
job containers and configured service images in service containers. Images
are compared exactly, including their digests. The expression is deterministic
across configuration map iteration and capped at 16 KiB.

The `LimitRange` uses the largest configured CPU, memory and ephemeral-storage
limit across job and service profiles as a namespace-wide per-container
ceiling. It also sets default requests and limits to those maxima for CPU,
memory and ephemeral storage. Omitted fields therefore receive bounded values
before quota admission, including ephemeral storage that quota otherwise
ignores when absent. The adapter and provider retain the narrower limits of
the selected profile.

`controller.Preflight` reads the named policy and binding and compares the
installed rules, namespace selector, `Deny` action and `LimitRange` maxima
with what the current config generates. A missing or changed rule fails
startup. The controller has only `get` permission on the two cluster-scoped
admission objects and the named namespace `LimitRange`. Changing profiles
requires rendering and applying the new admission policy before restarting
the controller; coordinate this with running Pods because their updates may
be evaluated against the new image list.

## Remaining verification

Local tests cover rendering, service images, deterministic config changes and
refusal to start with a missing or changed policy. They cannot prove CEL
compilation or actual Pod admission. On K3s, inspect the policy's
`status.typeChecking` warnings and test server-side dry runs: approved job
and service Pods must pass; foreign digests, mutable tags, extra containers,
secret volumes, PodSpec environment, host ports and another ServiceAccount
must fail. Verify that missing resource fields receive bounded defaults or
are rejected by quota. The API server must also reject policy deletion or mutation by the
controller ServiceAccount.

The generated `LimitRange` gives a per-container ceiling. Aggregate
`ResourceQuota` is rendered and checked locally, but concurrency behavior and
measured node capacity remain release work. The allowlist is global across profiles at admission; the Forgejo
adapter still enforces the selected profile's narrower image choice.
