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
| **Stage 2 — Hardening** | In progress | [roadmap](docs/ROADMAP.md#stage-2--hardening) |
| Reorg handling: detect by block hash, walk back to the common ancestor, reverse confirmed payments, rescan | Done (stage 2) | `go test ./internal/watcher` (fake-chain reorg tests; `TestWatcher_ReorgReversesConfirmedPayment` forces a reorg on Anvil with `evm_snapshot`/`evm_revert`) |
| Payment edge cases: payments in parts, top-ups, overpayment, paying twice, payment after settlement, invoice expiry and late payment | Done (stage 2) | `go test ./internal/watcher` (`edge_test.go`) |
| Transaction manager nonce and fee logic (hand-written) | Design note, signatures and failing tests ready; awaiting implementation | [PR #7](../../pull/7) |
| `BatchPayout` contract: all-or-nothing batches through the vault's payout-id guard and limits | Done (stage 2); deployed with the payout plumbing | `make test-contracts` (10 unit tests, 4 invariants) |
| Outbox and worker: jobs enqueued in the same transaction as the change, leased with `SKIP LOCKED`, retried with backoff, dead after the last attempt; invoice expiry runs in the worker | Done (stage 2) | `go test ./internal/outbox` (12 tests incl. 8 concurrent workers) |
| API key management (create, list, revoke) and signed outgoing webhooks: invoice events delivered by the worker, retried for about a day, every attempt logged, SSRF guard on endpoint URLs | Done (stage 2) | `go test ./internal/webhook ./internal/httpapi` |
| Transaction manager plumbing, sweeping | Not started (stage 2) | — |
| Naira settlement via Paystack test mode | Not started (stage 3) | — |
| Teams, batch pay, approvals, exports, reconciliation | Not started (stage 4) | — |
| Ethereum Sepolia, Bitcoin, RPC failover, observability | Not started (stage 5) | — |
| Public demo, "pull the plug", proof of reserves, installable app | Not started (stage 6) | — |

### Testnet deployments

**Base Sepolia (chain id 84532)**, deployed 2026-10-15 with `contracts/script/Deploy.s.sol`. The addresses are also in [`contracts/deployments/84532.json`](contracts/deployments/84532.json), which the services load and check against the chain at startup, and every transaction is in [`contracts/broadcast/Deploy.s.sol/84532/run-latest.json`](contracts/broadcast/Deploy.s.sol/84532/run-latest.json).

| Contract | Address |
|---|---|
| MockStablecoin (mUSDC) | [`0x69AC07E08782591417Ba77958869820563124590`](https://sepolia.basescan.org/address/0x69AC07E08782591417Ba77958869820563124590) |
| Vault | [`0x94cB9FBcba05DA1991c512Df8c9DB256D164F788`](https://sepolia.basescan.org/address/0x94cB9FBcba05DA1991c512Df8c9DB256D164F788) |
| ForwarderFactory | [`0xe3764E8c167133deFcb9cc1c400D87d01C92aaa4`](https://sepolia.basescan.org/address/0xe3764E8c167133deFcb9cc1c400D87d01C92aaa4) |
| DepositForwarder (implementation) | [`0xe95bB222F1333FDca26A4D0947C5AeBe4446F61C`](https://sepolia.basescan.org/address/0xe95bB222F1333FDca26A4D0947C5AeBe4446F61C) |

MockStablecoin deployment transaction: [`0xfa17f799f469f95a6e8fffbf2940d34abdc6deb81ba6dfb8585e531ef5932b70`](https://sepolia.basescan.org/tx/0xfa17f799f469f95a6e8fffbf2940d34abdc6deb81ba6dfb8585e531ef5932b70) (block 48167483). All seven deployment transactions succeeded.

**One invoice paid on Base Sepolia**, 2026-10-16: a 25.00 USDC invoice, paid with 25 mUSDC to its deposit address in transaction [`0x42925b1a794017d171ea879f287c292db277bc3c7bc13381614eab86af38af69`](https://sepolia.basescan.org/tx/0x42925b1a794017d171ea879f287c292db277bc3c7bc13381614eab86af38af69) (block 48176412). The watcher, pointed at `https://sepolia.base.org`, detected it 36 seconds after the invoice was created, confirmed it four seconds later, and made it final about 15 minutes later, once Base Sepolia's `finalized` block passed it. It posted `deposit.confirmed` and `deposit.final`, and the invoice ended `settled`.

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

This project is built with an AI coding assistant (Claude Code). Two modules are written by hand by the author, Joshua; the assistant supplies only their design notes, signatures and failing tests, and reviews the author's implementation:

| Module | Status |
|---|---|
| Payout state machine | Not yet written (stage 3) |
| Transaction manager nonce and fee logic | Not yet written (stage 2) |

To check that its failing tests are correct and satisfiable, the assistant may run them against a throwaway implementation. That code is deleted, never committed and never shown; the committed implementation is the author's. Everything else, including the stage 0 scaffold, is written with the assistant and reviewed by the author. This table is updated whenever one of these modules changes.
