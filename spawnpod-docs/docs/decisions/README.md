# Architecture Decision Records (ADR)

Use ADRs to record why a decision was made, alternatives, consequences and superseding changes. Statuses: proposed, accepted, superseded.

- [ADR-0001 — Execution core and adapters](0001-core-and-adapters.md) — **accepted as design direction**.
- [ADR-0002 — Kubernetes execution environments](0002-kubernetes-environments.md) — **accepted as design direction**.
- [ADR-0003 — Execution and recovery contract](0003-execution-contract.md) — **accepted as design direction**.
- [ADR-0004 — Workload topology and compatibility envelope](0004-workload-topology-and-compatibility.md) — **accepted as design direction**.
- [ADR-0005 — Policy, credentials and network boundaries](0005-security-and-network.md) — **accepted as design direction**.
- [ADR-0006 — Forgejo protocol, deployment and measurable gates](0006-protocol-and-operations.md) — **accepted as design direction**.

All identified architecture choices have a direction. The [compatibility and release gates](0006-protocol-and-operations.md#compatibility-and-release-gates) are empirical work still required before implementation can be called supported.

## ADR template

**Title / Status / Context / Decision / Alternatives / Consequences / Follow-ups.** Never mark a proposed implementation as deployed or tested.
