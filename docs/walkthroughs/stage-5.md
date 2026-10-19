# Stage 5 walkthrough — Breadth and operations

Stage 4 ran on one EVM test network with one RPC endpoint. Stage 5 adds a second EVM network (Ethereum Sepolia) and a different kind of chain (Bitcoin, receive only). It also keeps the services running when an RPC endpoint dies, and shows what they are doing through metrics and traces.

## What was built

| Piece | Where | Why |
|---|---|---|
| RPC failover | `internal/rpcpool`, `WAYBILL_CHAINS=net=url\|url` | Several endpoints per network. A transport error, a timeout or a 5xx is retried once on the next healthy endpoint. A JSON-RPC answer (a revert, "not found") is an answer and is never retried elsewhere. An endpoint more than 10 blocks behind the best head is demoted. The primary is used again after a healthy minute. |
| Every endpoint checked | `internal/safety` | The startup guard checks the chain id (EVM) or genesis hash (Bitcoin) of every endpoint, not just the first. An endpoint on the wrong chain refuses startup, so failover can never move money onto another network. |
| Health in the open | `/v1/health`, `rpc_endpoint_state` | Per network: each endpoint by host (never the full URL, which can hold a key), its state and the one in use. |
| Metrics and traces | `internal/telemetry`, `make up-observability` | OpenTelemetry over OTLP/HTTP, off unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set. Spans for requests, outbox jobs, watcher ticks, RPC calls and reconciliation. Metrics for watcher lag, outbox depth and dead jobs, pending transactions, endpoint state and failovers, and reconciliation differences. The profile adds one `grafana/otel-lgtm` container with a Waybill dashboard. |
| Bitcoin addresses | `internal/btc`, migration 00015 | A fresh BIP84 (P2WPKH) address per invoice, derived as `xpub/0/i` from an extended *public* key, with `i` from a per-network counter so no address is reused. A private key or a mainnet key refuses startup. |
| Bitcoin watcher | `internal/btcwatch` | Follows blocks through bitcoind JSON-RPC or an Esplora API. Every output paying one of our addresses becomes a payment `(txid, vout)`. Confirmed at 1 block, final at 3 (regtest) or 6 (signet). Reorgs are handled like the EVM watcher: walk back by block hash, reverse, rescan. |
| Bitcoin in the ledger and reconciliation | `custody:xpub:{net}:BTC`, check `R3:btc:*` | Bitcoin custody is an asset held at addresses whose key is offline. Reconciliation compares it with the unspent value at our addresses (`scantxoutset`, or Esplora UTXOs). |
| Bitcoin on the stack | `make up-bitcoin`, `deploy/bitcoin/setup.sh` | A pinned regtest `bitcoind`. Its descriptor wallet stands in for the offline key holder: setup writes only the wallet's xpub for Waybill to read. CI runs the whole stack with it. |
| Web | `/invoices/new`, `/pay/{code}` | "Paid in" offers USDC or BTC. A BTC payment page shows the address and a BIP21 `bitcoin:` link with the amount. API 0.8.0: `DepositAddress.token` is optional and `uri` is new. |
| Ethereum Sepolia | `scripts/live-evm.sh`, `make live-evm` | One script: deploy if needed, create an invoice, pay it, wait for finality and the sweep, reconcile. It asks for a throwaway key in a hidden prompt and runs the live services against the public network beside the local stack. |

## How a BTC invoice is paid

1. **Create.** The contractor asks for 0.01 BTC, held. The invoice service takes the next index for `btc:regtest` in the same transaction as the invoice and derives `xpub/0/i`. The address and its index are stored with a unique constraint on both.
2. **Pay.** The payer scans the `bitcoin:` link. On regtest the e2e test pays from the node's wallet and mines a block.
3. **Detect and confirm.** The watcher reads the block, finds the output, and records the payment as detected. At one block it is confirmed and booked: custody up, the contractor's pending liability up.
4. **Final.** At three blocks the payment is final and moves from pending to available. The invoice settles.
5. **Reorg.** If the block is replaced, the watcher walks back by hash to the fork point, reverses what it booked above it, and rescans. A payment that comes back in the new chain is booked again; one that does not is `reorged`.
6. **Prove.** Reconciliation reads the unspent value at our addresses at one height and compares it with `custody:xpub`. Payments seen but not yet booked are listed as in flight, so they are not called a difference.

## Exit check

The roadmap asks that "the same e2e path passes on Ethereum Sepolia and Bitcoin regtest, and failover is proven by a test that kills the primary RPC".

| Requirement | Test | Result |
|---|---|---|
| Failover with the primary killed mid-payment | `TestWatcher_SurvivesPrimaryRPCDying` (Anvil; the primary sits behind a TCP proxy that the test closes after the payment) | pass: detected, confirmed and final once, on the second endpoint |
| A lagging primary is demoted | `go test ./internal/rpcpool` | pass |
| The e2e path on Bitcoin regtest | `make e2e` on `make up-bitcoin` (`bitcoin.spec.ts`), locally and in CI | pass; `make reconcile` after it: 0 differences |
| Bitcoin addresses match a real node | `TestBTCWatch_RegtestEndToEnd` (a real `bitcoind`) | pass |
| Signet read path | Esplora at `blockstream.info/signet/api` | pass |
| The e2e path on Ethereum Sepolia, rehearsed | `scripts/live-evm.sh` against an Anvil fork of Sepolia (chain id 11155111, `finalized` tag 64 blocks behind) | pass: deployed, invoice settled in 178 s, swept, 0 differences |
| The e2e path on the live Ethereum Sepolia network | `make live-evm` with a funded throwaway key | **not run yet** |

