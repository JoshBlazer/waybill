# Stage 5: breadth and operations

**Status:** approved by the owner 2026-10-18 (ADR-045).
**Owner decisions (2026-10-18):**
- The design as written.
- (a) Add `btcsuite/btcd/btcutil`.
- (b) Regtest `bitcoind` in an opt-in profile, and signet through the public Esplora API.
- (c) OTLP over HTTP, with an opt-in `grafana/otel-lgtm` profile.
**Covers:** the four stage 5 roadmap items and the exit check: "the same e2e path passes on Ethereum Sepolia and Bitcoin regtest, and failover is proven by a test that kills the primary RPC".

## 1. Ethereum Sepolia (`evm:11155111`)

The code already treats networks as data, so this is mostly operations.

- **Deploy:** run `script/Deploy.s.sol` to chain 11155111, including `BatchPayout`. The addresses go in `contracts/deployments/11155111.json` and the transactions in `broadcast/`, as for Base Sepolia.
  - *Owner:* the deploy runs in your terminal with a funded throwaway key, which you type into a hidden prompt (`read -rs`), as with the Paystack key. I cannot enter keys or use faucets.
- **Finality:** the `finalized` tag, which is about 13 minutes on Sepolia. The watcher and the batch sender already support it.
- **Public RPCs:** two keyless ones, so failover (section 3) has somewhere to go.
- **The "same e2e path":** `make e2e-live NETWORK=evm:11155111` runs the thin-slice flow against Sepolia:
  - create an invoice;
  - pay it with mUSDC from the throwaway key;
  - see it confirmed, final and swept;
  - reconcile with 0 differences.
  - It takes about 15 minutes and needs the key, so it runs by hand and the result goes in the README, like Base Sepolia's. It does not run in CI.

## 2. Bitcoin receive (`btc:regtest`, `btc:signet`)

**Receive only.** Waybill holds only an extended *public* key (I15). It derives a fresh address per invoice and watches for payments. It can never spend, so there is no sweep and no batch pay in BTC.

- **Invoices:** an invoice can be priced in BTC, with `asset: "BTC"` and payout `hold` only. Converting BTC to naira is out of scope for this stage.
- **Addresses:** native segwit (P2WPKH, BIP84). The address is `xpub/0/i`, where `i` is a per-network counter in the database, so an address is never reused.
- **Config:**
  - `WAYBILL_BTC_XPUB_<NETWORK>` holds a `tpub` or `vpub`.
  - Startup refuses any private key (`tprv`/`vprv`/WIF) with `TestBTCConfig_RejectsPrivateKey` (I15).
  - Startup also refuses an xpub whose version bytes are for mainnet.
- **Watcher:** follows blocks through a small `BTCChain` interface:
  - `getblockcount`, `getblockhash`, `getblock` (verbosity 2);
  - every output paying one of our addresses becomes a payment (`txid`, `vout`);
  - reorgs use the same block-hash method as the EVM watcher: walk back, reverse, rescan;
  - confirmed at 1 block, final at 6 (regtest: a configured depth).
- **Ledger:**
  - `custody:xpub:{net}:BTC` is the asset: the coins sit at addresses whose key is held offline;
  - `liability:contractor:{id}:BTC:pending/available` is what is owed.
  - These are new account codes only; there is no ledger schema change.
- **Reconciliation:** R3 for Bitcoin compares `custody:xpub` with the sum of unspent outputs at our addresses, read with `scantxoutset`.
- **Web:** the payment page shows the address and a `bitcoin:` URI with the amount, and the tracking page is unchanged.

**Schema** (not the ledger tables). New tables, not loosened EVM constraints, so the strict `0x…` checks on the EVM tables stay as they are:

```sql
CREATE TABLE btc_addresses (network text, address text PRIMARY KEY, invoice_id uuid UNIQUE REFERENCES invoices (id),
                            derivation_index bigint, UNIQUE (network, derivation_index));
CREATE TABLE btc_payments  (id uuid PRIMARY KEY, network text, txid text CHECK (txid ~ '^[0-9a-f]{64}$'), vout int,
                            block_height bigint, block_hash text, address text REFERENCES btc_addresses, invoice_id uuid,
                            amount minor_units, state text CHECK (state IN ('detected','confirmed','final','reorged')),
                            UNIQUE (network, txid, vout));
CREATE TABLE btc_cursors   (network text PRIMARY KEY, next_height bigint, last_hash text);
```

