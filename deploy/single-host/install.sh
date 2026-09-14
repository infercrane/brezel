#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
INSTALL_DIR=${BREZEL_INSTALL_DIR:-"$REPO_DIR/.brezel"}
ENGINE_DIR="$INSTALL_DIR/engine"
STATE_DIR="$INSTALL_DIR/state"
SECRETS_DIR="$INSTALL_DIR/secrets"
WORKSPACE_DIR="$STATE_DIR/workspaces"
LOCK_FILE="$SCRIPT_DIR/engine.lock"
ENGINE_OVERRIDE="$SCRIPT_DIR/engine.override.yaml"
ENGINE_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0001-harden-volume-secrets-and-cleanup.patch"
ENGINE_BUILD_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0002-pin-api-build-images.patch"
ENGINE_CAPABILITY_PROBE="$SCRIPT_DIR/engine-capabilities.sh"
ENGINE_IMAGE_LOCK="$SCRIPT_DIR/engine.images.lock"
ENGINE_ARTIFACT_LOCK="$SCRIPT_DIR/engine.artifacts.lock"
ARTIFACT_SUPPLY_CHAIN="$SCRIPT_DIR/artifact-supply-chain.sh"

read_lock() {
  key=$1
  sed -n "s/^${key}=//p" "$LOCK_FILE"
}

ENGINE_REPOSITORY=$(read_lock repository)
ENGINE_COMMIT=$(read_lock commit)
ENGINE_PATCH_SHA256=$(read_lock api_patch_sha256)
ENGINE_BUILD_PATCH_SHA256=$(read_lock api_build_patch_sha256)
if [ -z "$ENGINE_REPOSITORY" ] || [ -z "$ENGINE_COMMIT" ] || [ -z "$ENGINE_PATCH_SHA256" ] || [ -z "$ENGINE_BUILD_PATCH_SHA256" ]; then
  echo "invalid engine.lock" >&2
  exit 1
fi
ENGINE_SOURCE_REPOSITORY=${BREZEL_ENGINE_SOURCE_REPOSITORY:-$ENGINE_REPOSITORY}
case "$ENGINE_SOURCE_REPOSITORY" in
  ""|-*) echo "the engine source repository is invalid" >&2; exit 1 ;;
esac
BREZEL_ENGINE_ARTIFACT_BASE_URL=${BREZEL_ENGINE_ARTIFACT_BASE_URL:-https://storage.googleapis.com/e2b-artifact-binaries}
case "$BREZEL_ENGINE_ARTIFACT_BASE_URL" in
  https://*|file:///host/*) ;;
  *)
    echo "BREZEL_ENGINE_ARTIFACT_BASE_URL must use HTTPS or file:///host/<absolute-host-path>" >&2
    exit 1
    ;;
esac
export BREZEL_ENGINE_ARTIFACT_BASE_URL

if [ "$(uname -s)" != Linux ] || { [ "$(uname -m)" != x86_64 ] && [ "$(uname -m)" != amd64 ]; } || [ ! -c /dev/kvm ] || [ ! -c /dev/net/tun ]; then
  echo "This host cannot run the Firecracker profile." >&2
  echo "Use x86_64 Linux with KVM and /dev/net/tun, then run brezel doctor." >&2
  exit 1
