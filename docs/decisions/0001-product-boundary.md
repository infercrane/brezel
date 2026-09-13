# ADR 0001: Build an agent runtime, not only a trust wrapper

- Status: proposed
- Date: 2026-09-13

## Context

An earlier design limited the project to capability admission and signed
execution receipts over third-party sandboxes. That is useful security plumbing,
but it does not meet the product requirement: customers need an integrated
execution primitive with isolation, resumable state, storage, networking, jobs,
and proximity to inference.

At the same time, a from-scratch public microVM cloud would consume most of the
project on undifferentiated Firecracker orchestration before a user can run a
valuable workload.

## Decision

Build a complete self-hosted agent runtime product while reusing a maintained
open-source microVM data plane.

The project owns:

- the developer-facing environment, sandbox, workspace, checkpoint, connector,
  job, rollout, and policy experience;
- installation, upgrades, operations, and conformance;
- durable lifecycle and cleanup semantics;
- tenant identity, policy, quota, placement, and audit;
- private networking, egress, and endpoint-bound credentials;
- job coordination and shared-workspace integration; and
- optional signed execution receipts.

The project does not initially own:

- a hypervisor or KVM implementation;
- a new Firecracker control plane;
- a custom distributed filesystem;
- a model server, training system, or agent framework;
- a global public capacity fleet; or
- compatibility with undocumented proprietary provider internals.

## Consequences

Positive:

- the repository describes a real end-to-end product rather than one subsystem;
- users can self-host a complete stateful agent runtime without waiting for a
  new VMM;
- private inference and enterprise operation create a distinct position; and
- upstream runtime improvements benefit more than one project.

Negative:

- the product inherits upstream runtime architecture and release risk;
- a cohesive distribution still requires serious lifecycle, network, storage,
  identity, and upgrade engineering;
- API compatibility must be narrower than the product vision at first; and
- hosted-provider latency and global scale remain out of reach for the MVP.

## Revisit when

Reconsider the boundary if a required isolation or lifecycle capability cannot
be added upstream or maintained as a small patch set, or if design partners need
a custom data plane whose measured value justifies ownership.
