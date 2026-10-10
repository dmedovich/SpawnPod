# Implementation step 37 — packaged amd64 release candidate

**Date:** 2026-10-10. **Version:** v0.1.0-rc.1. The user explicitly scoped
this session to finishing/specifying/packaging the tool, not deploying it on
`shusha`. No target-server deployment was attempted. Real ARM64 support is
outside this amd64-only candidate; the broader production gates remain open.

## Shipped contract and packaging

The release-specific [SPEC](../../../docs/SPEC.md) distinguishes the candidate
from the full target PRD. [INSTALL](../../../docs/INSTALL.md) documents image
pinning, operator configuration, external source-proxy requirements, Runner
registration, installation, verification, quiesced upgrades and rollback.
[RELEASE](../../../docs/RELEASE.md) lists evidence and the remaining production
support gates. VERSION declares `v0.1.0-rc.1`.

`scripts/release.sh` produces static Linux/amd64 controller, agent and source
proxy binaries, binary/archive SHA-256 checksums, release provenance, dependency
inventory, available dependency notices and installation docs. Final builds
require committed sources; explicit local previews are marked dirty.
`scripts/release-image.sh` builds a scratch runtime from those binaries and
exports its OCI archive plus named manifest digest and complete SHA256SUMS.
The image contains all three component entry points, uses UID/GID 10001 and
records the release/version source labels. It does not bundle Forgejo Runner.

The pinned-action GitHub workflow checks source and race tests, and on a tag
matching VERSION builds/uploads candidate artifacts through GitHub's scoped
workflow token. A pushed tag or configured workflow alone is not publication
proof: verify the actual run, release and uploaded checksums separately.

## Install renderer

`render-deployment` accepts a validated operator config and digest-pinned
runtime/official Runner images. The reference layout is `infra`/`ci` with a
single Recreate controller Deployment. It emits the operator ConfigMap,
Deployment and internal monitoring Service, clears host kubeconfig paths and
sets the private shared socket. Only the controller mounts a projected API
ServiceAccount token; only Runner mounts its externally created registration
Secret. Both containers are non-root with read-only roots and dropped
capabilities. Config checksum changes trigger rollout. Runner waits for the
private socket before polling jobs.

Unit tests verify token/Secret separation, lack of host kubeconfig in rendered
output, rollout behavior, image pinning and monitor exposure. The generated
ConfigMap, Deployment and Service passed **server-side dry-run** against local
K3s. This is schema/admission evidence, not a completed in-cluster sidecar
installation. The official Runner's amd64 image manifest observed during this
session was `sha256:96af81b1b0cd928f3f135fb8dc021d47cff868c151a4fd28af971e892990ee7a`.

## Probes, metrics and shutdown

`controller.monitorAddress` enables `/healthz`, `/readyz` and `/metrics`.
Readiness requires an active RPC server, a recent successful Kubernetes probe
and no drain. API failures and stale probes fail readiness; liveness remains
local. Metrics use bounded RPC method labels, never environment/repository IDs
or error text. Reconciliation counters and its last successful timestamp are
exposed; RPC duration is sum/count, not a percentile histogram.

On SIGTERM, the RPC server becomes unready and begins graceful stop. Existing
RPCs can complete within `drainTimeout` (default 30s, range 1s–5m); after that
they are cancelled. Lease renewal remains independent of the shutdown signal
until drain finishes. Genuine Lease loss forces immediate stop. This is RPC
drain, not multi-step job drain; planned upgrades still require quiescing
Runner and waiting for workflows.

Tests verify graceful completion, timeout cancellation, Lease loss during
drain, readiness dependency/staleness behavior and metric label safety. The
100-second Runner keepalive regression passed on the updated server. Targeted
race tests passed for controller, Unix RPC, telemetry and deployment packages.

## Local release-binary evidence

A temporary one-hour token for `infra/spawnpod-controller` was used with a
private kubeconfig to run the packaged controller directly, **without admin
impersonation**. `doctor` passed with this restricted identity. No token value
was printed or included in source/artifacts. Readiness returned 200 and
reconciliation/RPC counters were visible on loopback `127.0.0.1:19090`.

- Full RPC smoke passed (Create/Start/CopyIn/Exec/CopyOut/Remove), environment
  `7844a8976d27e2e6a8925ad666440b72`.
- Official Runner private HTTPS Node checkout passed again as **run 40** using
  the packaged controller, with exact commit/shallow assertions.
- Each runtime-image component printed `v0.1.0-rc.1` in a rootless, read-only,
  networkless container with capabilities dropped and no new privileges.
- Live negative admission, RBAC and quota checks passed again. The first
  attempt found that the old smoke fixture omitted the now-required network
  profile label; the fixture now includes `spawnpod.dev/network=isolated`.
- A long hold command ran in environment
  `45fc83f6a6d1020c286d5e04da5744ce`. SIGTERM made readiness return **503 in
  0.012 seconds**. With a five-second local drain budget, the RPC closed after
  the budget with the server's `graceful_stop` shutdown indication.
- Restart reconciliation marked that environment `cleaned`; no Pod or Service
  remained and cleanup preview had no candidates. Readiness returned to 200.

The local controller was restored using the ordinary private admin kubeconfig
with restricted-controller impersonation, not the expiring test token.
`go test -short ./...`, `go vet ./...`, `go mod verify`, the full keepalive test,
targeted race tests, shell/Python syntax checks and `git diff --check` passed.
The explicit long test plus the short suite cover the normal full suite.

## Remaining production evidence

This candidate does not claim capacity/SLOs on `shusha`, a live install of the
new sidecar topology, real ARM64, a maintained production job image/PKI,
complete live supervisor-death/eviction/resource-pressure coverage or a
previous supported version's upgrade/rollback. These gates are recorded in
the release checklist. The prior lifecycle, security and fault journals remain
the evidence for the corresponding implemented behaviors.
