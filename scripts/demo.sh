#!/usr/bin/env bash
# End-to-end demo: a wallet signs an unlimited approval to an attacker EOA, the
# attacker drains it, and chainwatch raises the alerts.
#
# Starts postgres + anvil with docker compose, deploys the test token, runs
# chainwatch locally, signs in with Ethereum (SIWE, signed by `cast wallet sign`),
# watches the victim in the signer's organization, plays
# contracts/script/Drain.s.sol and prints what the API reports. Everything is
# torn down on exit unless DEMO_KEEP=1.
set -euo pipefail
cd "$(dirname "$0")/.."

FORGE=${FORGE:-$(command -v forge || echo "$HOME/.foundry/bin/forge")}
CAST=${CAST:-$(command -v cast || echo "$HOME/.foundry/bin/cast")}
COMPOSE="docker compose -f deploy/docker-compose.yml"
export CHAINWATCH_PG_PORT=${CHAINWATCH_PG_PORT:-55432}
export CHAINWATCH_ANVIL_PORT=${CHAINWATCH_ANVIL_PORT:-18545}
HTTP_PORT=${DEMO_HTTP_PORT:-28080}
RPC=http://127.0.0.1:$CHAINWATCH_ANVIL_PORT
API=http://127.0.0.1:$HTTP_PORT
LOG=$(mktemp -t chainwatch-demo.XXXXXX.log)
JAR=$(mktemp -t chainwatch-demo.XXXXXX.cookies)
CHAINS=$(mktemp -t chainwatch-demo.XXXXXX.yaml)
CHAIN_ID=31337

# anvil's built-in dev accounts. These keys are public: never use them elsewhere.
DEPLOYER=0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266 # EIP-55 checksummed, as SIWE requires
DEPLOYER_KEY=0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80
VICTIM=0x70997970C51812dc3A010C7d01b50e0d17dc79C8
VICTIM_KEY=0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d
SCAMMER_KEY=0x5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
api() { curl -fsS -b "$JAR" -c "$JAR" "$@"; }
alert_count() { api "$API/v1/orgs/$ORG/alerts?wallet=$VICTIM" | { grep -o '"rule"' || true; } | wc -l; }
pretty() { if command -v jq >/dev/null; then jq .; else python3 -m json.tool; fi; }

CW_PID=""
cleanup() {
  [ -n "$CW_PID" ] && kill "$CW_PID" 2>/dev/null && wait "$CW_PID" 2>/dev/null || true
  if [ "${DEMO_KEEP:-0}" != 1 ]; then
    $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  rm -f "$JAR" "$CHAINS"
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

step "Starting chainwatch (anvil, 2 confirmations, large outflow threshold 100 CWT)"
cat >"$CHAINS" <<YAML
chains:
  - id: $CHAIN_ID
    name: anvil
    rpc_url: $RPC
    confirmations: 2
    start_block: 0
    poll_interval: 1s
    large_outflow:
      "$TOKEN": "100000000000000000000"
YAML
CHAINWATCH_CHAINS_FILE=$CHAINS \
CHAINWATCH_SIWE_DOMAIN=127.0.0.1:$HTTP_PORT \
CHAINWATCH_COOKIE_SECURE=false \
CHAINWATCH_DATABASE_URL="postgres://chainwatch:chainwatch@127.0.0.1:$CHAINWATCH_PG_PORT/chainwatch?sslmode=disable" \
CHAINWATCH_HTTP_ADDR=127.0.0.1:$HTTP_PORT \
CHAINWATCH_NOTIFY_INTERVAL=1s \
  ./bin/chainwatch >"$LOG" 2>&1 &
CW_PID=$!
for _ in $(seq 1 30); do
  kill -0 "$CW_PID" 2>/dev/null || { echo "chainwatch exited:"; tail -n 20 "$LOG"; exit 1; }
  curl -fs "$API/readyz" >/dev/null && break
  sleep 1
done
curl -fs "$API/readyz" | pretty

step "Signing in with Ethereum as $DEPLOYER (EIP-4361 message signed by cast)"
NONCE=$(api "$API/v1/auth/nonce" | sed -n 's/.*"nonce":"\([0-9a-zA-Z]*\)".*/\1/p')
[ -n "$NONCE" ] || { echo "could not get a nonce"; exit 1; }
MESSAGE="127.0.0.1:$HTTP_PORT wants you to sign in with your Ethereum account:
$DEPLOYER

Sign in to chainwatch.

URI: http://127.0.0.1:$HTTP_PORT
Version: 1
Chain ID: $CHAIN_ID
Nonce: $NONCE
Issued At: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "$MESSAGE" | sed 's/^/    /'
SIGNATURE=$("$CAST" wallet sign --private-key "$DEPLOYER_KEY" "$MESSAGE")
# The message has no quotes or backslashes; only newlines need JSON escaping.
MESSAGE_JSON=$(printf '%s' "$MESSAGE" | awk 'BEGIN{ORS="\\n"} {print}' | sed 's/\\n$//')
api -X POST "$API/v1/auth/verify" -H 'Content-Type: application/json' \
  -d "{\"message\":\"$MESSAGE_JSON\",\"signature\":\"$SIGNATURE\"}" | pretty
ORG=$(api "$API/v1/me" | grep -o '"id":[0-9]*' | head -n 1 | cut -d: -f2)
[ -n "$ORG" ] || { echo "sign-in did not yield an organization"; exit 1; }
echo "organization: $ORG"

step "Watching the victim wallet on chain $CHAIN_ID in organization $ORG"
api -X POST "$API/v1/orgs/$ORG/wallets" -H 'Content-Type: application/json' \
  -d "{\"chain_id\":$CHAIN_ID,\"address\":\"$VICTIM\",\"label\":\"demo victim\"}" | pretty

step "Playing the drain: unlimited approve to an EOA, then transferFrom of the whole balance"
(cd contracts && TOKEN=$TOKEN VICTIM_KEY=$VICTIM_KEY SCAMMER_KEY=$SCAMMER_KEY \
  "$FORGE" script script/Drain.s.sol --rpc-url "$RPC" --broadcast --slow 2>&1 | grep -E 'drained|ONCHAIN|Error' || true)

step "Waiting for confirmations and alerts"
for _ in $(seq 1 60); do
  [ "$(alert_count)" -ge 4 ] && break
  sleep 1
done

step "GET /v1/orgs/$ORG/allowances?chain_id=$CHAIN_ID&wallet=$VICTIM"
api "$API/v1/orgs/$ORG/allowances?chain_id=$CHAIN_ID&wallet=$VICTIM" | pretty

step "GET /v1/orgs/$ORG/alerts?wallet=$VICTIM"
api "$API/v1/orgs/$ORG/alerts?wallet=$VICTIM" | pretty

step "Metrics"
curl -fs "$API/metrics" | grep -E '^chainwatch_(indexed_block|head_lag_blocks|events_total|findings_total|alerts_pending|reorgs_total)' || true

n=$(alert_count)
if [ "$n" -lt 4 ]; then
  echo "expected 4 alerts, got $n"; tail -n 30 "$LOG"; exit 1
fi
step "Demo passed: $n alerts raised for the drained wallet"