fi
command -v git >/dev/null 2>&1 || { echo "git is required" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "Docker Engine is required" >&2; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "openssl is required" >&2; exit 1; }
command -v patch >/dev/null 2>&1 || { echo "patch is required" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required" >&2; exit 1; }
command -v tar >/dev/null 2>&1 || { echo "tar is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "Docker Compose v2 is required" >&2; exit 1; }
docker buildx version >/dev/null 2>&1 || { echo "Docker Buildx is required" >&2; exit 1; }

# Docker's Linux user parser accepts signed 32-bit IDs. Cloud OS Login and
# directory-backed identities can legitimately allocate larger host IDs, but
# passing one to --user or Compose fails only after an expensive engine build.
# Fail before downloading or mutating runtime state and require the operator to
# use a dedicated, unprivileged service account with representable IDs.
HOST_UID=$(id -u)
HOST_GID=$(id -g)
if [ "$HOST_UID" -gt 2147483647 ] || [ "$HOST_GID" -gt 2147483647 ]; then
  echo "The current host identity cannot be represented by Docker (uid=$HOST_UID gid=$HOST_GID)." >&2
  echo "Run the installer as a dedicated unprivileged service account whose UID and GID are at most 2147483647." >&2
  exit 1
fi

check_ufw_guest_network() {
  if [ "${BREZEL_SKIP_UFW_PREFLIGHT:-}" = "true" ] || ! command -v ufw >/dev/null 2>&1; then
    return
  fi

  run_ufw() {
    if [ "$(id -u)" -eq 0 ]; then
      ufw "$@"
    elif command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then
      sudo -n ufw "$@"
    else
      return 126
    fi
  }

  if ! ufw_status=$(run_ufw status verbose 2>/dev/null); then
    if grep -Eq '^ENABLED=yes$' /etc/ufw/ufw.conf 2>/dev/null; then
      echo "UFW is enabled, but the installer cannot inspect it without non-interactive sudo." >&2
      echo "Grant non-interactive inspection or apply equivalent host-scoped guest rules, then retry." >&2
      exit 1
    fi
    return
  fi
  case "$ufw_status" in
    *"Status: active"*) ;;
    *) return ;;
  esac

  guest_rule_count=$(printf '%s\n' "$ufw_status" | grep -F '10.11.0.0/24' | grep -Ec 'ALLOW (IN|FWD)' || true)
  if [ "$guest_rule_count" -lt 2 ]; then
    egress_interface=$(ip route show default 2>/dev/null | awk 'NR == 1 {for (i = 1; i <= NF; i++) if ($i == "dev") {print $(i + 1); exit}}')
    [ -n "$egress_interface" ] || egress_interface='<egress-interface>'
    echo "UFW is active and blocks the pinned engine's Firecracker guest network." >&2
    echo "Apply these host-scoped rules, then rerun the installer:" >&2
    echo "  sudo ufw allow in from 10.11.0.0/24 to any port 5010:5018 proto tcp comment 'Firecracker guest services'" >&2
    echo "  sudo ufw route allow out on $egress_interface from 10.11.0.0/24 comment 'Firecracker guest egress'" >&2
    echo "Set BREZEL_SKIP_UFW_PREFLIGHT=true only after enforcing equivalent nftables rules." >&2
    exit 1
  fi
}

check_ufw_guest_network

if [ "$(sha256sum "$ENGINE_PATCH" | awk '{print $1}')" != "$ENGINE_PATCH_SHA256" ]; then
  echo "engine API patch verification failed" >&2
  exit 1
fi
if [ "$(sha256sum "$ENGINE_BUILD_PATCH" | awk '{print $1}')" != "$ENGINE_BUILD_PATCH_SHA256" ]; then
  echo "engine API build patch verification failed" >&2
  exit 1
fi
"$ARTIFACT_SUPPLY_CHAIN" image-lock "$ENGINE_IMAGE_LOCK"

read_image_lock() {
  key=$1
  sed -n "s/^${key}=//p" "$ENGINE_IMAGE_LOCK"
}

