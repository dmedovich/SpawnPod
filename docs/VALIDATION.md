# Candidate validation

Version: v0.1.0-rc.2, Linux/amd64. Tests ran on local K3s; the target server
and ARM64 hardware were not tested.

Validation date: 2026-10-10. Source suite, short race suite, vet and module
integrity checks passed with Go 1.27.2. Source and all three built binaries
reported **No vulnerabilities found** with govulncheck v1.8.0.

## Source and release checks

```sh
go test -count=1 ./...
go test -race -short ./...
go vet ./...
go mod verify
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
git diff --check
```

The full suite includes a real-time 100-second keepalive regression. Security
regressions cover session escape, syscall ABI checks, failed guard installation,
exit/cancel races, disconnected and non-reading clients, terminal slot reuse,
synthetic credential canaries and partial record recovery. Failure persistence
is tested with a store that blocks until its context expires. No real credentials
are used in diagnostic canary tests. The vulnerability scan is a database snapshot,
not a guarantee against unknown vulnerabilities.

Package checks covered binary versions, checksums, source provenance and
third-party notices. See [RELEASE.md](RELEASE.md) for the build procedure.

## Local K3s acceptance

Environment: Fedora 43, Linux 7.2.7, K3s v1.34.12+k3s1, containerd 2.2.7,
Linux/amd64. Jobs use UID/GID 10001, a read-only root filesystem, dropped
capabilities and RuntimeDefault seccomp plus the inherited command guard.

| Check | What is exercised |
|---|---|
| Admission and RBAC | A valid dry-run Pod succeeds; unapproved images, privileged/host-network/host-path Pods, Secret mounts/env, API token mounts and alternate entry points are rejected. Controller identity can create job Pods but cannot read Secrets or create Pods in another namespace. |
| Network isolation | Own-service HTTP succeeds before and after denial tests. Job access to Kubernetes API, an unrelated reachable control Pod and another environment's service fails; unrelated service ingress fails. A control Pod without NetworkPolicy verifies endpoint reachability. |
| Command compatibility | Git init, Node subprocesses and tokenless execution succeed; `setsid` cannot create an escaped descendant. |
| API outage | The controller's HTTP transport is blocked after durable transfer intent. CopyIn returns within a bounded time; a fresh engine cleans the retained transferring record after connectivity returns. The K3s server itself stays running. |
| Controller crash | SIGKILL during a real command leaves executing intent; restarted controller reconciles it to cleaned and removes the job Pod. |
| Persisted states | Real resources plus synthesized records in provisioning, ready, executing, transferring, failed, indeterminate, terminating and cleaned states; fresh-engine recovery verifies the documented state policy. These are not eight independently timed crashes. |
| Controller upgrade/rollback | Actual rc.1 and rc.2 host binaries read drained version-1/version-2 records. Current package RPC smoke covers command execution and file transfer. All jobs are removed before switching controllers; both versions use the current agent image. |
| Capacity | Three simultaneous bounded Node workloads, metrics-server job CPU/RAM samples and host-controller RSS/CPU measurements. This is a local sample, not a capacity recommendation for another node. |

### Local workload sample

Three 90-second Node workloads each allocate/touch a 16 MiB buffer. The
metrics-server sample taken about 35 seconds into concurrent execution showed:

| Component | CPU | Memory |
|---|---|---|
| Job 1 | 400m | 26 MiB |
| Job 2 | 500m | 26 MiB |
| Job 3 | 501m | 27 MiB |
| Host controller during sampling window | 6m average | 47.9 MiB RSS |
| Host controller before workload start, with three jobs provisioned | Not measured separately | 44.2 MiB RSS |

Each job is limited to 500m CPU/256 MiB RAM. Instantaneous metrics and sampling
rounding can slightly exceed the nominal CPU limit. Controller CPU comes from
`/proc/PID/stat` deltas divided by elapsed wall time, over approximately 35 seconds;
job CPU/RAM comes from metrics-server, so these are different sampling windows.
This sample does not measure Node builds, image pulls or cold provisioning.
Measure actual workflows on the target node before raising concurrency.

### Reproduce