**Result (2026-10-19): not yet passed.** Failover and Bitcoin regtest pass. The Sepolia path passes on a fork but has not run on the public network, because it needs a key holding Sepolia test ETH. Getting that ETH means a faucet, and those require an account or a CAPTCHA. The stage closes when `make live-evm` passes on Sepolia itself and its deployment, payment and sweep are recorded under [Testnet deployments](../../README.md#testnet-deployments).

The rehearsal (2026-10-19) ran the unchanged script with `LIVE_RPCS` and `LIVE_HOST_RPC` pointing at `anvil --fork-url https://sepolia.drpc.org`, paid with Anvil's public development account. It deployed the five contracts, started the live API, watcher and worker against the fork, and then:

- 12 s: `received`; 50 s: `confirmed`; 175 s: `settled`, once the fork's `finalized` block passed the payment;
- the sweep was mined into the vault;
- `waybill reconcile` read block 11,946,938: vault 5.00 USDC on chain and in the ledger, deposits 0, 1 on-chain record checked, **0 differences**.

The fork's deployment file and broadcast record were deleted afterwards, and the local stack was reset, so the live run starts clean.

## Found along the way

- **Reconciliation booked deposits at the wrong moment (a stage 4 bug).** Reading the watcher for Bitcoin showed that deposits are booked at confirmation, not first sighting. A deposit only detected is on chain but not in the ledger, and stage 4 missed it. Fixed in #34 with a test that fails on the old code.
- **Telemetry could stop every service starting.** Two bugs, each caught by the new tests before merge: a resource schema URL that conflicted with the SDK default, and a metric callback registered at package init against the no-op provider ("invalid observable: from different implementation").
- **A test key tripped the secret scan.** A Bitcoin test used a real-looking `tprv` to prove private keys are refused. Gitleaks flagged it. Tests now use obviously fake strings, and the work moved to a clean branch rather than rewriting history.
- **mempool.space timed out from the development machine.** Signet reads Blockstream's Esplora instead; both speak the same API.
- **A public Sepolia RPC hung on fork lookups.** Rehearsing the Sepolia script on an Anvil fork of publicnode timed out on fresh accounts; a fork of drpc answered in seconds. It is a reason to configure two endpoints, which the script does.

## Decisions worth defending

- **Retry transport errors, never answers.** A revert on endpoint A is the truth about the chain; asking endpoint B is shopping for a different answer. Only a failure to get an answer moves to the next endpoint.
- **Check every endpoint at startup.** Failover is only safe if every endpoint is the same chain. Checking only the primary would let a misconfigured fallback move money onto another network after the first outage.
- **Bitcoin is receive only, from an xpub.** Waybill never holds a key that can spend. There is no sweep and no payout in BTC; the cost is that coins sit at many addresses until someone with the offline key moves them.
- **New tables for Bitcoin, not loosened EVM ones.** The EVM tables keep their strict `0x…` checks. A Bitcoin txid or address can never be written where an EVM one is expected.
- **Telemetry is off by default.** A plain `make up` is unchanged and sends nothing anywhere.
- **Sepolia by hand, not in CI.** The run needs a funded key and about 15 minutes of real finality. A key in CI secrets is one more key to leak, for a check that changes only when the contracts do.

## Interview questions

1. `rpcpool` retries a transport error on the next endpoint but never a JSON-RPC error. Give a case where retrying a JSON-RPC error would make Waybill do something wrong, and a case where *not* retrying a transport error could.
2. A `sendRawTransaction` times out on the primary. The pool retries it on the fallback, which answers "already known" or "nonce too low". What happened, and why is that safe given how the transaction manager tracks nonces?
3. Why must the guard check the chain id of every endpoint and not just the primary? What would an attacker need to control for the unchecked version to lose money?
4. An endpoint is demoted when it is more than 10 blocks behind the best head. What goes wrong if the threshold is 0? If it is 1,000? Why does the primary need a healthy minute before it is used again?
5. Bitcoin addresses come from a counter in the database, not from the invoice id. What breaks if two API instances derive addresses at the same time, and what in migration 00015 stops it?
6. Walk through a two-block reorg on regtest that removes a confirmed BTC payment and puts it back one block later. What does the ledger hold at each step, and what does the contractor see?
7. Reconciliation for Bitcoin uses `scantxoutset`. Why is the unspent value at our addresses the right thing to compare with `custody:xpub`, and what would a payer spending from one of our addresses (they cannot) or a dust attack do to it?
8. Waybill refuses a `tprv`. What else in the configuration path could leak a spending key, and how would you prove the deployed service has none?
9. Which metric would you page on, and why is "reconciliation differences > 0" better than "watcher errors > 0"?
10. The Sepolia check is a manual script, not a CI job. Argue for moving it into CI, then say what you would have to accept to do it safely.
