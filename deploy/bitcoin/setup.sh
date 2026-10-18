#!/bin/sh
# Regtest setup for the opt-in bitcoin profile (ADR-045). Runs once, after
# bitcoind is up:
#   - "custody" is a descriptor wallet standing in for the offline key holder.
#     Waybill is given only its BIP84 receive xpub, written to /btc/xpub.
#   - "payer" holds mined coins, for tests and demos to pay invoices with.
# Safe to run again: wallets are loaded if they exist, and coins are mined
# only when the chain is short.
set -eu

cli() { bitcoin-cli -regtest -rpcconnect=bitcoind -rpcuser=waybill -rpcpassword=waybill-local-only "$@"; }

until cli getblockcount >/dev/null 2>&1; do sleep 1; done

for w in custody payer; do
  cli loadwallet "$w" >/dev/null 2>&1 || cli createwallet "$w" >/dev/null 2>&1 || true
done

if [ "$(cli getblockcount)" -lt 101 ]; then
  cli generatetoaddress 101 "$(cli -rpcwallet=payer getnewaddress)" >/dev/null
fi

# wpkh([fingerprint/84h/1h/0h]tpub.../0/*)#checksum -> tpub...
xpub=$(cli -rpcwallet=custody listdescriptors \
  | grep -o '"desc": "wpkh(\[[^]]*\]tpub[^/]*/0/\*)' \
  | sed 's/.*\]//; s#/0/\*)##' | head -1)
case "$xpub" in
  tpub*) ;;
  *) echo "setup: no wpkh receive descriptor in the custody wallet" >&2; exit 1 ;;
esac
umask 022
printf '%s\n' "$xpub" > /btc/xpub
echo "setup: regtest ready at height $(cli getblockcount); custody xpub written to /btc/xpub"
