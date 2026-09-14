# ADR 0006: Bind credentials to projects and fence single-host state

- Status: accepted
- Date: 2026-09-13

## Context

The developer API originally compared one global bearer token and then trusted
the caller's `X-Project-ID`. Project-scoped storage prevented reading another
project's objects, but the credential itself could select or create any project.
The JSON authority also allowed two controller processes to open the same file,
reported readiness without checking the engine, and had no project resource
limits. Those behaviors are not an acceptable private deployment boundary.

## Decision

The hardened single-host profile uses a versioned protected policy containing
only SHA-256 token digests and explicit project allowlists. Authentication and
project authorization are separate results. An unknown token returns `401`; a
known principal requesting an unbound project returns `403`. The global trusted-
operator token remains available only through an explicit development switch.

The profile also:

- applies positive per-project count and live-operation limits before backend
  mutation;
- binds create idempotency keys to canonical request digests;
- exclusively locks the state authority to one controller process;
- rejects symlink, loose-permission, and oversized state files;
- fsyncs both the replacement file and its parent directory; and
- reports ready only when state and the authenticated engine health path pass.

## Consequences

A stolen project token remains a bearer credential, but it cannot manufacture
access to an arbitrary project. Static policy changes require a controlled
restart. The file authority now fails closed under concurrent controller start
and common crash-consistency hazards.

This is still not organization identity, fine-grained RBAC, distributed
fencing, dynamic quota administration, a replicated database, or disaster
recovery. Those are release gates for the clustered profile rather than claims
of this ADR.