BREZEL_ENGINE_POSTGRES_IMAGE=$(read_image_lock BREZEL_ENGINE_POSTGRES_IMAGE)
BREZEL_ENGINE_REDIS_IMAGE=$(read_image_lock BREZEL_ENGINE_REDIS_IMAGE)
BREZEL_ENGINE_CLICKHOUSE_IMAGE=$(read_image_lock BREZEL_ENGINE_CLICKHOUSE_IMAGE)
BREZEL_ENGINE_VECTOR_IMAGE=$(read_image_lock BREZEL_ENGINE_VECTOR_IMAGE)
E2B_DB_MIGRATOR_IMAGE=$(read_image_lock E2B_DB_MIGRATOR_IMAGE)
E2B_CLIENT_PROXY_IMAGE=$(read_image_lock E2B_CLIENT_PROXY_IMAGE)
E2B_CLICKHOUSE_MIGRATOR_IMAGE=$(read_image_lock E2B_CLICKHOUSE_MIGRATOR_IMAGE)
E2B_TOOLS_IMAGE=$(read_image_lock E2B_TOOLS_IMAGE)
E2B_NODE_E2B_IMAGE=$(read_image_lock E2B_NODE_E2B_IMAGE)
E2B_SEED_IMAGE=$(read_image_lock E2B_SEED_IMAGE)
export BREZEL_ENGINE_POSTGRES_IMAGE BREZEL_ENGINE_REDIS_IMAGE BREZEL_ENGINE_CLICKHOUSE_IMAGE BREZEL_ENGINE_VECTOR_IMAGE
export E2B_DB_MIGRATOR_IMAGE E2B_CLIENT_PROXY_IMAGE E2B_CLICKHOUSE_MIGRATOR_IMAGE E2B_TOOLS_IMAGE E2B_NODE_E2B_IMAGE E2B_SEED_IMAGE

umask 077
mkdir -p "$INSTALL_DIR" "$STATE_DIR" "$SECRETS_DIR" "$WORKSPACE_DIR"
chmod 700 "$INSTALL_DIR" "$STATE_DIR" "$SECRETS_DIR" "$WORKSPACE_DIR"

if [ ! -s "$SECRETS_DIR/engine-volume-token.key" ]; then
  printf 'HMAC:' > "$SECRETS_DIR/engine-volume-token.key"
  openssl rand -base64 32 | tr -d '\n' >> "$SECRETS_DIR/engine-volume-token.key"
fi
chmod 600 "$SECRETS_DIR/engine-volume-token.key"
if ! grep -Eq '^HMAC:[A-Za-z0-9+/]+={0,2}$' "$SECRETS_DIR/engine-volume-token.key"; then
  echo "the local volume signing key is malformed; move it aside and rerun the installer" >&2
  exit 1
fi
BREZEL_WORKSPACE_DIR="$WORKSPACE_DIR"
export BREZEL_WORKSPACE_DIR
BREZEL_VOLUME_TOKEN_KEY_FILE="$SECRETS_DIR/engine-volume-token.key"
export BREZEL_VOLUME_TOKEN_KEY_FILE

if [ ! -d "$ENGINE_DIR/.git" ]; then
  git clone --filter=blob:none --no-checkout "$ENGINE_SOURCE_REPOSITORY" "$ENGINE_DIR"
fi
git -C "$ENGINE_DIR" fetch --depth=1 "$ENGINE_SOURCE_REPOSITORY" "$ENGINE_COMMIT"
git -C "$ENGINE_DIR" checkout --detach "$ENGINE_COMMIT"
ACTUAL_COMMIT=$(git -C "$ENGINE_DIR" rev-parse HEAD)
if [ "$ACTUAL_COMMIT" != "$ENGINE_COMMIT" ]; then
  echo "engine revision verification failed" >&2
  exit 1
fi
if [ -n "$(git -C "$ENGINE_DIR" status --porcelain)" ]; then
  echo "engine checkout contains uncommitted changes; refusing to run unverified source" >&2
  exit 1
fi
"$ARTIFACT_SUPPLY_CHAIN" source "$ENGINE_DIR" "$LOCK_FILE" "$ENGINE_IMAGE_LOCK" "$ENGINE_ARTIFACT_LOCK"

# Pull exact manifests once, or require an operator-preloaded image set. The
# compose override uses pull_policy: never, so startup cannot silently replace
# these bytes after this gate.
"$ARTIFACT_SUPPLY_CHAIN" images "$ENGINE_IMAGE_LOCK" "${BREZEL_ENGINE_IMAGE_MODE:-pull}"