**API (OpenAPI 0.8.0)**
- `createInvoice` accepts `asset: "BTC"`.
- `DepositAddress.token` becomes optional (Bitcoin has none), and a `uri` field is added.
- These changes are backward compatible.

**Owner choices:**
- **(a) Library.** Derivation needs BIP32, bech32 and RIPEMD-160.
  - `github.com/btcsuite/btcd/btcutil` (`hdkeychain` and `address`) is the standard Go implementation. `btcec`, from the same project, is already in the module graph through go-ethereum, so this adds one new direct dependency.
  - The alternative is about 300 hand-written lines tested against the BIP32 and BIP84 test vectors.
- **(b) Nodes.**
  - **Regtest:** a pinned `bitcoind` image in an opt-in Compose profile (`--profile bitcoin`). Its descriptor wallet stands in for the offline key holder: Waybill reads only the wallet's xpub, and the e2e test pays from that node.
  - **Signet:** either a pruned signet `bitcoind` in the same profile (a few GB and a slow first sync), or the public mempool.space Esplora REST API (no node, but a third-party service). A second backend behind the same interface also exercises failover.

## 3. RPC failover and node health

- `WAYBILL_CHAINS` takes several URLs per network, separated by `|`: `evm:11155111=https://a|https://b`. The first is the primary.
- An `rpcpool.Client` implements the interfaces the watcher, txmgr, batch sender and reconciler already use.
  - Every call goes to the current endpoint.
  - On a transport error, a timeout or a 5xx, it marks the endpoint down and retries the call once on the next healthy one.
  - JSON-RPC errors such as a revert are answers, not failures, and are never retried.
- **Health loop:** every 15 seconds, per endpoint, it records the head height, the latency and the errors.
  - An endpoint more than N blocks behind the best head is *lagging*, and is used only when nothing better is up.
  - The primary is used again once it has been healthy for a minute.
- **Safety:** each endpoint must pass the startup chain-ID check on its own. A failover endpoint that reports another chain is refused for good, not just skipped.
- **Health in the open:** shown in `/v1/health` (per network: endpoints up, current one, head lag) and as metrics.
- **Exit-check test:** two endpoints for one Anvil chain, the second through a TCP proxy. The test kills the primary in the middle of a payment, and the payment is still detected, confirmed and made final, with no duplicate. A second test makes the primary lag behind and shows it is demoted.

## 4. Metrics and traces (OpenTelemetry)

- **Traces:** spans for every HTTP request, every outbox job, every watcher tick, each transaction sent, and each RPC call, tagged with the endpoint's host but never its full URL, since that can contain a key.
- **Metrics:**
  - request rate, errors and latency;
  - watcher lag in blocks;
  - outbox depth and dead jobs;
  - transactions pending;
  - RPC endpoint health;
  - reconciliation differences, the most important one.
- **Off by default:** OpenTelemetry stays off unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set, so a plain `make up` is unchanged.

**Owner choice (c): exporter.**
- **OTLP over HTTP**, with an opt-in Compose profile (`--profile observability`) running one pinned `grafana/otel-lgtm` container. It holds a collector, Tempo, Prometheus and Grafana, and ships a dashboard JSON.
  - Dependencies: `go.opentelemetry.io/otel` with its SDK, the OTLP-HTTP exporters and `otelhttp`.
- **Or** OTLP only, with no container, documented for any collector.
- **Or** a Prometheus `/metrics` endpoint with no traces.

## 5. Order of work

1. Failover. It is pure code, and the other items use it.
2. Metrics and traces.
3. Bitcoin regtest, then signet.
4. Ethereum Sepolia: the deploy and the live run need you.
5. The stage walkthrough.

Each item is its own PR, with its README row, roadmap tick, RISKS entries and an ADR for each stack change (ADR-045 onwards).
