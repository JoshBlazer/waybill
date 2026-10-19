# Waybill

**Pay Nigerian contractors in stablecoins. They receive naira in their bank. Every payment is trackable like a parcel.**

A contractor sends a payment link. The payer pays in USDC (or by card or bank transfer) after seeing the contractor's verified name. Both follow the payment on a public tracking page with four plain steps: **received → confirmed → converted → sent to bank**.

> **Test money only.** Waybill runs on test networks (Anvil, Base Sepolia, Ethereum Sepolia, Bitcoin regtest and signet) and in Paystack test mode. The services refuse to start against anything else; see [the test-money guard](docs/ARCHITECTURE.md#10-test-money-guard).

## Status

Updated in the same commit as the work it describes. "Done" means a command or test in the repository proves it.

| Area | Status | Proof |
|---|---|---|
| **Stage 0 — Foundations** (exit check passed 2026-10-09) | Done | [walkthrough](docs/walkthroughs/stage-0.md) |
| Product, architecture, design, roadmap, decisions, risks documents | Done | [docs/](docs/) |
| Test-money guard: chain allowlist, live chain-ID check, Paystack test-key check | Done | `make test-api` → `api/internal/safety` |
| OpenAPI contract (`GET /v1/health`), generated Go and TypeScript types, drift check | Done | `make check-gen` |
| Database migrations (goose; schema empty) against real PostgreSQL | Done | `make test-api` → `TestMigrate_AgainstRealPostgres` |
| **Stage 1 — Thin slice** (exit check passed 2026-10-16) | Done | [walkthrough](docs/walkthroughs/stage-1.md) |
| Contracts: mock stablecoin, CREATE2 deposit forwarders and factory, vault with limits and pause | Done (stage 1) | `make test-contracts` (25 tests, fuzzed) |
| Local stack: PostgreSQL, Anvil, API, watcher, worker, web | Done | `make up` |
| CI on every push | Done | [Actions](../../actions) |
| `money` package: exact integer amounts, no floats (AST-checked), property-tested | Done (stage 1) | `go test ./internal/money` |
| Invoice state machine: pure, all 72 (state, event) pairs tested | Done (stage 1) | `go test ./internal/statemachine` |
| Ledger: double-entry, append-only, overdraft-proof schema enforced by PostgreSQL, and its posting engine | Done (stage 1) | `go test ./internal/ledger` (14 schema tests, 18 engine tests incl. property and concurrency tests) |
| Thin slice: invoice → pay on Anvil → watcher → ledger → live tracking page | Done on Anvil (stage 1) | `make up && make e2e` (3 tests: live tracking, settlement to final, browser wallet) |
| Accessibility (axe, WCAG 2.1 AA) on every thin-slice page and state | Done (stage 1) | `make e2e` → `e2e/accessibility.spec.ts` |
| Performance budgets (Lighthouse CI: LCP, TBT, CLS, transfer) | Done (stage 1); budgets set from CI measurement (ADR-036) | `make lighthouse` |
| Base Sepolia: contracts deployed, one invoice paid and settled | Done (stage 1) | [Testnet deployments](#testnet-deployments) |
| **Stage 2 — Hardening** (exit check passed 2026-10-16) | Done | [walkthrough](docs/walkthroughs/stage-2.md) |
| **Stage 3 — Naira settlement** | In progress: every item done; the live exit check waits on the Paystack account ("cannot initiate third party payouts as a starter business") | [roadmap](docs/ROADMAP.md#stage-3--naira-settlement-first-public-milestone), [walkthrough](docs/walkthroughs/stage-3.md) |
| Payout state machine: pure, all 130 (state, event) pairs tested; a provider timeout is `unknown`, never `failed` | Done (stage 3) | `go test ./internal/statemachine` ([design](docs/design/payout.md)) |
| Rates (exact kobo-per-USDC quotes with spread and expiry), bank-name verification, Paystack test-mode client (resolve, recipients, transfers, verify by reference, webhook signatures) | Done (stage 3): ExchangeRate-API (ADR-039), Paystack account resolution (ADR-040) | `go test ./internal/rates ./internal/bankverify ./internal/paystack` |
| Verified bank accounts (24 h wait after a change), naira invoices with a locked rate, invoice lookup, accepting a shortfall, Paystack callbacks stored once | Done (stage 3); API 0.4.0 (ADR-041) | `go test ./internal/httpapi` |
| Card and bank pay-in: Paystack test checkout at the locked naira amount, confirmed only by verifying the reference, settled in one transaction; the payment page offers it only when the link can be paid in naira | Done (stage 3); API 0.5.0 (ADR-042) | `go test ./internal/checkout ./internal/httpapi`; `make e2e` (`naira.spec.ts`) |
| Naira payouts: one per settled naira invoice; converted at the locked rate (re-quoted for late payments), sent through Paystack with the payout id as reference, timeouts reconciled by reference and never resent blind, transfer callbacks checked, tracking steps converted and sent to bank | Done (stage 3); in Paystack test mode, account resolution, recipients and checkout pass, and the transfer is refused until the Paystack business is upgraded past "starter" | `go test ./internal/payout` (incl. `TestExitCheck_LinkPaidAndNairaPayoutCompletes` on Anvil) |
| Web: bank account page (bank list, verification, 24 h wait shown in Lagos time) and the naira choice with its locked rate on the new-invoice form | Done (stage 3) | `make e2e` (`e2e/naira.spec.ts`, axe on every state) |
| **Stage 4 — Teams** (exit check passed 2026-10-18) | Done | [walkthrough](docs/walkthroughs/stage-4.md) |
| **Stage 5 — Breadth and operations** | In progress | [design](docs/design/stage5.md), [roadmap](docs/ROADMAP.md#stage-5--breadth-and-operations) |
| RPC failover: several endpoints per network, a transport failure retried once on the next healthy one (a node's answer never retried), lagging endpoints demoted, the primary back after a healthy minute, every endpoint checked against the chain id at startup, health in `/v1/health` | Done (stage 5); API 0.8.0 (ADR-045) | `go test ./internal/rpcpool`; `go test ./internal/watcher` (`TestWatcher_SurvivesPrimaryRPCDying`: Anvil, primary killed mid-payment); `go test ./internal/safety` |
| Batch-run state machine: pure, all 100 (state, event) pairs tested; money leaves only after an approval, and a sent run cannot be cancelled | Done (stage 4) | `go test ./internal/statemachine` ([design](docs/design/teams.md)) |
| Payer organisations: members with admin, maker and approver roles (the last admin cannot be removed), member API keys kept apart from contractor keys, approval threshold, funding addresses credited pending then available and swept into the vault | Done (stage 4); API 0.6.0 (ADR-043) | `go test ./internal/httpapi` (`org_test.go`), `go test ./internal/sweep` (`TestFunding_OrgDepositIsCreditedThenSwept` on Anvil) |
| Contractor payout wallets (one per network, 24 h wait after a change) and batch runs: every line checked (verified contractor, wallet past its wait, vault limit, no repeat payment in 30 days, funds), approval by someone other than the creator above the threshold (refused in SQL too), funds reserved before sending, one `BatchPayout` transaction, final at finality, a reverted run's money returned, per-line tracking pages | Done (stage 4); API 0.6.0 (ADR-043) | `go test ./internal/batch` (incl. `TestExitCheck_BatchWithDualApprovalCompletes` on Anvil), `go test ./internal/httpapi` (`batches_test.go`) |
| Web pages for teams: the organisation page (funds, funding address, runs, members), building a run, a run page offering each member only what their role allows, a demo switch between the two seeded members, and the contractor's payout-wallet page | Done (stage 4) | `make e2e` (`batches.spec.ts`: fund, build a 550 USDC run, approve as the second member, send, paid, track the line; axe on every page) |
| Reorg handling: detect by block hash, walk back to the common ancestor, reverse confirmed payments, rescan | Done (stage 2) | `go test ./internal/watcher` (fake-chain reorg tests; `TestWatcher_ReorgReversesConfirmedPayment` forces a reorg on Anvil with `evm_snapshot`/`evm_revert`) |
| Payment edge cases: payments in parts, top-ups, overpayment, paying twice, payment after settlement, invoice expiry and late payment | Done (stage 2) | `go test ./internal/watcher` (`edge_test.go`) |
| Transaction manager nonce and fee logic: single-writer lease per key, gap-free nonces under concurrency, EIP-1559 initial and replacement fees | Done (stage 2) | `go test ./internal/txmgr` (13 tests incl. `rapid` properties and a concurrency test; [design](docs/design/txmgr.md)) |
| `BatchPayout` contract: all-or-nothing batches through the vault's payout-id guard and limits | Done (stage 2); deployed with the payout plumbing | `make test-contracts` (10 unit tests, 4 invariants) |
| Outbox and worker: jobs enqueued in the same transaction as the change, leased with `SKIP LOCKED`, retried with backoff, dead after the last attempt; invoice expiry runs in the worker | Done (stage 2) | `go test ./internal/outbox` (12 tests incl. 8 concurrent workers) |
| API key management (create, list, revoke) and signed outgoing webhooks: invoice events delivered by the worker, retried for about a day, every attempt logged, SSRF guard on endpoint URLs | Done (stage 2) | `go test ./internal/webhook ./internal/httpapi` |
| Transaction manager plumbing: submit, broadcast (hash recorded first), receipts, replacing stuck transactions with both fees bumped, gas-estimation failures recorded and never broadcast | Done (stage 2) | `go test ./internal/txmgr` (Anvil in testcontainers) |
| Sweeping final deposits into the vault through the transaction manager, recorded in the ledger from the on-chain `Swept` event; `BatchPayout` in the deploy script | Done (stage 2) | `go test ./internal/sweep` (real contracts on Anvil) |
| Reconciliation: the ledger checked against the chain (vault and deposit balances at one block height, with money in flight listed by record; every final payment and paid run against its receipt) and against Paystack (every settled payout and paid card checkout); a balance off once is unsettled, and off twice is a difference; reports stored, worker every 15 minutes, `waybill reconcile` exits 1 on a difference | Done (stage 4); ADR-044 | `go test ./internal/batch` (`TestExitCheck_ReconciliationAfterDualApprovalRunIsClean` and four negative and in-flight tests on Anvil), `go test ./internal/payout` (provider agreement, disagreement, skipped without a key); `make reconcile` after `make e2e` in CI |
| Invoices, receipts and the accountant export: invoice and receipt PDFs (a run's receipt names its creator, approver and sender) from a standard-library PDF writer, JSON receipts, and a CSV of the caller's own available balances with money in, money out, balance and chain or provider reference, guarded against spreadsheet formulas; each figure read from the ledger | Done (stage 4); API 0.7.0 (ADR-044) | `go test ./internal/pdf ./internal/documents ./internal/httpapi` (`documents_test.go`), `go test ./internal/payout ./internal/batch` (receipts and exports against the ledger; the run on Anvil); `make e2e` (`batches.spec.ts` downloads the run receipt and the CSV) |
| Contractor balance and conversion: a two-minute quote on part of the USDC balance, accepted only before it expires (never re-quoted), booked with the same four legs as an invoice payout's conversion, and sent to the bank through the existing payout pipeline; concurrent accepts cannot overdraw | Done (stage 4); API 0.7.0 (ADR-044) | `go test ./internal/conversion` (incl. expiry, concurrent accepts, the payout sent and reconciled), `go test ./internal/httpapi` (`conversions_test.go`); `make e2e` (`conversion.spec.ts`) |
| Metrics and traces: OpenTelemetry over OTLP/HTTP, off unless an endpoint is set; spans for API requests, outbox jobs, watcher ticks, RPC calls and reconciliation; metrics for request rate and latency, watcher blocks behind, outbox depth and dead jobs, pending transactions, RPC endpoint state and failovers, and reconciliation differences; an opt-in `grafana/otel-lgtm` profile with a Waybill dashboard | Done (stage 5); ADR-045 | `go test ./internal/telemetry` (exports to an OTLP receiver); `make up-observability`, then the metrics and traces were queried in Prometheus and Tempo |
| Bitcoin receive: BTC invoices (held in BTC) paid to a fresh BIP84 address derived from an extended public key (any private key refuses startup); a watcher that books, settles and reverses on reorg like the EVM one; the guard checks the node's genesis block; reconciliation compares xpub custody with the node's unspent value; regtest in an opt-in Compose profile (`make up-bitcoin`), signet through an Esplora API | Done (stage 5); API 0.8.0 (ADR-045) | `go test ./internal/btc` (BIP84 vector, private keys refused), `go test ./internal/btcwatch` (incl. `TestBTCWatch_RegtestEndToEnd` on a real bitcoind), `make e2e` with `make up-bitcoin` (`bitcoin.spec.ts`) |
| Ethereum Sepolia | Not started (stage 5) | — |
| Public demo, "pull the plug", proof of reserves, installable app | Not started (stage 6) | — |

### Testnet deployments

**Base Sepolia (chain id 84532)**, deployed 2026-10-15 with `contracts/script/Deploy.s.sol`. The addresses are also in [`contracts/deployments/84532.json`](contracts/deployments/84532.json), which the services load and check against the chain at startup, and every transaction is in [`contracts/broadcast/Deploy.s.sol/84532/run-latest.json`](contracts/broadcast/Deploy.s.sol/84532/run-latest.json).

| Contract | Address |
|---|---|
| MockStablecoin (mUSDC) | [`0x69AC07E08782591417Ba77958869820563124590`](https://sepolia.basescan.org/address/0x69AC07E08782591417Ba77958869820563124590) |
| Vault | [`0x94cB9FBcba05DA1991c512Df8c9DB256D164F788`](https://sepolia.basescan.org/address/0x94cB9FBcba05DA1991c512Df8c9DB256D164F788) |
| ForwarderFactory | [`0xe3764E8c167133deFcb9cc1c400D87d01C92aaa4`](https://sepolia.basescan.org/address/0xe3764E8c167133deFcb9cc1c400D87d01C92aaa4) |
| DepositForwarder (implementation) | [`0xe95bB222F1333FDca26A4D0947C5AeBe4446F61C`](https://sepolia.basescan.org/address/0xe95bB222F1333FDca26A4D0947C5AeBe4446F61C) |
| BatchPayout (added 2026-10-16 with `script/AddBatchPayout.s.sol`) | [`0x062fc972FEa4b72e4b9516b06465f61cE7Eb3c62`](https://sepolia.basescan.org/address/0x062fc972FEa4b72e4b9516b06465f61cE7Eb3c62) |

MockStablecoin deployment transaction: [`0xfa17f799f469f95a6e8fffbf2940d34abdc6deb81ba6dfb8585e531ef5932b70`](https://sepolia.basescan.org/tx/0xfa17f799f469f95a6e8fffbf2940d34abdc6deb81ba6dfb8585e531ef5932b70) (block 48167483). All seven deployment transactions succeeded.

**One invoice paid on Base Sepolia**, 2026-10-16: a 25.00 USDC invoice, paid with 25 mUSDC to its deposit address in transaction [`0x42925b1a794017d171ea879f287c292db277bc3c7bc13381614eab86af38af69`](https://sepolia.basescan.org/tx/0x42925b1a794017d171ea879f287c292db277bc3c7bc13381614eab86af38af69) (block 48176412). The watcher, pointed at `https://sepolia.base.org`, detected it 36 seconds after the invoice was created, confirmed it four seconds later, and made it final about 15 minutes later, once Base Sepolia's `finalized` block passed it. It posted `deposit.confirmed` and `deposit.final`, and the invoice ended `settled`.

**One deposit swept on Base Sepolia**, 2026-10-16 (stage 2): a 30.00 USDC invoice was paid in [`0x04d12a49e63595f8e1df09141a3d285beb524094db0333fb6e2009f5ede420ef`](https://sepolia.basescan.org/tx/0x04d12a49e63595f8e1df09141a3d285beb524094db0333fb6e2009f5ede420ef) (block 48189929). About 20 minutes later, once Base Sepolia's `finalized` block passed it, the payment became final and the invoice `settled`. The worker added a sweep job, and the transaction manager sent `deployAndSweep` in [`0x6f698e90cc12e3fd621c732fd9fa282f0463bc59700102a1ee08fef3ace3b196`](https://sepolia.basescan.org/tx/0x6f698e90cc12e3fd621c732fd9fa282f0463bc59700102a1ee08fef3ace3b196) (block 48190509, nonce 14: the key had been used outside Waybill up to 13, and `NextNonce` started from the node's count). The forwarder was deployed and emptied into the vault in that one transaction, and the ledger moved 30.00 USDC from `custody:deposit` to `custody:vault`. The BatchPayout transactions are in [`contracts/broadcast/AddBatchPayout.s.sol/84532/run-latest.json`](contracts/broadcast/AddBatchPayout.s.sol/84532/run-latest.json).

The deployer key is a throwaway test key kept outside the repository. It was funded from the Sepolia proof-of-work faucet and bridged to Base Sepolia through Base's L1StandardBridge.

## Run it

Requirements: Linux or WSL2, Docker with Compose v2, Go 1.27+, Node 24 (see `.node-version`), Foundry 1.8+, GNU Make.

```bash
git clone --recurse-submodules https://github.com/JoshBlazer/waybill.git
cd waybill
make up      # PostgreSQL, Anvil, migrations, API on :8080, web on :3000
make test    # Go (incl. Docker-backed integration), Foundry, Vitest
make e2e     # Playwright against the running stack (pulls the Playwright image once)
make lint    # linters, formatters, type checks, codegen drift
make down

make up-observability  # make up plus metrics and traces: Grafana on :53001, dashboard "Waybill"
make reconcile         # check the ledger against the chain and the provider; fails on a difference
```

- API health: <http://localhost:8080/v1/health>
- Web: <http://localhost:3000>; create a test invoice at <http://localhost:3000/invoices/new>
- PostgreSQL: `localhost:55432`; Anvil: `localhost:58545`. Host ports avoid the usual 5432 and 8545 so the stack can run beside other projects; override them with the variables listed in `deploy/compose.yaml`.

Configuration is documented in [`.env.example`](.env.example). No secrets are stored in the repository.

## How it is built

| Part | Technology |
|---|---|
| Backend | Go, one binary (`api`, `worker`, `watcher`, `migrate`), PostgreSQL via pgx and sqlc, goose migrations |
| Background work | Transactional outbox with `FOR UPDATE SKIP LOCKED`; PostgreSQL is the only source of truth |
| API | REST under `/v1`, OpenAPI first, generated server and client types |
| Chains | go-ethereum (Base Sepolia first); btcsuite (stage 5) |
| Contracts | Solidity, Foundry, OpenZeppelin: CREATE2 deposit forwarders, vault, batch payout |
| Web | Next.js App Router, TypeScript, Tailwind; live updates over Server-Sent Events |
| Fiat | Paystack test mode |

Read in this order: [PRODUCT](docs/PRODUCT.md) → [ARCHITECTURE](docs/ARCHITECTURE.md) → [RISKS](docs/RISKS.md) → [DECISIONS](docs/DECISIONS.md) → [DESIGN](docs/DESIGN.md) → [ROADMAP](docs/ROADMAP.md).

## Built with AI assistance

This project is built with an AI coding assistant (Claude Code), including the stage 0 scaffold, and reviewed by the author. The modules where the money rules are hardest (the ledger posting engine, the transaction manager's nonce and fee logic, the payout state machine and the batch-run state machine) each started from a design note, signatures and failing tests written before any implementation, so the tests define what correct means: [ledger](docs/design/ledger.md), [transaction manager](docs/design/txmgr.md), [payouts](docs/design/payout.md), [batch runs](docs/design/teams.md).
