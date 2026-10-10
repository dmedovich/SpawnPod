# ADR-0001 — Provider-independent core with CI adapters

**Status:** Accepted (design direction) · **Date:** 2026-10-07

## Context

Forgejo Runner provides an alpha plugin protocol. Binding all business logic to generated Forgejo protobuf types would make upstream protocol changes expensive and hinder other integrations.

## Decision

Use a Go modular monolith with a Forgejo adapter, provider-independent execution engine, policy/profile modules and Kubernetes provider. Adapter owns external protocol; engine owns environment semantics; provider owns Kubernetes details.

## Alternatives

Forgejo-specific plugin with Kubernetes logic embedded in RPC handlers; completely generic public execution service from day one.

## Consequences

Clearer boundaries and testability, but some translation overhead. Other runner support is an architectural option, not a delivered feature without separate integration work.