ENGINE_BUILD_DIR=$(mktemp -d "$INSTALL_DIR/engine-build.XXXXXX")
cleanup_build_dir() {
  case "$ENGINE_BUILD_DIR" in
    "$INSTALL_DIR"/engine-build.*) rm -rf -- "$ENGINE_BUILD_DIR" ;;
  esac
}
trap cleanup_build_dir EXIT HUP INT TERM
git -C "$ENGINE_DIR" archive "$ENGINE_COMMIT" | tar -xf - -C "$ENGINE_BUILD_DIR"
patch -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_PATCH"
patch -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_BUILD_PATCH"
"$ENGINE_CAPABILITY_PROBE" source "$ENGINE_BUILD_DIR"
EXPECTED_MIGRATION_TIMESTAMP=$(find "$ENGINE_BUILD_DIR/packages/db/migrations" -maxdepth 1 -type f -printf '%f\n' | sed 's/_.*//' | sort | tail -n 1)
if [ -z "$EXPECTED_MIGRATION_TIMESTAMP" ]; then
  echo "could not resolve the pinned engine migration version" >&2
  exit 1
fi
BREZEL_ENGINE_API_IMAGE="brezel/engine-api:${ENGINE_COMMIT}-hardening-v1"
docker build \
  -t "$BREZEL_ENGINE_API_IMAGE" \
  -f "$ENGINE_BUILD_DIR/packages/api/Dockerfile" \
  --build-arg "COMMIT_SHA=${ENGINE_COMMIT}-hardening-v1" \
  --build-arg "VERSION=${ENGINE_COMMIT}-hardening-v1" \
  --build-arg "EXPECTED_MIGRATION_TIMESTAMP=$EXPECTED_MIGRATION_TIMESTAMP" \
  "$ENGINE_BUILD_DIR/packages"
export BREZEL_ENGINE_API_IMAGE

ENGINE_COMPOSE="$ENGINE_DIR/embed/compose/compose.yaml"
ENGINE_ENV="$ENGINE_DIR/embed/compose/.env"
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" config >/dev/null
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" up -d --wait

# The upstream fetcher also verifies these downloads. Verify them again using
# product-owned lock data rather than trusting checksums embedded only in the
# tools image, then write the exact installed distribution record.
"$ARTIFACT_SUPPLY_CHAIN" host "$ENGINE_ARTIFACT_LOCK" /
"$ARTIFACT_SUPPLY_CHAIN" manifest "$INSTALL_DIR/distribution.manifest" "$LOCK_FILE" "$ENGINE_IMAGE_LOCK" "$ENGINE_ARTIFACT_LOCK" /

docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" \
  exec -T ready sh -c 'cat /run/e2b/team-api-key' > "$SECRETS_DIR/engine.token"
if [ ! -s "$SECRETS_DIR/engine.token" ]; then
  echo "the local engine did not produce an access token" >&2
  exit 1
fi

if [ ! -s "$SECRETS_DIR/service.token" ]; then
  openssl rand -hex 32 | tr -d '\n' > "$SECRETS_DIR/service.token"
fi
SERVICE_TOKEN=$(tr -d '\r\n' < "$SECRETS_DIR/service.token")
if [ "${#SERVICE_TOKEN}" -lt 32 ]; then
  echo "the runtime service token must contain at least 32 characters" >&2
  exit 1
