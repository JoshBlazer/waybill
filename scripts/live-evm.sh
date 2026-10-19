#!/usr/bin/env bash
# Live check on a public EVM test network (docs/design/stage5.md §1).
#
#   make up                    # the local stack, seeded (once)
#   scripts/live-evm.sh        # Ethereum Sepolia (default)
#   scripts/live-evm.sh 84532  # Base Sepolia
#
# A rehearsal against a local fork of the network runs the same steps with no
# real test ETH (it writes a deployment file for the fork: delete it after):
#
#   LIVE_RPCS=http://sepolia-fork:8545 LIVE_HOST_RPC=http://127.0.0.1:58546 scripts/live-evm.sh
#
# Runs the thin-slice path against the real network: create an invoice, pay
# it with mUSDC, wait until it is final and swept into the vault, then
# reconcile. It deploys the contracts first if the network has no
# contracts/deployments/<chainId>.json yet.
#
# It asks for a THROWAWAY test key holding test ETH only. It is never
# echoed and never written to disk. The worker container gets it from the
# environment (WAYBILL_SIGNER_KEY). forge and cast take it as --private-key
# (this Foundry version reads no key variable), so it is visible in this
# machine's process list while they run: use a key with test ETH only.
#
# While it runs, the stack's own api, watcher and worker containers are
# stopped and live ones take their place against the same database; they
# are put back at the end, even on failure.
set -euo pipefail
cd "$(dirname "$0")/.."

CHAIN_ID=${1:-11155111}
case "$CHAIN_ID" in
  11155111)
    NAME="Ethereum Sepolia"
    RPCS="https://ethereum-sepolia-rpc.publicnode.com|https://sepolia.drpc.org"
    EXPLORER="https://sepolia.etherscan.io" ;;
  84532)
    NAME="Base Sepolia"
    RPCS="https://sepolia.base.org|https://base-sepolia-rpc.publicnode.com"
    EXPLORER="https://sepolia.basescan.org" ;;
  *) echo "live-evm: only 11155111 (Ethereum Sepolia) and 84532 (Base Sepolia) are allowed" >&2; exit 2 ;;
esac
# Rehearsal on a local fork (anvil --fork-url): LIVE_RPCS replaces the
# endpoints the services use, LIVE_HOST_RPC the one cast and forge use here.
RPCS=${LIVE_RPCS:-$RPCS}
[ -z "${LIVE_RPCS:-}" ] || EXPLORER="(fork)"
NETWORK="evm:$CHAIN_ID"
PRIMARY=${LIVE_HOST_RPC:-${RPCS%%|*}}
DB=postgres://waybill:waybill-local-only@postgres:5432/waybill?sslmode=disable
CURL_IMAGE=curlimages/curl:8.10.1
say() { printf '\n== %s\n' "$*"; }
psql() { docker exec waybill-postgres-1 psql -U waybill -d waybill -tAc "$1"; }

say "Checking the local stack"
docker inspect waybill-postgres-1 >/dev/null 2>&1 || { echo "Start the stack first: make up" >&2; exit 1; }
[ -f deploy/.env ] || { echo "deploy/.env is missing: run make up" >&2; exit 1; }
set -a; . deploy/.env; set +a
open_jobs=$(psql "SELECT count(*) FROM outbox WHERE done_at IS NULL AND dead_at IS NULL")
if [ "$open_jobs" != "0" ]; then
  echo "The outbox has $open_jobs unfinished jobs; let the stack finish them first (the live worker watches $NAME only)." >&2
  exit 1
fi
chain_id=$(cast chain-id --rpc-url "$PRIMARY")
[ "$chain_id" = "$CHAIN_ID" ] || { echo "$PRIMARY reports chain $chain_id, not $CHAIN_ID" >&2; exit 1; }

say "Your throwaway $NAME key"
read -rsp "Private key (hex; test ETH only; not shown): " KEY; echo
WAYBILL_SIGNER_KEY=$KEY
export WAYBILL_SIGNER_KEY
ADDR=$(cast wallet address --private-key "$KEY")
BAL=$(cast balance "$ADDR" --rpc-url "$PRIMARY" --ether)
echo "Address $ADDR holds $BAL ETH on $NAME."
awk -v b="$BAL" 'BEGIN { exit !(b >= 0.02) }' || { echo "Fund it with at least 0.02 test ETH first." >&2; exit 1; }

