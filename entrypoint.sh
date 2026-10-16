#!/usr/bin/env bash
# The whole stack in one container: prepare the host, start the orchestrator
# (which runs in the host's namespaces, like in E2B Embed) and serve the
# minimal API in front of it.
set -euo pipefail

S=/opt/e2b/scripts

log() { echo "entrypoint: $*"; }
die() { echo "entrypoint: $*" >&2; exit 1; }

# Everything that is not object storage lives under one host directory, so it
# can be its own disk or mount.
DATA="${E2B_DATA_DIR:-/var/lib/e2b}"
case "$DATA" in /*) ;; *) die "E2B_DATA_DIR must be an absolute host path" ;; esac
mkdir -p "/host$DATA"/{storage/templates,storage/build-cache,orchestrator,fc-vm,minimal}

# Memory mode. regular (default): sandboxes use ordinary host memory on
# demand, guest-freed memory goes back to the host, and SANDBOX_ALL_MEMORY_MAX
# caps all sandboxes together. hugepages: E2B's default, every sandbox draws
# from a fixed pool of 2 MiB pages reserved at start (HUGEPAGES).
MEMORY_MODE="${MEMORY_MODE:-regular}"
case "$MEMORY_MODE" in
  regular) template_huge_pages=false; HUGEPAGES=1 ;;
  hugepages) template_huge_pages=true; HUGEPAGES="${HUGEPAGES:-2048}" ;;
  *) die "MEMORY_MODE must be regular or hugepages" ;;
esac
export HUGEPAGES

if [ "${SKIP_HOST_SETUP:-0}" != 1 ]; then
  log "preflight"
  nsenter -t 1 -m -u -i -n -p -- /bin/bash -s < "$S/preflight.sh"
  log "host setup (nbd, tun, hugepages=$HUGEPAGES)"
  nsenter -t 1 -m -u -i -n -p -- /bin/env HUGEPAGES="$HUGEPAGES" /bin/bash -s < "$S/host-setup.sh"
  log "fetching envd, kernel, firecracker, busybox"
  HOST_ROOT=/host bash "$S/fetch-artifacts.sh"
fi

# The orchestrator is built into this image (see orchestrator/); copy it to
# the host, where it runs.
install -D -m 0755 /opt/e2b/bin/orchestrator /host/var/lib/e2b/bin/orchestrator

# Limits for all sandboxes together, on the parent cgroup the orchestrator
# puts every sandbox under. Unset means no limit.
CG=/sys/fs/cgroup/e2b
mkdir -p "$CG"
echo "+cpu +memory" > "$CG/cgroup.subtree_control"
if [ -n "${SANDBOX_ALL_CPU_MAX:-}" ]; then
  echo "$((SANDBOX_ALL_CPU_MAX * 100000)) 100000" > "$CG/cpu.max"
else
  echo "max 100000" > "$CG/cpu.max"
fi
mem_max=max
if [ -n "${SANDBOX_ALL_MEMORY_MAX:-}" ]; then mem_max=$((SANDBOX_ALL_MEMORY_MAX * 1024 * 1024)); fi
echo "$mem_max" > "$CG/memory.max"
echo 0 > "$CG/memory.swap.max" 2>/dev/null || true
log "all sandboxes together: cpu.max=$(cat "$CG/cpu.max") memory.max=$(cat "$CG/memory.max")"

storage_url() { # bucket local-dir
  if [ -n "$1" ]; then
    local q="s3ForcePathStyle=${S3_FORCE_PATH_STYLE:-true}"
    [ -n "${S3_ENDPOINT:-}" ] && q="$q&endpoint=$S3_ENDPOINT"
    [ -n "${S3_REGION:-}" ] && q="$q&region=$S3_REGION"
    echo "s3://$1?$q"
  else
    echo "file://$2"
  fi
}
template_storage="$(storage_url "${S3_BUCKET:-}" "$DATA/storage/templates")"
build_cache_storage="$(storage_url "${S3_BUILD_CACHE_BUCKET:-}" "$DATA/storage/build-cache")"
log "template storage: ${template_storage%%\?*}, build cache: ${build_cache_storage%%\?*}"

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
  TEMPLATE_STORAGE_URL="$template_storage"
  BUILD_CACHE_STORAGE_URL="$build_cache_storage"
  PAUSE_ADMISSION_DISK_HEADROOM_MIB=1024
  ARTIFACTS_REGISTRY_PROVIDER=Local
  LOCAL_UPLOAD_BASE_URL="http://127.0.0.1:${ORCHESTRATOR_GRPC_PORT:-5008}"
  FIRECRACKER_VERSIONS_DIR=/fc-versions
  HOST_KERNELS_DIR=/fc-kernels
  HOST_BUSYBOX_DIR=/fc-busybox
  BUSYBOX_VERSION="$E2B_BUSYBOX_VERSION"
  HOST_ENVD_PATH=/fc-envd/envd
  ORCHESTRATOR_BASE_PATH="$DATA/orchestrator"
  ORCHESTRATOR_LOCK_PATH="$DATA/orchestrator.lock"
  SANDBOX_DIR="$DATA/fc-vm"
  NBD_POOL_SIZE="${NBD_POOL_SIZE:-32}"
  NETWORK_VERSION=1
  DEFAULT_KERNEL_VERSION="$E2B_KERNEL_VERSION"
  DEFAULT_FIRECRACKER_VERSION="$E2B_FIRECRACKER_VERSION"
  TEMPLATE_HUGE_PAGES="$template_huge_pages"
  TEMPLATE_FREE_PAGE_REPORTING=true
)
for v in AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_REGION AWS_ENDPOINT_URL; do
  if [ -n "${!v:-}" ]; then orchestrator_env+=("$v=${!v}"); fi
done

log "starting orchestrator"
env -i PATH="$PATH" "${orchestrator_env[@]}" bash "$S/orchestrator-launch.sh" &
orch=$!

# The memory promise per sandbox only works on regular memory: hugepages are
# not part of the memory cgroup.
if [ "$MEMORY_MODE" = regular ]; then
  api_huge_pages=false; memory_min="${SANDBOX_MEMORY_MIN:-256}"
else
  api_huge_pages=true; memory_min=0
fi
log "starting api on ${LISTEN_ADDR:-:3000}"
ORCHESTRATOR_GRPC="127.0.0.1:${ORCHESTRATOR_GRPC_PORT:-5008}" \
ORCHESTRATOR_PROXY="127.0.0.1:${ORCHESTRATOR_PROXY_PORT:-5007}" \
DATA_DIR="/host$DATA/minimal" \
HUGE_PAGES="$api_huge_pages" \
DEFAULT_CPU_COUNT="${DEFAULT_CPU_COUNT:-$(nproc)}" \
SANDBOX_CGROUP_ROOT="$CG" \
SANDBOX_MEMORY_MIN="$memory_min" \
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
