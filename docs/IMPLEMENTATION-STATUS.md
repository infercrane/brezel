# Implementation status

This file distinguishes implemented control-plane behavior from designed or
qualified product behavior. A green unit test is not a deployment qualification.

## Implemented in this repository

| Capability | State | Evidence |
| --- | --- | --- |
| Immutable Environment registration | Implemented | Domain validation, content-derived revision, file-store and HTTP tests |
| Project-scoped Sandbox lifecycle | Implemented | Create, inspect, pause, resume, delete, expiration reconciliation |
| Explicit standby state kinds | Implemented | Filesystem and full-state remain distinct through validation and adapter calls |
| Crash recovery lookup | Implemented | Local ID and project are written to backend metadata and used for reconciliation |
| Independent filesystem checkpoint | Implemented | E2B snapshot adapter and API path |
| Independent full-state checkpoint | Unavailable | Rejected before backend execution; upstream snapshot route lacks the required explicit guarantee |
| Idempotent mutations | Implemented | Durable idempotency index; duplicate create test proves one backend call |
| Ordered lifecycle event log | Implemented for one controller | Durable append log; cluster ordering is not claimed |
| Signed lifecycle receipt | Implemented | Ed25519 DSSE envelope with an in-toto statement and tamper test |
| Tenant isolation at API/store boundary | Implemented | Project-scoped keys and cross-project denial test |
| Connector definition | Implemented | HTTPS, method/path, and opaque secret-handle validation |
| Connector credential enforcement | Developer preview | Five-minute signed lease, current-state checks, route policy, external file resolver, header stripping, DNS/metadata guard, response scrub |
| Release backend | E2B adapter only | API key over TLS, redirects disabled, unknown backend state remains unknown |
| Test backend | Tests only | Defined exclusively in `_test.go`; cannot be selected by release code |
| Deployment conformance | Implemented for the current control API subset | Explicit paid-resource acknowledgement, lifecycle, idempotency, cross-project denial, checkpoint, cleanup, and receipt verification |

## Not implemented yet

- environment/image builds;
- process, file, terminal, log, and preview data paths;
- durable volumes and shared drives;
- proof-of-possession lease renewal, managed secret providers, and streaming response scrubbing;
- fork, sandbox groups, jobs, rollouts, and webhooks;
- OIDC, organizations, RBAC, quotas, and approvals;
- PostgreSQL/Redis/object-store clustered profile;
- a Linux/KVM installer and host-isolation conformance suite;
- SDKs and CLI; and
- any qualified performance, hostile shared-multitenant, air-gap, GPU, or
  hardware-attestation claim.

## Current deployment profile

The runnable binary is an **early single-controller control plane** over a
configured E2B Runtime API. Its JSON file store uses atomic replacement and
survives restart, but it is intentionally not a multi-controller database. Run
it only in a development or controlled evaluation environment.

The current bearer-renewal connector is limited to the trusted-operator profile;
see ADR 0003. The next release gate is not more UI. It is substrate conformance
on a named Linux/KVM deployment, followed by guest data paths and
proof-of-possession connector identity.
