# Repository instructions

This repository defines a self-hosted agent runtime and a security boundary.
Product convenience must not weaken the declared assurance model.

## Before changing code or contracts

- Read `README.md`, `docs/ARCHITECTURE.md`, and `docs/THREAT_MODEL.md` fully.
- Preserve the product boundary in ADR 0001 and substrate decision in ADR 0002.
- Treat schemas under `spec/v1alpha1/` as versioned public contracts.
- Check the working tree and preserve unrelated user changes.

## Non-negotiable invariants

- Firecracker microVMs are the default hostile-code boundary; plain containers
  cannot claim equivalent multi-tenant isolation.
- Fail before execution when a requested capability cannot be enforced.
- Never translate an unknown backend or lifecycle state into success.
- Keep sandbox standby, archive, expiration, and deletion as distinct states.
- Never place long-lived credentials in sandbox files, images, command lines,
  ordinary environment variables, logs, events, snapshots, or receipts.
- Do not collect prompt, response, source, terminal, or artifact content by
  default.
- Do not label a signed runtime claim as hardware attestation.
- Bind sandbox, policy, backend, image, input, output, and cleanup identities in
  the receipt.
- Keep model access separate from general network access.
- Make deletion and resource cleanup durable, retryable, and visible.

## Implementation rules

- Extend or contribute to the maintained E2B Runtime before forking its data
  plane. Pin the exact upstream revision used by a release.
- Do not copy proprietary Blaxel code or private API behavior. Public product
  concepts can inform clean-room interfaces.
- Prefer adapters to maintained storage, identity, policy, and networking
  systems over in-repository reinvention.
- Add a conformance test before advertising a capability or compatibility claim.
- Keep fake drivers clearly named and impossible to enable in release builds.
- Use opaque secret handles and endpoint-bound, short-lived leases.
- Treat observed benchmark results as environment-specific evidence, never as a
  universal performance claim.
- Treat policy observation or learning as advice only until a human or
  deterministic organization policy approves it.

## Documentation

Update architecture, threat model, schema, examples, and an ADR together when a
change moves a trust boundary, alters a lifecycle state, or changes an assurance
claim.
