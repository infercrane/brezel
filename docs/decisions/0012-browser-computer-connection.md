# ADR 0012: path-bound browser computer connections

Status: accepted; named-host qualification completed 2026-10-04

## Context

Coding and research agents need a real browser inside the same isolated Linux
computer as their tools and files. The existing HTTP preview deliberately
rejects WebSockets, while Chrome DevTools Protocol (CDP) requires a long-lived
bidirectional WebSocket. Turning every preview into a general WebSocket or TCP
tunnel would unnecessarily widen Brezel's public ingress boundary.

## Decision

Brezel exposes a separate browser-connection lease. The service first probes
Chromium's authenticated internal `/json/version` endpoint, validates the
reported `webSocketDebuggerUrl`, and pins the lease to that exact
`/devtools/browser/{id}` path. The public endpoint accepts only a WebSocket
upgrade for the opaque lease and exact path.

The lease lasts at most five minutes and authorizes at most 256 MiB in each
direction. The node relay receives a generation-bound, single-operation
`port.websocket` capability. Route and guest-operation leases remain held for
the life of the connection. Substrate routing credentials stay on the private
node-to-engine hop.

Browser profile data may live in an explicitly mounted durable workspace. The
live profile runs on the microVM-local filesystem because Chromium's
lock-heavy active profile stalled when placed directly on the workspace block
path. An explicit browser stop atomically checkpoints the quiesced profile to
the workspace; a start restores it locally. Stopping compute does not promise
to preserve a live Chromium process, open sockets, timers, external sessions,
or changes since the last explicit browser stop.

## Consequences

- Generic HTTP previews continue to reject WebSockets.
- Browser automation clients can connect with Playwright or CDP without a
  public Chrome debugging port.
- The first browser environment is a pinned Playwright/Chromium image and runs
  Chromium as an unprivileged user, including when launched directly through a
  normal sandbox command. Chromium's nested namespace sandbox is disabled
  because the browser already runs inside a dedicated Firecracker microVM and
  Ubuntu 24.04 otherwise requires a host-installed AppArmor policy. The
  microVM, not a shared container, is the isolation boundary.
- Managed residential proxies, CAPTCHA bypass, fingerprint spoofing, session
  recording, and arbitrary public TCP tunnels are outside this decision.
- Named-host qualification covers a real public Playwright workflow, exact-path
  rejection, generic-preview WebSocket rejection, lease expiry, per-sandbox
  lease overload, API loss, private-node loss and recovery, cross-microVM
  profile persistence, and confirmed cleanup. The cumulative byte bound is
  covered by the same bridge's deterministic unit test rather than transferring
  256 MiB through the production edge merely to trigger the limit. The
  sanitized receipt is under
  `evidence/browser-computer-qualification-2026-10-04/`.
