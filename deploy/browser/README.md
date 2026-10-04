# Brezel browser environment

This profile turns a Brezel sandbox into a browser computer: Chromium, shell,
files, and an optional durable browser profile share one Firecracker microVM.
It is deliberately not a stealth, proxy, CAPTCHA, or scraping product.

The source image is pinned to the Linux/amd64 Playwright 1.55.0 manifest. Build
and publish this Dockerfile to a registry you control, record its resulting
digest, and use that immutable image as the input to a dedicated Brezel browser
template. The current single-host distribution installs one selected base
template; it does not claim arbitrary per-request OCI builds.

Chromium runs as an unprivileged user: `pwuser` when the launcher is invoked by
root while assembling an image, or the current sandbox user during ordinary
CLI and SDK use. Its nested namespace sandbox is disabled. The isolation
boundary for this profile is the dedicated Firecracker microVM. This avoids
depending on a host-wide Ubuntu AppArmor exception while keeping the debugging
port private and reachable only through Brezel's path-bound CDP capability. Do
not run this profile as an ordinary shared Docker container and treat it as an
equivalent security boundary.

The launcher forces Chromium through the guest's system resolver. The
qualified microVM network exposes an ordinary resolver through
`/etc/resolv.conf`; Chromium's opportunistic DNS-over-HTTPS path can remain
connected to an upstream resolver without completing requests on that path.
Disabling browser-level DoH avoids a silent navigation stall while preserving
the sandbox's normal outbound-network policy.

Recent Chromium builds keep their debugging listener on loopback even when an
external bind address is requested. A byte-transparent relay inside the guest
exposes that listener to Brezel's private node data path; it does not create a
public port. The public edge still requires a short-lived, exact-path browser
lease.

Inside a browser-qualified sandbox:

```console
brezel run sbx_01... /usr/local/bin/brezel-browser start \
  --profile /workspace/browser-profile --port 9222
brezel browser connect --ttl 180 sbx_01...
```

`browser connect` first probes Chromium's authenticated `/json/version`
endpoint, extracts the exact browser-level CDP path, and creates a short-lived
WebSocket capability for that path only. Generic HTTP previews still reject
WebSockets.

For a named-host release qualification, install `playwright-core@1.55.0` in a
disposable harness directory and pipe the short-lived connection URL to
`qualify.mjs`. Keeping the capability on stdin avoids putting it in an argument
or committed configuration:

```console
brezel browser connect --ttl 180 --json "$sandbox" \
  | jq -r .connect_url \
  | node deploy/browser/qualify.mjs probe
```

Mount a durable workspace at `/workspace` when cookies and local browser state
should outlive disposable compute. A filesystem checkpoint or fork does not
copy a live browser process or open connection. Stop Chromium or place it at an
explicit quiescence point before checkpointing profile state.

The live Chromium profile runs on the microVM's local filesystem. On
`brezel-browser start`, the launcher restores the requested durable profile
from `/workspace`; on `brezel-browser stop`, it atomically replaces that copy
after Chromium exits. This avoids putting Chromium's lock-heavy live profile
directly on a remote workspace block path. A crash can lose changes since the
last explicit stop, so callers that need durable cookies or sessions must stop
the browser at a named workflow boundary.
