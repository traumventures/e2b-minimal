#!/usr/bin/env bash
# The whole stack in one container: prepare the host, start the released
# orchestrator (which runs in the host's namespaces, like in E2B Embed) and
# serve the minimal API in front of it.
set -euo pipefail

S=/opt/e2b/scripts
: "${E2B_API_KEY:?set E2B_API_KEY, e.g. e2b_\$(openssl rand -hex 20)}"

log() { echo "entrypoint: $*"; }

if [ "${SKIP_HOST_SETUP:-0}" != 1 ]; then
  log "preflight"
  nsenter -t 1 -m -u -i -n -p -- /bin/bash -s < "$S/preflight.sh"
  log "host setup (nbd, tun, hugepages=${HUGEPAGES:-2048})"
  nsenter -t 1 -m -u -i -n -p -- /bin/bash -s < "$S/host-setup.sh"
  log "fetching orchestrator, envd, kernel, firecracker, busybox"
  HOST_ROOT=/host bash "$S/fetch-artifacts.sh"
fi

# Orchestrator settings: E2B Embed's, without Redis, ClickHouse and the log
# collector. Paths are on the host; the binary runs in its mount namespace.
orchestrator_env=(
  ORCHESTRATOR_SERVICES=orchestrator,template-manager
  NODE_ID=orchestrator
  NODE_IP=127.0.0.1
  ENVIRONMENT=local
  GRPC_PORT="${ORCHESTRATOR_GRPC_PORT:-5008}"
  PROXY_PORT="${ORCHESTRATOR_PROXY_PORT:-5007}"
  PPROF_PORT="${ORCHESTRATOR_PPROF_PORT:-6061}"
  TEMPLATE_STORAGE_URL=file:///var/lib/e2b/storage/templates
  BUILD_CACHE_STORAGE_URL=file:///var/lib/e2b/storage/build-cache
  PAUSE_ADMISSION_DISK_HEADROOM_MIB=1024
  ARTIFACTS_REGISTRY_PROVIDER=Local
  LOCAL_UPLOAD_BASE_URL="http://127.0.0.1:${ORCHESTRATOR_GRPC_PORT:-5008}"
  FIRECRACKER_VERSIONS_DIR=/fc-versions
  HOST_KERNELS_DIR=/fc-kernels
  HOST_BUSYBOX_DIR=/fc-busybox
  BUSYBOX_VERSION="$E2B_BUSYBOX_VERSION"
  HOST_ENVD_PATH=/fc-envd/envd
  ORCHESTRATOR_BASE_PATH=/orchestrator
  SANDBOX_DIR=/fc-vm
  NBD_POOL_SIZE="${NBD_POOL_SIZE:-32}"
  NETWORK_VERSION=1
  DEFAULT_KERNEL_VERSION="$E2B_KERNEL_VERSION"
  DEFAULT_FIRECRACKER_VERSION="$E2B_FIRECRACKER_VERSION"
)

log "starting orchestrator"
env -i PATH="$PATH" "${orchestrator_env[@]}" bash "$S/orchestrator-launch.sh" &
orch=$!

log "starting api on ${LISTEN_ADDR:-:3000}"
ORCHESTRATOR_GRPC="127.0.0.1:${ORCHESTRATOR_GRPC_PORT:-5008}" \
ORCHESTRATOR_PROXY="127.0.0.1:${ORCHESTRATOR_PROXY_PORT:-5007}" \
DATA_DIR="${DATA_DIR:-/host/var/lib/e2b/minimal}" \
  e2b-minimal &
api=$!

stop() {
  log "stopping"
  kill -TERM "$api" 2>/dev/null || true
  # orchestrator-launch ends every sandbox before the orchestrator drains.
  kill -TERM "$orch" 2>/dev/null || true
  wait "$api" 2>/dev/null || true
  wait "$orch" 2>/dev/null || true
}
trap 'stop; exit 0' TERM INT

set +e
wait -n "$orch" "$api"
code=$?
set -e
log "a process exited with $code, shutting down"
stop
exit "$code"
