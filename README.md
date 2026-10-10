# SpawnPod

Run Forgejo Actions jobs in Kubernetes Pods, without a Docker daemon.

SpawnPod plugs into the official Forgejo Runner. The Runner reads your workflow
and decides which steps to execute; SpawnPod creates the job environment,
runs commands, moves workspace files and cleans up Kubernetes resources.

It is intended for operators who already use Kubernetes and want to decide
exactly which images, resources and network access their CI jobs may use.

**Status: v0.1.0-rc.3, Linux/amd64 release candidate.** Tested locally with
Forgejo Runner, including private HTTPS checkout. See the
[validation results](docs/VALIDATION.md) and [security model](docs/SECURITY.md).

## How it works

```text
Forgejo → official Runner → SpawnPod → Kubernetes job Pod
                             │         ├─ workspace
                             │         └─ process supervisor
                             └─ approved service Pods
```

The Runner and controller communicate over a private Unix socket. The
controller has a restricted Kubernetes identity. Job Pods have no Kubernetes
API token, run as a fixed non-root user and use approved images pinned by digest.
State is kept in ConfigMaps so a restarted controller can find interrupted work
and retry cleanup.

## What you get

- Execution profiles with image allowlists, CPU/RAM/storage limits and deadlines.
- A workspace shared between a job's steps, streamed output and file transfer.
- Optional approved service containers and controlled source-network access.
- Generated admission policy, RBAC, quotas and default-deny NetworkPolicies.
- Recovery reconciliation, health/readiness endpoints and Prometheus metrics.
- Static binaries, an OCI runtime image and release checksums.

SpawnPod supports **Forgejo Runner v13.1.0** and its experimental plugin
protocol. It does not provide arbitrary Docker compatibility, privileged
Docker-in-Docker, cross-job caches or a strong isolation boundary between
hostile tenants sharing a node. See the [specification](docs/SPEC.md).

## Try it

You need a Linux/amd64 Kubernetes test cluster with enforcing NetworkPolicy,
stable ValidatingAdmissionPolicy, a registered Forgejo Runner and a suitable
job image containing your workflow's tools. Node-based actions need Node in
that image; the SpawnPod runtime image contains only the SpawnPod binaries.

Start with the [installation guide](docs/INSTALL.md). It covers package
verification, approved image digests, policy installation and Runner setup.
The sample configuration has placeholder digests and must be edited first.

To build from source with the release-tested Go 1.27.2 toolchain:

```sh
make check
./scripts/release.sh
./scripts/release-image.sh
```

The release scripts require clean, committed sources and write to `dist/`.
They do not upload or deploy anything. `go 1.26.0` in `go.mod` sets the
minimum Go version; `toolchain go1.27.2` selects the preferred toolchain.
With automatic selection enabled, Go downloads it if needed. See
[Go toolchain selection](https://go.dev/doc/toolchain).

## Documentation

- [Installation and operations](docs/INSTALL.md)
- [Architecture and lifecycle](docs/ARCHITECTURE.md)
- [Release specification](docs/SPEC.md)
- [Security model and installation requirements](docs/SECURITY.md)
- [Validation and local K3s acceptance](docs/VALIDATION.md)
- [Release process](docs/RELEASE.md) and [candidate release notes](docs/releases/v0.1.0-rc.3.md)

## Repository

| Directory | Contents |
|---|---|
| `cmd/` | Controller, in-Pod agent and source proxy entry points |
| `internal/` | Forgejo adapter, lifecycle engine, Kubernetes provider and supervisor |
| `deploy/` | Installation manifests and runtime image recipe |
| `tests/integration/` | K3s acceptance suite, workflow fixtures and test setup |
| `scripts/` | Release packaging |
| `docs/` | Maintained public documentation bundled with releases |


Licensed under [Apache-2.0](LICENSE). The vendored Forgejo plugin schema retains
its upstream MIT notice.
