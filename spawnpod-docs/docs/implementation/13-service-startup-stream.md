# Implementation step 13 — service startup without PodSpec secrets

**Status:** the agent, Kubernetes provider and engine have a local service
startup path. Forgejo `Create` now maps `services:` behind a disabled-by-default
configuration gate described in [step 14](14-forgejo-service-mapping.md). Kubernetes SPDY,
non-root image compatibility, networking and secret handling in real Runner
jobs remain unverified.

## Startup contract

An operator service profile now includes an explicit `command` array. The
service Pod uses the pinned agent image in an init container, then runs
`spawnpod-agent serve-service` as PID 1 inside the approved service image.
The image's original entrypoint is ignored. It must supply the command's
executable and work with a read-only root filesystem and writable `/tmp`.

During `Create`, the engine creates and records the Service and Pod, resolves
ClusterIP, then calls `StartService` before creating the job Pod. The provider
waits for the service Pod and its agent socket, opens `pods/exec` on the
service container, and sends a bounded JSON request to `agent service-start`
**on stdin**. The request carries the operator-approved command and transient
workflow environment. The service agent starts the process with the image's
base environment plus those values and sends one acknowledgement. Closing
the exec connection leaves the process running under agent PID 1. If the
process exits, PID 1 exits and the service Pod becomes terminal. `Start`
still waits for its TCP readiness probe. The Create path is bounded by ten
minutes or the earlier environment deadline.

`CreateRequest.ServiceEnvironments` is held only for this operation. The
engine does not write it to `ServiceRecord`, ConfigMap or PodSpec. Invalid
environment names and NUL values are rejected; the provider caps the request
at 1 MiB. A startup failure marks the environment failed, and reconciliation
removes any partially created resources. The service process may itself print
environment values to its logs; this transport does not prevent workload code
from exposing secrets it receives.

## Verification and release gate

Local tests cover agent start acknowledgement, environment delivery,
continued execution after the connection closes, rejection of a second
start, process-exit behavior, empty PodSpec environment and the absence of a
test secret from serialized management records. `go test ./...` and
`go test -race ./internal/supervisor ./internal/engine
./internal/provider/kubernetes` exercise these paths locally.

The provider's real `pods/exec` path needs a K3s test. Strict image, port and
profile mapping now exists behind `controller.enableServices`; keep it disabled
until service NetworkPolicy and admission rules are installed and tested, then
run a real Runner job including cancellation and cleanup. The profile's
explicit command limits compatibility with images that rely on their original
entrypoint.