if [ ! -f "contracts/deployments/$CHAIN_ID.json" ]; then
  say "Deploying the contracts to $NAME"
  # Without WAYBILL_SIGNER_KEY: the script would otherwise send the
  # service signer 100 ETH, which only Anvil has.
  (cd contracts && env -u WAYBILL_SIGNER_KEY forge script script/Deploy.s.sol --rpc-url "$PRIMARY" --broadcast --slow --private-key "$KEY")
fi
DEPLOYMENT="contracts/deployments/$CHAIN_ID.json"
TOKEN=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['token'])" "$DEPLOYMENT")
echo "Deployment: $(tr -d '\n' < "$DEPLOYMENT")"

restore() {
  say "Putting the local stack back"
  docker rm -f waybill-live-api waybill-live-watcher waybill-live-worker >/dev/null 2>&1 || true
  docker start waybill-api-1 waybill-watcher-1 waybill-worker-1 >/dev/null 2>&1 || true
  unset KEY WAYBILL_SIGNER_KEY
}
trap restore EXIT

say "Starting live services for $NAME"
docker stop waybill-api-1 waybill-watcher-1 waybill-worker-1 >/dev/null
common=(--network waybill_default -v "$PWD/contracts/deployments:/deployments:ro"
  -e DATABASE_URL="$DB" -e WAYBILL_CHAINS="$NETWORK=$RPCS" -e WAYBILL_DEPLOYMENTS_DIR=/deployments
  -e WAYBILL_RATES=fake -e WAYBILL_DEV_CONTRACTOR_KEY -e WAYBILL_WEB_URL=http://localhost:3000)
docker run -d --name waybill-live-api "${common[@]}" -e HTTP_ADDR=:8080 waybill-api:local api >/dev/null
docker run -d --name waybill-live-watcher "${common[@]}" waybill-api:local watcher >/dev/null
docker run -d --name waybill-live-worker "${common[@]}" -e WAYBILL_SIGNER_KEY waybill-api:local worker >/dev/null
api() { docker run --rm --network waybill_default "$CURL_IMAGE" -sS -H "Authorization: Bearer $WAYBILL_DEV_CONTRACTOR_KEY" "$@"; }
for _ in $(seq 1 30); do api -o /dev/null -f http://waybill-live-api:8080/v1/health 2>/dev/null && break; sleep 2; done
api -f http://waybill-live-api:8080/v1/health; echo

say "Creating a 5.00 USDC invoice"
inv=$(api -f -X POST http://waybill-live-api:8080/v1/invoices -H "Content-Type: application/json" \
  -H "Idempotency-Key: live-$CHAIN_ID-$(date +%s)" \
  -d '{"description":"Live check on '"$NAME"'","amount":"5","asset":"USDC","payout":"hold"}')
read -r ID DEPOSIT < <(python3 -c "
import json,sys
i=json.loads(sys.argv[1]); a=[d for d in i['depositAddresses'] if d['network']==sys.argv[2]][0]
print(i['id'], a['address'])" "$inv" "$NETWORK")
start=$(date +%s)
echo "Invoice $ID, deposit address $DEPOSIT"

say "Paying 5 mUSDC to the deposit address"
tx=$(cast send "$TOKEN" "mint(address,uint256)" "$DEPOSIT" 5000000 --rpc-url "$PRIMARY" --private-key "$KEY" --json | python3 -c "import json,sys; print(json.load(sys.stdin)['transactionHash'])")
echo "Payment $EXPLORER/tx/$tx"

say "Waiting for settlement (finality on $NAME takes about 15 minutes)"
last=""
for _ in $(seq 1 120); do
  state=$(api -f "http://waybill-live-api:8080/v1/invoices/$ID" | python3 -c "import json,sys; print(json.load(sys.stdin)['state'])")
  if [ "$state" != "$last" ]; then echo "$(( $(date +%s) - start ))s: $state"; last=$state; fi
  [ "$state" = "settled" ] && break
  sleep 30
done
[ "$state" = "settled" ] || { echo "Not settled after an hour." >&2; exit 1; }

say "Waiting for the sweep into the vault"
for _ in $(seq 1 60); do
  sweep=$(psql "SELECT coalesce(mined_hash,'') FROM outbound_txs WHERE purpose='sweep' AND network='$NETWORK' AND state='mined' ORDER BY updated_at DESC LIMIT 1")
  [ -n "$sweep" ] && break
  sleep 15
done
[ -n "$sweep" ] && echo "Sweep $EXPLORER/tx/$sweep" || echo "No sweep mined yet (the worker sweeps every 15 s once final)."

say "Reconciling"
docker exec waybill-live-worker waybill reconcile --settle --wait 60s

say "Done: invoice $ID settled $(( $(date +%s) - start ))s after creation"
