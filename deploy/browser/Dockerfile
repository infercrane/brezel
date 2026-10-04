# syntax=docker/dockerfile:1.7

# The amd64 manifest is pinned independently from the mutable multi-platform
# tag. Update it only with the browser qualification evidence for that release.
FROM mcr.microsoft.com/playwright:v1.55.0-noble@sha256:ffc33305f7b4b04057ae4a0caa70aad4fde87454fb403a1a22e7f931707dfcf9

USER root

COPY launch-browser.sh /usr/local/bin/brezel-browser

RUN set -eux; \
    browser="$(find /ms-playwright -type f \( -path '*/chrome-linux/chrome' -o -path '*/chrome-linux64/chrome' \) -print -quit)"; \
    test -n "$browser"; \
    ln -s "$browser" /usr/local/bin/brezel-chromium; \
    chmod 0755 /usr/local/bin/brezel-browser; \
    install -d -o pwuser -g pwuser -m 0700 /home/pwuser/.cache /workspace

LABEL org.opencontainers.image.source="https://github.com/infercrane/brezel" \
      org.opencontainers.image.description="Pinned Chromium environment for Brezel browser computers"