The source repository contains the opt-in suite and fixtures in
[`tests/integration/`](https://github.com/dmedovich/SpawnPod/tree/main/tests/integration).
Its README lists prerequisites, commands and admission-policy cleanup behavior.
Ordinary `go test` skips the live cluster suite.

## Target-cluster checks

Repeat installation, real packet denials, representative workflows and resource
measurements on the target cluster. Test its HTTPS trust, source proxy routes,
scheduling/storage budget and failure behavior. Local synthetic API failure does
not establish resilience to every API/network outage. The record-state matrix
does not replace eviction, OOM or disk-pressure testing. Drain/remove old jobs
before controller/agent upgrade or rollback; running-job migration and mixed
controller versions are unsupported. Real ARM64 execution and hostile tenant
isolation remain outside this candidate's support claim.

## Operations changes for rc.5, 2026-10-11

The working-tree controller passed `make check`: all Go tests, `go vet`,
module verification and whitespace checks. Added regression coverage includes
custom tombstone retention, repeated jobs, restart and delete retry, safe and
unsafe default-deny selectors, strict doctor warnings, current quota usage,
missing Runner Secret keys and suppression of credential contents.

All local K3s acceptance scenarios passed across the initial run and a targeted
retry of Capacity, ControllerCrash and ReleasedControllerUpgradeRollback.
The initial failures of those three scenarios came from a test evidence
directory created with mode 0755; after setting the required 0700, all three
passed. The controller's private socket check stayed enabled.

Confirmed with the real Kubernetes API:

- Pod removal and retention expiration after process exits 0 and 7. Only test
  record timestamps were backdated; ConfigMap deletion used real resourceVersion
  preconditions and a fresh Engine, without waiting a full minute.
- Recovery of interrupted states and a killed controller; synthetic API outage.
- Namespace-wide default-deny with permitted same-environment service traffic
  and denied API egress, unrelated Pod egress, cross-environment service traffic
  and unrelated service ingress, using positive controls.
- Three concurrent jobs and drained rc.1 → working-tree controller → rc.1
  upgrade/rollback. The suite restores the legacy managed selector before
  starting old controllers, and uses namespace-wide selection for the new one.
- Test namespaces were deleted; the original admission policy UID and spec
  matched the saved snapshot after cleanup.

Local evidence: `.local/operations-qol-20261011/check.log`, `cluster.log`,
`cluster-retry.log` and controller logs (ignored by Git). These scenarios use
direct plugin RPCs and an existing local agent image; they do not confirm a
successful workflow in the original Forgejo installation or validate a newly
published OCI image. Release publication, GitOps rollout and that installation's
workflow outcomes remain pending.

## rc.6 upgrade development, 2026-10-11

Local platform: amd64, K3s `v1.34.12+k3s1`, official Forgejo Runner
`v13.1.0`. This is development evidence, not validation of a published rc.6
artifact or the target server's K3s `v1.36.4+k3s1`.

Confirmed:

- `make check` and `go test -race -short ./...` pass.
- Real API round trip accepts rendered/defaulted admission. Changing only the
  expected Agent digest fails at `admission.policy.validations[7].expression`,
  in both startup preflight and doctor. This reproduces a stale configuration
  failure, not the original historical incident's unknown cause.
- Host Controller rc.4 → development → rc.4 retains drained records and passes
  direct plugin RPC smoke.
- A full containerized rc.4 installation, using its release binary/OCI image
  and the official Runner, passes a real Forgejo workflow. A drained switch to
  the locally built preview passes ready checks and another real workflow.
- An intentional missing Controller config produces a failed rollout;
  `upgrade check` rejects it. Restoring old network/admission and the old full
  ConfigMap/Deployment/Service restores rc.4 and a real Forgejo workflow.
- Repeating the target update after recovery passes readiness and a fourth
  real workflow. The full scenario passed twice; the second run also uses the
  strict Deployment comparison with enumerated API defaults.
- The Runner Secret's UID, resourceVersion and credential data remain unchanged;
  previous CI records remain available. Test namespaces are removed and the
  original admission policy is restored by the acceptance harness.

The first two container smoke attempts exposed a test configuration mismatch:
Runner requested a 30-minute environment while the profile allowed 10 minutes.
They failed with `environment timeout exceeds profile limit`. Matching Runner
`timeout` to profile `maxLifetime` resolved it; the production timeout guard
remained enabled. Successful local Forgejo runs: 43–50 (two four-job sequences).

Private evidence (ignored by Git): `.local/rc6-validation/`, including
`cluster-admission-upgrade.log`, `cluster-bundle-first-success.log`,
`cluster-bundle-final.log`, `check-final.log`, `race-final.log`, workflow logs and
per-phase manifests. The container test uses preview runtime digest
`sha256:d028383b861c6599aecf43dc559e125f1afdfcb783dca80e3dc0c65f1dd79e1c`;
subsequent CLI diagnostics/refinements are covered by source tests and the CI
bootstrap, which rebuilds runtime from its own checkout. A final release image
must pass the CI job from its actual source revision.

### RFC acceptance coverage

| RFC test | Evidence |
| --- | --- |
| T01, T03 | deterministic bundle and shared runtime dependency tests |
| T02, T06 | real K3s admission and container ready checks |
| T04, T05, T14 | stale Agent named predicate; strict full admission specs; shared validator and separate Controller identity |
| T07 | Recreate quota ownership/usage tests; historical events retained only as evidence |
| T08–T10 | containerized intentional failure, full restore and repeat update |
| T11 | local image cache/node architecture blockers; registry platform/authentication remain explicit operator prerequisites |
| T12 | unchanged live Secret UID/RV/data and retained CI records |
| T13 | real official Runner/Forgejo workflows before and after switch/recovery |
| T15 | unrelated files preserved, edited/symlink conflicts rejected, render preview and interrupted-write recovery |
| T16 | drain gate rejects remaining managed Pods; no mixed-generation admission is generated |

`upgrade-ci.sh` adds the containerized scenario to the GitHub release workflow
as a prerequisite for publication. Its bootstrap is restricted to disposable
GitHub-hosted VMs and installs pinned K3s, verifies prior release checksums,
builds/imports the current runtime and runs real local Forgejo workflows. The
bootstrap has local syntax/lint checks; its GitHub execution is still pending.
The local actionlint version also flags the pre-existing `artifact-metadata`
permission, which the rc.5 release workflow already used successfully.

The original rc.5 startup predicate remains unproved from the supplied healthy
later snapshots. No claim is made that a particular default or `DeepEqual`
caused it. New field/hash/binary/config diagnostics allow a future failing
observation to be distinguished from the passing host context. Automatic apply,
expanded Agent generations and automatic rollback are not implemented.