fi
TOKEN_TMP=$(mktemp "$SECRETS_DIR/.service-token.XXXXXX")
printf '%s' "$SERVICE_TOKEN" > "$TOKEN_TMP"
chmod 600 "$TOKEN_TMP"
mv -f -- "$TOKEN_TMP" "$SECRETS_DIR/service.token"
SERVICE_TOKEN_SHA256=$(sha256sum "$SECRETS_DIR/service.token" | awk '{print $1}')
ACCESS_POLICY_TMP=$(mktemp "$SECRETS_DIR/.access-policy.XXXXXX")
cat > "$ACCESS_POLICY_TMP" <<EOF
{"version":1,"principals":[{"name":"single-host-operator","token_sha256":"$SERVICE_TOKEN_SHA256","projects":["brezel-default","brezel-conformance","brezel-conformance-isolation","brezel-benchmark"]}]}
EOF
chmod 600 "$ACCESS_POLICY_TMP"
mv -f -- "$ACCESS_POLICY_TMP" "$SECRETS_DIR/access-policy.json"
BREZEL_IMAGE=$(docker build -q -f "$REPO_DIR/Dockerfile" "$REPO_DIR")
if [ ! -s "$SECRETS_DIR/receipt.key" ]; then
  docker run --rm \
    --user "$(id -u):$(id -g)" \
    -v "$SECRETS_DIR:/secrets" \
    --entrypoint /usr/local/bin/brezeld \
    "$BREZEL_IMAGE" \
    keygen -out /secrets/receipt.key
fi

if { [ -s "$SECRETS_DIR/node-capability.key" ] && [ ! -s "$SECRETS_DIR/node-capability.pub" ]; } || \
   { [ ! -s "$SECRETS_DIR/node-capability.key" ] && [ -s "$SECRETS_DIR/node-capability.pub" ]; }; then
  echo "node capability key pair is incomplete; restore its matching file before continuing" >&2
  exit 1
fi
if [ ! -s "$SECRETS_DIR/node-capability.key" ]; then
  docker run --rm \
    --user "$(id -u):$(id -g)" \
    -v "$SECRETS_DIR:/secrets" \
    --entrypoint /usr/local/bin/brezeld \
    "$BREZEL_IMAGE" \
    keygen -out /secrets/node-capability.key -public-out /secrets/node-capability.pub
fi
CAPABILITY_PUBLIC_KEY=$(tr -d '\r\n' < "$SECRETS_DIR/node-capability.pub")
CAPABILITY_POLICY_TMP=$(mktemp "$SECRETS_DIR/.node-capability-keys.XXXXXX")
printf '{"version":1,"issuer":"brezel-api","keys":[{"id":"node-key-1","public_key_base64":"%s"}]}\n' "$CAPABILITY_PUBLIC_KEY" > "$CAPABILITY_POLICY_TMP"
chmod 600 "$CAPABILITY_POLICY_TMP"
mv -f -- "$CAPABILITY_POLICY_TMP" "$SECRETS_DIR/node-capability-keys.json"

NODE_TLS_RENEW=false
for tls_file in node-ca.crt node-ca.key node.crt node.key api.crt api.key; do
  if [ ! -s "$SECRETS_DIR/$tls_file" ]; then
    NODE_TLS_RENEW=true
  fi
done
if [ "$NODE_TLS_RENEW" = false ] && ! openssl x509 -checkend 604800 -noout -in "$SECRETS_DIR/node.crt" >/dev/null 2>&1; then
  NODE_TLS_RENEW=true
fi
if [ "$NODE_TLS_RENEW" = false ] && ! openssl x509 -checkend 604800 -noout -in "$SECRETS_DIR/api.crt" >/dev/null 2>&1; then
  NODE_TLS_RENEW=true
