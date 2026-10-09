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
| Mock 6-decimal stablecoin | Done | `make test-contracts` |
| Local stack: PostgreSQL, Anvil, API, web | Done | `make up` |
| CI on every push | Done | [Actions](../../actions) |
| `money` package: exact integer amounts, no floats (AST-checked), property-tested | Done (stage 1) | `go test ./internal/money` |
| Invoice state machine: pure, all 72 (state, event) pairs tested | Done (stage 1) | `go test ./internal/statemachine` |
| Thin slice: invoice → pay on Anvil → watcher → ledger → live tracking page | In progress (stage 1) | — |
| Base Sepolia deployment | Not started (stage 1) | — |
| Reorgs, stuck transactions, payment edge cases, vault, webhooks | Not started (stage 2) | — |
| Naira settlement via Paystack test mode | Not started (stage 3) | — |
| Teams, batch pay, approvals, exports, reconciliation | Not started (stage 4) | — |
| Ethereum Sepolia, Bitcoin, RPC failover, observability | Not started (stage 5) | — |
| Public demo, "pull the plug", proof of reserves, installable app | Not started (stage 6) | — |

### Testnet deployments

None yet. Contract addresses and a transaction hash on Base Sepolia are recorded here at the end of stage 1.

## Run it

Requirements: Linux or WSL2, Docker with Compose v2, Go 1.27+, Node 24 (see `.node-version`), Foundry 1.8+, GNU Make.

```bash
git clone --recurse-submodules https://github.com/JoshBlazer/waybill.git
cd waybill
make up      # PostgreSQL, Anvil, migrations, API on :8080, web on :3000
make test    # Go (incl. Docker-backed integration), Foundry, Vitest
make lint    # linters, formatters, type checks, codegen drift
make down
```

- API health: <http://localhost:8080/v1/health>
- Web: <http://localhost:3000>
- PostgreSQL: `localhost:55432`; Anvil: `localhost:8545`. Host ports are overridable (see `deploy/compose.yaml`).

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

This project is built with an AI coding assistant (Claude Code). Three modules are written by hand by the author, Joshua; the assistant supplies only their design notes, signatures and failing tests, and reviews the author's implementation:

| Module | Status |
|---|---|
| Ledger posting engine | Not yet written (stage 1) |
| Payout state machine | Not yet written (stage 3) |
| Transaction manager nonce and fee logic | Not yet written (stage 2) |

Everything else, including this stage 0 scaffold, is written with the assistant and reviewed by the author. This table is updated whenever one of these modules changes.
