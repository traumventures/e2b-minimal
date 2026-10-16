#!/usr/bin/env bash
# Runs the official JS and Python SDKs against the API with a fake
# orchestrator: checks the REST contract without KVM. Needs go, node (or bun)
# and python3 with `pip install e2b`, plus `npm install` in test/sdk.
set -euo pipefail
cd "$(dirname "$0")/.."

work="$(mktemp -d)"
trap 'kill $(jobs -p) 2>/dev/null || true; rm -r "$work"' EXIT

go build -race -o "$work/e2b-minimal" ./cmd/e2b-minimal
go build -o "$work/fakeorch" ./test/fakeorch

"$work/fakeorch" -addr 127.0.0.1:15008 -http 127.0.0.1:15009 > "$work/fake.log" 2>&1 &
sleep 1
export E2B_API_KEY="e2b_$(openssl rand -hex 20)"
ORCHESTRATOR_GRPC=127.0.0.1:15008 ORCHESTRATOR_HTTP=127.0.0.1:15009 \
ORCHESTRATOR_PROXY=127.0.0.1:15007 LISTEN_ADDR=127.0.0.1:3991 \
DATA_DIR="$work/data" E2B_KERNEL_VERSION=test \
  "$work/e2b-minimal" > "$work/api.log" 2>&1 &

export E2B_API_URL=http://127.0.0.1:3991 E2B_SANDBOX_URL=http://127.0.0.1:3991
for _ in $(seq 30); do
  curl -fsS -H "X-API-Key: $E2B_API_KEY" "$E2B_API_URL/templates" | grep -q '"ready"' && break
  sleep 1
done

(cd test/sdk && node lifecycle.mjs)
python3 test/sdk/lifecycle.py

if grep -q 'DATA RACE' "$work/api.log"; then
  cat "$work/api.log"
  exit 1
fi
echo "fake-e2e: ok"
