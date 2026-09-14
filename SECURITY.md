# Security policy

This project is an early implementation and has not completed an external
security review. Do not use it as a hostile shared-multitenant boundary yet.

## Reporting a vulnerability

Please report suspected vulnerabilities privately to `security@infercrane.com`.
Do not include production credentials, customer data, or weaponized public
proofs. Include the affected revision, deployment profile, expected boundary,
and minimal reproduction when possible.

We will acknowledge a complete report, coordinate remediation and disclosure,
and credit the reporter when requested. No bug-bounty payment is promised.

## Current assurance boundary

- Release execution uses the bundled, locally deployed microVM engine through
  a private loopback interface. Users do not configure a managed substrate API.
- The internal adapter requires Firecracker isolation as a deployment prerequisite, but
  a specific deployment is not qualified until the conformance suite passes.
- The local file state store is exclusively locked to one controller, private,
  size-bounded, schema-versioned, identity-validated, atomically replaced, and
  directory-synced. It is not a
  clustered database, replicated backup, or physical-host-loss boundary.
- API credentials are represented by SHA-256 digests in a protected policy and
  bound to explicit project IDs. OIDC, fine-grained roles, revocation without a
  policy reload, and organization administration are not implemented.
- Positive project limits cover current resource counts and concurrent guest
  operations. They do not yet meter CPU time, disk bytes, IOPS, bandwidth, or
  model spend.
- Process-wide HTTP admission is bounded. Health, readiness, and content-free
  counters remain available during saturation; access logs never use raw
  preview paths.
- Durable workspaces are project-scoped and single-writer in the current
  profile. They are host-local, not replicated, backed up, securely erased, or
  protected from the host administrator.
- Connector attachment requires the explicitly configured preview broker. Its
  signed bearer renewal is limited to a trusted-operator profile until
  proof-of-possession identity and deployment conformance are complete.
- Signed receipts are control-plane claims, not hardware attestation.
- Content telemetry is outside the control plane and is not collected by
  default.

Read [`docs/THREAT_MODEL.md`](docs/THREAT_MODEL.md) before deployment.
