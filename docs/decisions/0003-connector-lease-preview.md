# ADR 0003: short-lived connector lease preview

- Status: accepted for the developer preview
- Date: 2026-09-13

## Context

Agents need private model and tool access, but putting a provider credential in
an image, file, ordinary environment variable, process argument, snapshot, or
receipt would violate the product boundary. The audited E2B Runtime revision has
network controls and a workload-identity configuration surface, but its public
contract does not yet give this control plane a complete renewable identity and
credential-broker protocol.

## Decision

The preview implements a project-owned connector gateway. A Sandbox carrying
connector revisions receives a signed lease with a maximum lifetime of fifteen
minutes; the default is five. The lease binds project, sandbox, connector
revisions, issue time, expiry, and nonce. The long-lived secret remains in an
external resolver and is injected only by the gateway after current sandbox
state and route policy are checked.

The E2B sandbox is configured with deny-by-default internet access and an
explicit route to the connector gateway. The gateway independently enforces
HTTPS destination, method, path, request and response size, redirect, DNS,
metadata, header, and credential-response controls. Private address space needs
an explicit connector setting; loopback, link-local, and known metadata
addresses remain blocked.

The development resolver reads only non-symlink regular files with mode 0600.
Vault, cloud secret managers, and SPIFFE-bound renewal are follow-up adapters.

## Consequences

The sandbox never receives the long-lived provider credential. A captured lease
has a bounded lifetime and stops authorizing once the sandbox leaves `running`.
The current renewal endpoint is bearer-based, so a stolen unexpired lease can be
renewed while the sandbox remains active. This preview therefore does not carry
the `private-single-tenant` or hostile shared-multitenant assurance label.

Responses are buffered to enforce a size cap and scrub exact credential bytes;
streaming inference needs a separately tested streaming scrubber. The gateway is
not a generic network proxy and supports bearer injection only in this revision.

This decision must be revisited when substrate workload identity can provide
proof-of-possession renewal without a durable guest secret.
