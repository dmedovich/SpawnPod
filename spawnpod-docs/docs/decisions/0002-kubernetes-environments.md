# ADR-0002 — Kubernetes-managed ephemeral environments

**Status:** Accepted (design direction) · **Date:** 2026-10-07

## Context

CI should execute inside the existing K3s platform without requiring privileged Docker-in-Docker or granting runner processes host-level container access.

## Decision

Provision isolated, ephemeral Kubernetes environments with one job Pod per environment and a shared temporary workspace. Required services use separate Pods and Services as specified in [ADR-0004](0004-workload-topology-and-compatibility.md). Run the controller in `infra` and workloads in `ci`; enforce policy in both spawnPod and Kubernetes admission.

## Alternatives

Host executor; privileged DinD; one Pod per individual step; VM-only executor.

## Consequences

Workspace persists between steps, resource quotas and network controls can be applied. The execution and security contracts are specified in ADR-0003 through ADR-0006 and still require implementation and testing.
