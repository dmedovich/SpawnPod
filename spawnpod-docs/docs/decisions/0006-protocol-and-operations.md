# ADR-0006 — Forgejo protocol, deployment and measurable gates

**Status:** Accepted (design direction) · **Date:** 2026-10-07

## Decision

- Pin the first adapter to **Forgejo Runner v13.1.0** and its `plugin.v1alpha` schema. Generate code from that pinned source or pin the matching Go module. Later releases require a schema diff and integration matrix before support is declared. `Capabilities` reports only the backend name that the observed protocol can represent; no invented feature negotiation fields.
- Configure Forgejo Runner's `plugins` entry to point to the local Unix socket and expose labels of the form `label:plugin://approved-image`. Plugin `options` may contain only an authorized profile name and documented non-secret settings. In the inspected local runner, the label suffix is `label_arg`, while `CreateRequest.image` is the job's `container.image` or empty. Resolve the approved image from `image` when present, otherwise from `label_arg`; validate either choice. `Start` and `Exec` preserve the runner's output/terminal ordering and always send a terminal message; `ExecFailed` includes a non-empty error string. Unknown features return `InvalidArgument` or an explicit unsupported error; missing environment IDs return `NotFound`; controller unavailability returns `Unavailable`.
- The internal environment API may include Inspect, List, Stop and capability descriptions, but the observed Forgejo plugin has no corresponding RPCs. Likewise it has no stdin request field for a workflow command. Do not advertise these as Forgejo capabilities.
- Configuration is a versioned YAML document. Startup rejects unknown fields and invalid profiles, deadlines, quotas, image digests and network dependencies. Precedence is command-line path/flags, then configuration file, then built-in safe defaults; environment variables are limited to explicitly documented bootstrap fields and cannot loosen a profile. `validate` is offline; `doctor` probes cluster RBAC, admission, policy enforcement, namespace resources and proxy connectivity.
- A single controller deployment is supported. On SIGTERM it stops accepting Create, allows a configurable bounded drain for in-flight operations, then cancels remaining commands and leaves reconciliation metadata for cleanup. Readiness fails when Kubernetes/API dependency or Lease ownership is lost; liveness reports only local process health. Upgrades must keep record schema readable across one previous minor version; irreversible schema migration requires an explicit operator step. Rollback fails closed if record schema is too new.
- Default limits are configuration **safety caps**, not performance promises: environment lifetime 60 min, provisioning 10 min, command 30 min, cleanup attempt 2 min, disconnect grace 10 s, TERM grace 5 s, maximum 2 active environments on the single-node reference deployment. Operators tune them after measuring `shusha`; all values have validation bounds. No numeric latency/availability SLO is claimed before measured K3s and Forgejo end-to-end runs. The target is no resource left beyond its deadline plus reconciliation recovery after the API becomes available.
- Each resource and operation emits structured status with environment ID but no secrets; metrics omit environment ID and repository labels. Cleanup failures are retried with bounded backoff and remain visible. A dry-run administrative cleanup lists exact names/UIDs and reasons; the destructive operation must recheck UID and ownership.

## Compatibility and release gates

1. Diff the local `/home/dmedovich/runner-main/runner` source against the tagged Runner v13.1.0 source, then build and run the pinned protocol against that release. Verify Create/Start/Exec/CopyIn/CopyOut/Remove, errors (including empty `ExecFailed` defense), cancel-then-Remove race, image env, paths, OS/architecture and service hostname/ports. Record exact runner commit and test output in a version matrix.
2. In K3s, verify Restricted admission, Role permissions, default-deny egress, source proxy, service isolation, `emptyDir` limits, non-root images and both architectures. Negative tests must show blocked privileged/host/secret access and tar traversal.
3. Fault-inject API loss, controller restart, supervisor death, Pod eviction and deletion failure. Verify indeterminate commands are never replayed and all owned resources become removable after recovery.
4. Measure cold provision latency, image-pull latency, command round trip, memory/CPU idle footprint, maximum concurrent jobs and cleanup latency on the actual single-node hardware. Set SLOs only from these measurements plus an explicit capacity budget protecting `prod` and observability.

## Sources

- [Forgejo Runner v13.1.0 plugin announcement and configuration](https://forgejo.org/2026-09-runner-release-v131/)
- [Forgejo plugin Go API for v13.1.0](https://pkg.go.dev/code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha)
- [Kubernetes local ephemeral storage](https://kubernetes.io/docs/concepts/storage/ephemeral-storage/)

## Consequences

These decisions specify what to build and what can be claimed after testing. The Forgejo plugin remains experimental upstream, so a successful prototype does not establish future-release compatibility. Numeric SLOs and exact hardware capacity are empirical outputs, not architecture choices.
