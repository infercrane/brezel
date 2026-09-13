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

- Release execution only supports an explicitly configured E2B Runtime API.
- The adapter requires Firecracker isolation as a deployment prerequisite, but
  a specific deployment is not qualified until the conformance suite passes.
- The local file state store is a single-controller developer profile, not a
  clustered production database.
- Connector definitions are registrable, but attachment fails before sandbox
  creation until the credential broker is implemented and qualified.
- Signed receipts are control-plane claims, not hardware attestation.
- Content telemetry is outside the control plane and is not collected by
  default.

Read [`docs/THREAT_MODEL.md`](docs/THREAT_MODEL.md) before deployment.

