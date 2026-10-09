# CLAUDE.md

## What Waybill is

Waybill lets small web3 teams pay their Nigerian contractors in stablecoins, and lets those contractors receive naira in their bank account. Every payment is trackable like a parcel on a public page: received, confirmed, converted, sent to bank. It is a portfolio project that runs on **test networks and provider sandboxes only**, and is judged by engineers who read the code and by non-technical visitors who spend a minute on the demo.

## Commands

All commands run inside WSL Ubuntu from `~/waybill`.

| Command | What it does |
|---|---|
| `make up` / `make down` | Start (and wait for healthy) / stop the local stack: PostgreSQL, Anvil, migrate, API, web |
| `make test` | Go tests (incl. testcontainers), Foundry tests, Vitest |
| `make lint` | golangci-lint, `forge fmt --check`, ESLint, `tsc`, Prettier, codegen drift check |
| `make gen` | Regenerate Go server types, sqlc code and TS types from `openapi/` and `api/db/` |
| `make e2e` | Playwright end to end (stage 1; fails until then) |
| `make help` | List every target |

Single suites: `make test-api`, `make test-contracts`, `make test-web`. Fast Go loop: `cd api && go test -short ./...` skips Docker-backed tests.

## Repository layout

```
api/                 Go module, one binary: cmd/waybill (api | worker | watcher | migrate | healthcheck)
  internal/safety    test-money guard (runs first in every service)
  internal/config    environment loading
  internal/httpapi   generated server (api.gen.go) + handlers
  internal/store     sqlc output; queries in db/queries, migrations in db/migrations (goose)
contracts/           Foundry; src/, test/, lib/ (forge-std, OpenZeppelin as pinned submodules)
web/                 Next.js App Router; src/lib/api/schema.d.ts is generated
openapi/waybill.yaml The API contract. Edit here first, then `make gen`
deploy/compose.yaml  Local stack; heavy services behind profiles
docs/                Product, architecture, design, roadmap, decisions, risks
```

Generated files (`api.gen.go`, `internal/store/*`, `schema.d.ts`) are committed and never edited by hand.

## Safety rule: test money only

- Services refuse to start unless every chain in `WAYBILL_CHAINS` is one of `evm:31337`, `evm:84532`, `evm:11155111`, `btc:regtest`, `btc:signet`, **and** each EVM RPC's live `eth_chainId` matches. "Couldn't confirm" counts as a failure.
- Services refuse to start if `PAYSTACK_SECRET_KEY` is set and does not begin with `sk_test_`.
- No secrets in the repository. `.env.example` documents variables; real values come from the environment. Never print RPC URLs (they hold API keys); show the host only.
- Do not weaken or bypass `internal/safety`. Changes to it need tests first.

## Rules the architecture enforces

Full list with proving tests: [docs/ARCHITECTURE.md §6](docs/ARCHITECTURE.md#6-invariants).

1. **Money is integers.** Minor units with an explicit asset and scale; `NUMERIC(78,0)` in SQL, `money.Amount` in Go. No floats near money.
2. **Ledger is double-entry and append-only.** Each entry balances to zero per asset, enforced by a database trigger. Corrections are reversing entries; no UPDATE or DELETE.
3. **Money-moving requests are idempotent.** Creating endpoints require `Idempotency-Key`; a retry returns the original result.
4. **State machines are pure and exhaustively tested.** No I/O in `internal/statemachine`; a test for every (state, event) pair.
5. **A payment is not final when first seen.** Detected, then confirmed, then final; reorgs are detected by block hash and reverse ledger entries.
6. **One writer per signing key.** The transaction manager owns nonces, bumps both EIP-1559 fees to replace stuck transactions, and records every hash.
7. **"I couldn't find out" is its own answer.** RPC or provider failure is never "not paid" or "not sent".
8. **Webhooks are signed both ways.** Outgoing: timestamped HMAC, retries with backoff, delivery log. Incoming: verify, store raw, process once per event ID.
9. **Keys stay out of reach.** Signing behind an interface; keys never logged; Bitcoin side holds only an xpub.
10. **PostgreSQL is the only source of truth.** Background work goes through the outbox (`FOR UPDATE SKIP LOCKED`). No Redis, no broker.
11. **The OpenAPI file is the contract.** Change it first; generated code must match (`make lint` checks).

## How we work

- **Hand-written modules.** For these three, Claude writes the design note, function signatures and failing tests, then **stops**. The owner implements; Claude reviews:
  - the ledger posting engine
  - the payout state machine
  - the nonce and fee logic in the transaction manager
- **Everything else** Claude may implement, in small reviewable commits.
- **Proof before claims.** Never say something works unless a command run in this session showed it. Name the command when reporting.
- **Honest, current documents.** The README status table changes in the same commit as the work it describes. Neutral engineering voice; decisions recorded with reasons. The README's AI-assistance statement must stay true.
- **Ask first** before: adding infrastructure; adding a dependency not already in [docs/DECISIONS.md](docs/DECISIONS.md); changing the ledger schema; changing the public API.
- **Stack changes** need a proposed record in [docs/DECISIONS.md](docs/DECISIONS.md) and owner approval before code changes.
- **Thin slice first.** Build stage 1 end to end before deepening any part.
- **End of each stage:** a walkthrough of what was built and why, and ten interview questions about it.
- Commit messages: imperative mood, one logical change per commit.
- `web/AGENTS.md`: this Next.js version differs from older training data; read `web/node_modules/next/dist/docs/` before using a Next.js API.

## Documents

- [docs/PRODUCT.md](docs/PRODUCT.md): users, journeys, scope, non-goals, definition of done
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): components, diagrams, data model, state machines, invariants, failure modes
- [docs/DESIGN.md](docs/DESIGN.md): tokens, type, motifs, screens state by state, accessibility, performance budgets
- [docs/ROADMAP.md](docs/ROADMAP.md): stages, tasks, exit checks
- [docs/DECISIONS.md](docs/DECISIONS.md): append-only decision records
- [docs/RISKS.md](docs/RISKS.md): every way money could be lost or misdirected, and the guard
