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
