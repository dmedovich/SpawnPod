# Implementation step 02 — Kubernetes job Pod provider

**Status:** Pod rendering and basic API transport implemented and unit tested;
no cluster integration test has run.

## Packages and control flow

`internal/provider/kubernetes` is the only current package importing
Kubernetes API types. It accepts a fully approved `JobSpec`; profile selection
and authentication remain the future engine's responsibility.

1. `BuildJobPod` validates the random environment ID, digest-pinned job/agent
   images, non-root UID/GID and resource requests/limits. It renders one job
   Pod with `restartPolicy: Never`, no automatic service-account token, dropped
   capabilities, RuntimeDefault seccomp, read-only root filesystems and
   disk-backed workspace/tools/tmp `emptyDir` volumes.
2. The init container runs `spawnpod-agent install /tools/agent` from the agent
   image. The job container mounts that tools volume read-only and launches
   `/opt/spawnpod/agent serve`. Its readiness probe runs `agent ping`.
3. `CreateJob` submits the Pod and returns the API-assigned UID. `WaitReady`
   polls Pod readiness and rejects a changed UID or terminal Pod phase.
4. `Exec` checks the recorded UID, starts `agent invoke` through `pods/exec`,
   sends the command request on stdin, decodes JSON output events and requires
   a terminal event. A clean transport EOF without one is an error.
5. `CancelCommand` runs `agent cancel ID` through a second `pods/exec` session.
   Its response is an acknowledgement, not proof that the process has exited.
   `RemoveJob` uses a Kubernetes UID precondition to avoid deleting a Pod that
   replaced the recorded one.

The agent image recipe is [`deploy/agent.Dockerfile`](../../../deploy/agent.Dockerfile).
Publish a multi-architecture image and pin it by digest before cluster use.

## Verification

`go test ./...` checks restrictive Pod fields, workspace and resource
validation, readiness UID checking, and delete UID preconditions through a
client-go fake. These tests do not prove Pod Security admission, volume
permissions, CNI policy, image compatibility or SPDY behavior on K3s.

## Next integration step

Build the environment engine that persists desired state and resource UIDs in
ConfigMaps, serializes operations, owns cleanup deadlines, and calls this
provider. Then run a kind/K3s integration test with a real `pods/exec` stream,
real non-root image, agent image and cancellation. Service Pods, file transfer,
NetworkPolicies and the Forgejo adapter follow that stable lifecycle.
