# Implementation step 09 — service Pod policy

**Status:** local rendering and configuration validation are implemented.
Forgejo `Create` still rejects `services:`; service lifecycle, secret delivery,
network policy and K3s behavior are not implemented.

## Approved input

An operator may add `profiles.<name>.services.<service-name>` with a pinned
image digest, the job's architecture, numeric non-root UID/GID, one TCP port
and CPU, memory and ephemeral-storage bounds. `internal/config` rejects
unknown fields, invalid names and limits, a mutable image and an architecture
different from the job. The workflow cannot supply a PodSpec.

For example, inside `profiles.default` (replace the digest with an approved
image):

```yaml
services:
  redis:
    image: registry.example.com/redis@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
    command: [redis-server, --port, "6379"]
    architecture: amd64
    uid: 10002
    gid: 10002
    port: 6379
    cpuRequest: 50m
    cpuLimit: 250m
    memoryRequest: 64Mi
    memoryLimit: 256Mi
    storageRequest: 128Mi
    storageLimit: 512Mi
```

The service name is limited to 27 DNS-label characters so the deterministic
`sp-<32-character environment ID>-<service name>` resource name fits the
Kubernetes 63-character limit. The image must work with a read-only root
filesystem and a writable `/tmp` mount. The operator supplies the service
command because the agent runs as PID 1; the image entrypoint is not used.

## Rendered resources

`internal/provider/kubernetes.BuildServicePod` creates a separate Pod with
`restartPolicy: Never`, a pinned architecture, no service-account token,
non-root identity, dropped capabilities, RuntimeDefault seccomp and a TCP
readiness probe on the approved port. `BuildService` creates a ClusterIP
Service selecting only the Pod labels for that environment and service. It
exposes no host or node port. Neither resource is submitted to Kubernetes by
this step.

Workflow service environment values may include secrets. The current
protobuf request carries them, but placing them in PodSpec would expose them
to Kubernetes API readers. The renderers have no environment field; the
adapter continues rejecting any service request until startup can pass those
values to the service process over a protected stream. The remaining lifecycle
must persist resource UIDs, wait for readiness, add the service alias to the
job Pod and remove both resources by UID. NetworkPolicy and admission rules
must be enforced before service traffic is enabled.

## Verification

`go test ./internal/provider/kubernetes ./internal/config` covers the
restricted Pod, ClusterIP selector, port, empty environment, profile parsing
and invalid inputs. K3s admission, readiness and networking remain untested.
