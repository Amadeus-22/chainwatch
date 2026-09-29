#!/usr/bin/env bash
# End-to-end demo: a wallet signs an unlimited approval to an attacker EOA, the
# attacker drains it, and chainwatch raises the alerts.
#
# Starts postgres + anvil with docker compose, deploys the test token, runs
# chainwatch locally, plays contracts/script/Drain.s.sol and prints what the API
# reports. Everything is torn down on exit unless DEMO_KEEP=1.
set -euo pipefail
cd "$(dirname "$0")/.."

FORGE=${FORGE:-$(command -v forge || echo "$HOME/.foundry/bin/forge")}
COMPOSE="docker compose -f deploy/docker-compose.yml"
export CHAINWATCH_PG_PORT=${CHAINWATCH_PG_PORT:-55432}
export CHAINWATCH_ANVIL_PORT=${CHAINWATCH_ANVIL_PORT:-18545}
HTTP_PORT=${DEMO_HTTP_PORT:-28080}
RPC=http://127.0.0.1:$CHAINWATCH_ANVIL_PORT
API=http://127.0.0.1:$HTTP_PORT
LOG=$(mktemp -t chainwatch-demo.XXXXXX.log)

# anvil's built-in dev accounts. These keys are public: never use them elsewhere.
DEPLOYER_KEY=0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80
VICTIM=0x70997970C51812dc3A010C7d01b50e0d17dc79C8
VICTIM_KEY=0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d
SCAMMER_KEY=0x5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
alert_count() { curl -fs "$API/v1/alerts?wallet=$VICTIM" | { grep -o '"rule"' || true; } | wc -l; }
pretty() { if command -v jq >/dev/null; then jq .; else python3 -m json.tool; fi; }

CW_PID=""
cleanup() {
  [ -n "$CW_PID" ] && kill "$CW_PID" 2>/dev/null && wait "$CW_PID" 2>/dev/null || true
  if [ "${DEMO_KEEP:-0}" != 1 ]; then
    $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  echo "chainwatch log: $LOG"
}
trap cleanup EXIT

port_busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

step "Starting fresh postgres and anvil"
$COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
for p in "$CHAINWATCH_PG_PORT" "$CHAINWATCH_ANVIL_PORT" "$HTTP_PORT"; do
  if port_busy "$p"; then
    echo "port $p is already in use; override CHAINWATCH_PG_PORT, CHAINWATCH_ANVIL_PORT or DEMO_HTTP_PORT"
    exit 1
  fi
done
$COMPOSE up -d --wait postgres anvil

step "Building chainwatch"
[ -f contracts/lib/forge-std/src/Test.sol ] || git submodule update --init --recursive
make -s build

step "Deploying the test token and minting 1000 CWT to the victim"
TOKEN=$(cd contracts && DEPLOYER_KEY=$DEPLOYER_KEY VICTIM=$VICTIM \
  "$FORGE" script script/Deploy.s.sol --rpc-url "$RPC" --broadcast --slow 2>&1 |
  sed -n 's/.*TestToken deployed at \(0x[0-9a-fA-F]\{40\}\).*/\1/p')
[ -n "$TOKEN" ] || { echo "token deployment failed"; exit 1; }
echo "token: $TOKEN"

step "Starting chainwatch (2 confirmations, large outflow threshold 100 CWT)"
CHAINWATCH_RPC_URL=$RPC \
CHAINWATCH_DATABASE_URL="postgres://chainwatch:chainwatch@127.0.0.1:$CHAINWATCH_PG_PORT/chainwatch?sslmode=disable" \
CHAINWATCH_HTTP_ADDR=127.0.0.1:$HTTP_PORT \
CHAINWATCH_CONFIRMATIONS=2 \
CHAINWATCH_START_BLOCK=0 \
CHAINWATCH_POLL_INTERVAL=1s \
CHAINWATCH_NOTIFY_INTERVAL=1s \
CHAINWATCH_LARGE_OUTFLOW="$TOKEN=100000000000000000000" \
  ./bin/chainwatch >"$LOG" 2>&1 &
CW_PID=$!
for _ in $(seq 1 30); do
  kill -0 "$CW_PID" 2>/dev/null || { echo "chainwatch exited:"; tail -n 20 "$LOG"; exit 1; }
  curl -fs "$API/readyz" >/dev/null && break
  sleep 1
done
curl -fs "$API/readyz" | pretty

step "Watching the victim wallet"
curl -fs -X POST "$API/v1/wallets" -H 'Content-Type: application/json' \
  -d "{\"address\":\"$VICTIM\",\"label\":\"demo victim\"}" | pretty

step "Playing the drain: unlimited approve to an EOA, then transferFrom of the whole balance"
(cd contracts && TOKEN=$TOKEN VICTIM_KEY=$VICTIM_KEY SCAMMER_KEY=$SCAMMER_KEY \
  "$FORGE" script script/Drain.s.sol --rpc-url "$RPC" --broadcast --slow 2>&1 | grep -E 'drained|ONCHAIN|Error' || true)

step "Waiting for confirmations and alerts"
for _ in $(seq 1 60); do
  [ "$(alert_count)" -ge 4 ] && break
  sleep 1
done

step "GET /v1/wallets/$VICTIM/allowances"
curl -fs "$API/v1/wallets/$VICTIM/allowances" | pretty

step "GET /v1/alerts?wallet=$VICTIM"
curl -fs "$API/v1/alerts?wallet=$VICTIM" | pretty

step "Metrics"
curl -fs "$API/metrics" | grep -E '^chainwatch_(indexed_block|head_lag_blocks|events_total|findings_total|alerts_pending|reorgs_total)' || true

n=$(alert_count)
if [ "$n" -lt 4 ]; then
  echo "expected 4 alerts, got $n"; tail -n 30 "$LOG"; exit 1
fi
step "Demo passed: $n alerts raised for the drained wallet"