fi
if [ "$NODE_TLS_RENEW" = true ]; then
  NODE_TLS_DIR=$(mktemp -d "$SECRETS_DIR/.node-tls.XXXXXX")
  cleanup_node_tls() {
    case "$NODE_TLS_DIR" in
      "$SECRETS_DIR"/.node-tls.*) rm -rf -- "$NODE_TLS_DIR" ;;
    esac
  }
  trap 'cleanup_node_tls; cleanup_build_dir' EXIT HUP INT TERM
  openssl ecparam -name prime256v1 -genkey -noout -out "$NODE_TLS_DIR/node-ca.key"
  openssl req -x509 -new -sha256 -key "$NODE_TLS_DIR/node-ca.key" -days 365 \
    -subj '/CN=Brezel node CA' -out "$NODE_TLS_DIR/node-ca.crt"
  openssl ecparam -name prime256v1 -genkey -noout -out "$NODE_TLS_DIR/node.key"
  openssl req -new -sha256 -key "$NODE_TLS_DIR/node.key" -subj '/CN=node-a.internal' -out "$NODE_TLS_DIR/node.csr"
  printf '%s\n' \
    'basicConstraints=critical,CA:FALSE' \
    'keyUsage=critical,digitalSignature,keyAgreement' \
    'extendedKeyUsage=serverAuth' \
    'subjectAltName=DNS:node-a.internal,IP:127.0.0.1,URI:spiffe://brezel/node/node-a' \
    > "$NODE_TLS_DIR/node.ext"
  openssl x509 -req -sha256 -in "$NODE_TLS_DIR/node.csr" -CA "$NODE_TLS_DIR/node-ca.crt" \
    -CAkey "$NODE_TLS_DIR/node-ca.key" -CAcreateserial -days 30 -extfile "$NODE_TLS_DIR/node.ext" \
    -out "$NODE_TLS_DIR/node.crt"
  openssl ecparam -name prime256v1 -genkey -noout -out "$NODE_TLS_DIR/api.key"
  openssl req -new -sha256 -key "$NODE_TLS_DIR/api.key" -subj '/CN=api-a' -out "$NODE_TLS_DIR/api.csr"
  printf '%s\n' \
    'basicConstraints=critical,CA:FALSE' \
    'keyUsage=critical,digitalSignature,keyAgreement' \
    'extendedKeyUsage=clientAuth' \
    'subjectAltName=URI:spiffe://brezel/api/api-a' \
    > "$NODE_TLS_DIR/api.ext"
  openssl x509 -req -sha256 -in "$NODE_TLS_DIR/api.csr" -CA "$NODE_TLS_DIR/node-ca.crt" \
    -CAkey "$NODE_TLS_DIR/node-ca.key" -CAcreateserial -days 30 -extfile "$NODE_TLS_DIR/api.ext" \
    -out "$NODE_TLS_DIR/api.crt"
  chmod 600 "$NODE_TLS_DIR/node-ca.crt" "$NODE_TLS_DIR/node-ca.key" "$NODE_TLS_DIR/node.crt" "$NODE_TLS_DIR/node.key" "$NODE_TLS_DIR/api.crt" "$NODE_TLS_DIR/api.key"
  for tls_file in node-ca.crt node-ca.key node.crt node.key api.crt api.key; do
    mv -f -- "$NODE_TLS_DIR/$tls_file" "$SECRETS_DIR/$tls_file"
  done
  cleanup_node_tls
  trap cleanup_build_dir EXIT HUP INT TERM
fi

chmod 600 "$SECRETS_DIR/engine.token" "$SECRETS_DIR/service.token" "$SECRETS_DIR/access-policy.json" \
  "$SECRETS_DIR/receipt.key" "$SECRETS_DIR/node-capability.key" "$SECRETS_DIR/node-capability.pub" \
  "$SECRETS_DIR/node-capability-keys.json" "$SECRETS_DIR/node-ca.crt" "$SECRETS_DIR/node-ca.key" \
  "$SECRETS_DIR/node.crt" "$SECRETS_DIR/node.key" "$SECRETS_DIR/api.crt" "$SECRETS_DIR/api.key"

BREZEL_STATE_DIR="$STATE_DIR" BREZEL_SECRETS_DIR="$SECRETS_DIR" \
BREZEL_UID="$(id -u)" BREZEL_GID="$(id -g)" \
  docker compose -f "$SCRIPT_DIR/compose.yaml" up -d --build --wait

"$SCRIPT_DIR/qualify.sh"

echo "Brezel API is listening on http://127.0.0.1:8080"
echo "Read the local CLI token from $SECRETS_DIR/service.token"
