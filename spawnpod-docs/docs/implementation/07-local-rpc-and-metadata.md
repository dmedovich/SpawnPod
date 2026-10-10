# Implementation step 07 — local Forgejo RPC and Pod metadata

**Status:** the controller starts a Forgejo gRPC server on a private Unix
socket while it holds the Kubernetes Lease. The agent reports its runtime
environment and architecture. Local tests pass; K3s and real Runner end-to-end
behavior remain unverified.

## Startup and shutdown

1. `internal/config` requires a canonical absolute `controller.socketPath`.
   Each profile declares `architecture: amd64` or `arm64`.
2. The Kubernetes Pod builder pins `kubernetes.io/arch` to the profile and
   disables Kubernetes service environment links. The agent image must be
   published as a suitable multiarchitecture digest.
3. After acquiring the Lease, `internal/controller` performs an initial
   reconciliation. Only then does it register the Forgejo protobuf service
   and standard gRPC health service. A socket or startup recovery error stops
   leader election and the process exits with an error.
4. `internal/transport/unixrpc` creates a directory owned by the controller
   UID with no group or other access, binds a socket with mode `0600`, and
   removes only the socket it created. A stale socket owned by the same UID
   is removed only after a connection attempt proves it refuses connections.
   An active or foreign socket stops startup. The Runner must run as the same
   UID and use `unix:///run/spawnpod/backend.sock` for the example config.
5. Lease loss or process shutdown stops gRPC streams and removes the socket.
   The Lease is not released early; cleanup metadata remains for a successor.

## Runtime metadata

`spawnpod-agent metadata` returns JSON from inside the running job container.
The provider reads at most 1 MiB and verifies the recorded Pod UID before
opening `pods/exec`. The engine does not persist the environment map because
it may contain secrets. `Start` compares the observed architecture with the
approved profile before sending `StartComplete.image_env`. Create reports
`Linux` and maps `amd64` to `x86_64`, `arm64` to `aarch64`.

The environment map is the agent process's effective environment with common
runtime-only keys removed. It is a useful approximation of image `ENV`, but
the exact baked image config cannot be distinguished from all kubelet-injected
variables here. This needs a real Runner compatibility test. The Pod must run
on the architecture selected in the profile; a single pinned image digest
must resolve to a compatible image there.

## Verification and limits

Unit tests cover strict socket and architecture configuration, Pod node
selection, metadata mapping, private socket permissions, gRPC health and
Capabilities, rejection of a shared socket directory, and a controller test
that acquires a fake Lease, opens the socket, then closes it on cancellation.
Run `go test ./...`
and `go vet ./...`; `go test -race ./internal/controller
./internal/transport/unixrpc ./internal/adapter/forgejo` also passes locally.
Unix socket tests require an environment that allows
socket creation.

Health reports local listener availability after successful initial recovery.
It does not yet change to `NOT_SERVING` if the Kubernetes API fails later.
Remote TCP is not exposed; there is no remote authentication or TLS setup.
The next gate is a real K3s test of `pods/exec` metadata and file streams,
then a full Forgejo Runner job against this socket with cancellation and
cleanup. Service Pods, network policy and production deployment manifests are
still pending.

The [next local step](08-exec-stream-hardening.md) fixes agent exec stream
completion before that cluster test.
