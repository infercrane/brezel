ARG BASE_IMAGE=node:24.14.1-bookworm@sha256:80fc934952c8f1b2b4d39907af7211f8a9fff1a4c2cf673fb49099292c251cec
FROM ${BASE_IMAGE}

# General agent-development tools, not benchmark inputs. Keep the signed apt
# indexes in the immutable image so a fresh sandbox can cheaply revalidate them
# instead of downloading every index before its first useful command.
RUN apt-get update -qq \
    && env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
      bash \
      build-essential \
      ca-certificates \
      curl \
      git \
      python3 \
      python3-setuptools \
      unzip \
    && test -x /usr/local/lib/node_modules/npm/node_modules/node-gyp/bin/node-gyp.js \
    && ln -s /usr/local/lib/node_modules/npm/node_modules/node-gyp/bin/node-gyp.js /usr/local/bin/node-gyp \
    && command -v node \
    && command -v node-gyp \
    && command -v git \
    && command -v python3

LABEL org.opencontainers.image.title="Brezel agent development base"
LABEL org.opencontainers.image.description="Node plus common source-build tools; no benchmark source or dependency cache"
