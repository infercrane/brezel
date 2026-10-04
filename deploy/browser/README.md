# Brezel browser environment

This profile turns a Brezel sandbox into a browser computer: Chromium, shell,
files, and an optional durable browser profile share one Firecracker microVM.
It is deliberately not a stealth, proxy, CAPTCHA, or scraping product.

The source image is pinned to the Linux/amd64 Playwright 1.55.0 manifest. Build
and publish this Dockerfile to a registry you control, record its resulting
digest, and use that immutable image as the input to a dedicated Brezel browser
template. The current single-host distribution installs one selected base
template; it does not claim arbitrary per-request OCI builds.

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

Mount a durable workspace at `/workspace` when cookies and local browser state
should outlive disposable compute. A filesystem checkpoint or fork does not
copy a live browser process or open connection. Stop Chromium or place it at an
explicit quiescence point before checkpointing profile state.
