# Browser computer named-host qualification

This receipt covers Brezel's browser-computer boundary on the public
single-host installation. It is a protocol, lifecycle, and recovery
qualification, not a browser benchmark.

## Qualified identity

- Browser artifact source: `7fa8447d0fded17c173eaa0d8bf9a40fe5eee82b`
- API and private-node source during qualification:
  `a0ba9a8a9c7b9ed40106bc932c41d97e6c10dad6`
- Immutable engine template:
  `4evni17gmj8dnk772wvk:dca5ac63-cb3e-4394-bee4-a961b3eb8595`
- Environment revision: `envr_40dfb123094394b68fa73816`
- Source image:
  `mcr.microsoft.com/playwright:v1.55.0-noble@sha256:ffc33305f7b4b04057ae4a0caa70aad4fde87454fb403a1a22e7f931707dfcf9`
- Browser: Chrome 140.0.7339.16, CDP 1.3
- Guest: 4 vCPU, 8 GiB requested memory, 40+ GB root device
- Host: Scaleway EM-B230E-NVME, AMD EPYC 4345P, 8 physical cores,
  64 GB RAM

The browser launcher and relay were embedded from the source commit by
`deploy/browser/build-template.mjs`; no mutable template alias was used by the
environment revision.

## Result

Playwright 1.55 connected through the public HTTPS endpoint, navigated to
`https://example.com/`, and observed `Example Domain`. Local storage was then
written, Chromium was explicitly stopped, and the first microVM was deleted. A
second microVM from the same immutable environment mounted the same workspace,
restored the quiesced profile, and read the stored value successfully.

Live negative and recovery tests passed:

- a path-mismatched browser capability returned HTTP 404;
- the ordinary preview route rejected a WebSocket upgrade with HTTP 501;
- a 30-second browser capability could not reconnect after expiry;
- restarting the API invalidated the old in-memory capability, while a new
  capability completed the same Playwright probe;
- the first 16 browser capabilities were admitted and the next was rejected
  with `quota_exceeded`;
- stopping the private node made new connections fail closed, and starting the
  node restored a successful Playwright probe without replacing the guest;
- the pinned OCI artifact completed start, metadata, stop, persisted-profile,
  restart, and stop smokes when invoked as root and as `pwuser`;
- both microVMs and the qualification workspace were confirmed deleted;
- the final active-sandbox count was zero.

The 256 MiB cumulative tunnel bound is covered by
`TestBridgeRejectsCumulativeBudgetOverrun`. The live qualification exercises
the same `websocketbridge.Bridge` path but does not transfer 256 MiB only to
trigger the deterministic limit.

## Findings retained

Qualification found and fixed three real boundary failures:

1. Ubuntu 24.04 denied Chromium's nested unprivileged user namespace. The
   browser therefore runs unprivileged with `--no-sandbox` inside a dedicated
   Firecracker microVM; this profile is not approved for shared-container use.
2. Chromium kept CDP on loopback. A byte-transparent guest relay now exposes it
   only to Brezel's private data path; the public edge still requires an
   opaque, short-lived, exact-path capability.
3. A live Chrome profile on the durable workspace block path stalled network
   navigation. The launcher now restores it to microVM-local storage and
   atomically checkpoints it back only after an explicit stop. Browser-level
   DNS-over-HTTPS is also disabled in favor of the guest's qualified system
   resolver.

These are part of the product contract, not benchmark-specific switches.
