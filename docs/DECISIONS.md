# Waybill — Decision records

Append-only. To change a decision, add a new record that supersedes the old one and update the old record's status line only. Proposed records wait for the project owner's approval before any code changes.

Format: **Status** · **Context** · **Decision** · **Consequences**.

---

## ADR-001 Go for the backend
**Status:** Accepted, 2026-10-09.
**Context:** The backend moves money, talks to chains and must be readable in an interview. The target roles use Go widely in fintech and blockchain infrastructure (go-ethereum, btcd).
**Decision:** Go for every server-side component.
**Consequences:** Strong standard library (HTTP, `slog`, `crypto`) and first-class chain libraries. Explicit error handling suits "couldn't find out" results.

## ADR-002 PostgreSQL as the only source of truth
**Status:** Accepted, 2026-10-09.
**Context:** Ledger correctness depends on transactions, constraints and triggers that application code cannot bypass.
**Decision:** PostgreSQL holds all state: ledger, domain state, outbox, idempotency keys, chain view, webhook logs.
**Consequences:** One system to back up and reason about. Invariants such as entry balance and append-only are enforced by the database.

## ADR-003 pgx with sqlc
**Status:** Accepted, 2026-10-09.
**Context:** Queries must be explicit SQL that a reviewer can read, with typed results and no ORM magic around money.
**Decision:** `pgx/v5` as the driver and pool; `sqlc` generates typed Go from SQL in `api/db/queries`.
**Consequences:** SQL is reviewed as SQL. Schema changes surface as compile errors.

## ADR-004 goose for migrations
**Status:** Accepted, 2026-10-09.
**Context:** Migrations must run in CI, in testcontainers and from the binary.
**Decision:** `goose` with SQL migrations in `api/db/migrations`, embedded in the binary and run by `waybill migrate`.
**Consequences:** One migration path everywhere. sqlc reads the same files as its schema.

## ADR-005 One binary with subcommands
**Status:** Accepted, 2026-10-09.
**Context:** API, worker and watcher share the domain packages but must scale and fail independently.
**Decision:** A single `waybill` binary with subcommands `api`, `worker`, `watcher`, `migrate`, `healthcheck`. Each runs as its own process.
**Consequences:** One build and one image. Killing a worker does not touch the API, which the "pull the plug" demo relies on.

## ADR-006 Transactional outbox, no broker
**Status:** Accepted, 2026-10-09.
**Context:** Background work must never be lost or duplicated in its effect, and the machine has 7 GB of RAM.
**Decision:** An `outbox` table written in the same transaction as the state change, leased by workers with `FOR UPDATE SKIP LOCKED`. Live updates use `LISTEN/NOTIFY`. No Redis, no message broker.
**Consequences:** At-least-once delivery, so every handler is idempotent. Throughput is bounded by PostgreSQL, which is far above this project's needs.

## ADR-007 REST, OpenAPI first, generated on both sides
**Status:** Accepted, 2026-10-09.
**Context:** A contract change must fail the build on both sides rather than at runtime.
**Decision:** `openapi/waybill.yaml` is written first. `oapi-codegen` generates Go server types (`std-http` mode); `openapi-typescript` generates TypeScript types. `make lint` fails if generated files differ from what is committed.
**Consequences:** The OpenAPI file is the only place the contract is defined. Changing the public API requires owner approval.

## ADR-008 go-ethereum; Base Sepolia first, Ethereum Sepolia second
**Status:** Accepted, 2026-10-09.
**Context:** The EVM is where stablecoin payers are, and Base has low fees and fast blocks.
**Decision:** `go-ethereum` (`ethclient`, `types`, `crypto`) for EVM access. Base Sepolia (84532) is the first public test network; Ethereum Sepolia (11155111) is added in stage 5.
**Consequences:** OP-stack finality (`finalized` tag) and Ethereum finality are both handled through the per-network policy in the watcher.

## ADR-009 btcsuite for Bitcoin, stage 5 only
**Status:** Accepted, 2026-10-09.
**Context:** Bitcoin receive broadens the product but is not needed for the thin slice.
**Decision:** `btcsuite` against `bitcoind` in regtest locally (opt-in Compose profile) and signet for the demo. The server holds only a watch-only extended public key.
**Consequences:** No Bitcoin spending from the server. The default local stack stays light.

## ADR-010 Foundry and OpenZeppelin for contracts
**Status:** Accepted, 2026-10-09.
**Context:** Contracts must be fuzzed and invariant-tested, using audited building blocks.
**Decision:** Foundry (`forge`, `anvil`) with OpenZeppelin Contracts. Libraries are git submodules pinned to release tags.
**Consequences:** Tests in Solidity with fuzz and invariant support. Anvil serves as both the dev chain and the reorg lab.

## ADR-011 Per-invoice deposit addresses as CREATE2 minimal proxies
**Status:** Accepted, 2026-10-09.
**Context:** Attributing a payment by amount or memo is unreliable. Each invoice needs its own address, without deploying a contract per invoice up front.
**Decision:** EIP-1167 clones of `DepositForwarder`, deployed by `ForwarderFactory` at `CREATE2(salt = keccak256(invoiceId))`. The address is computed in advance and the clone is deployed only when sweeping.
**Consequences:** Deployment gas is paid only for paid invoices. Funds at an undeployed address are recoverable because the address is deterministic.

## ADR-012 Vault with roles, limits and pause; batch payout contract
**Status:** Accepted, 2026-10-09.
**Context:** A compromised hot key must have a bounded impact.
**Decision:** A `Vault` with `AccessControl` roles, per-transaction and daily limits, and `Pausable`. A `BatchPayout` contract that pays many recipients in one transaction and rejects reused payout ids.
**Consequences:** Outflow loss is capped per day. Double payment is blocked on-chain as well as in the database.

## ADR-013 Mock six-decimal stablecoin for local work
**Status:** Accepted, 2026-10-09.
**Context:** Local work and CI need a token that behaves like USDC (6 decimals) and can be minted freely.
**Decision:** `MockStablecoin`, an OpenZeppelin ERC-20 with 6 decimals and open minting, deployed only to test networks.
**Consequences:** Decimal-handling bugs surface locally, because the scale is not 18.

## ADR-014 Next.js App Router, TypeScript, Tailwind, TanStack Query, wagmi and viem
**Status:** Accepted, 2026-10-09.
**Context:** The tracking page must be server-rendered and fast on cheap phones. Wallet pages need a mature EVM stack.
**Decision:** Next.js App Router with TypeScript and Tailwind. TanStack Query for client data. `wagmi` and `viem` for wallets, loaded only on wallet routes. Each library is added in the stage that first uses it (TanStack Query, wagmi and viem in stage 1).
**Consequences:** Server components keep JavaScript off the tracking page. Unused libraries are not installed early.

## ADR-015 Server-Sent Events for live updates
**Status:** Accepted, 2026-10-09.
**Context:** Updates flow one way (server to browser) and must survive flaky mobile connections.
**Decision:** SSE from the API, fed by PostgreSQL `LISTEN/NOTIFY`, with replay via `Last-Event-ID` from `invoice_events`.
**Consequences:** Plain HTTP and automatic browser reconnection, with no WebSocket server.

## ADR-016 Paystack test mode; verification and rates behind interfaces
**Status:** Accepted, 2026-10-09.
**Context:** Paystack covers card and bank pay-in and naira transfers in a test mode. The identity and rate sandbox providers are not chosen yet.
**Decision:** Paystack test mode for pay-in and payout. `Verifier` and `RateSource` interfaces, each with a fake implementation first. The sandbox provider for each is chosen in a later record.
**Consequences:** Stages 1–2 do not wait on provider choice. The test-money guard requires `sk_test_` keys.

## ADR-017 Testing stack
**Status:** Accepted, 2026-10-09.
**Context:** Money code needs unit tests, property tests and tests against real infrastructure.
**Decision:** Go `testing` plus `rapid` for properties. `testcontainers-go` for real PostgreSQL and Anvil. Foundry fuzz and invariant tests. Vitest for web units. Playwright for e2e.
**Consequences:** Integration tests need Docker, which CI runners provide.

## ADR-018 GitHub Actions on every push
**Status:** Accepted, 2026-10-09.
**Decision:** Separate jobs for api, contracts, web, codegen drift and a Compose smoke test.
**Consequences:** A failure points at one part of the system.

## ADR-019 Light default local stack
**Status:** Accepted, 2026-10-09.
**Context:** The development machine is WSL2 with 4 threads and about 7 GB of RAM.
**Decision:** `make up` runs PostgreSQL, Anvil, a one-shot migration, the API and the web app. Heavier services (bitcoind, worker and watcher replicas, observability) sit behind opt-in Compose profiles.
**Consequences:** The default stack fits comfortably in memory.

## ADR-020 Standard library HTTP and logging
**Status:** Accepted, 2026-10-09.
**Context:** Go's `net/http` mux supports method and path patterns, and `log/slog` provides structured logs.
**Decision:** No router or logging dependency. `oapi-codegen` targets `std-http`. Logs go through `slog`.
**Consequences:** Fewer dependencies to explain. `oapi-codegen/runtime` is the only HTTP-adjacent dependency.

## ADR-021 Tooling: golangci-lint, npm, fnm, `go tool`
**Status:** Accepted, 2026-10-09.
**Decision:**
- Go code generators (`oapi-codegen`, `sqlc`, `goose`) are pinned as `tool` dependencies in `api/go.mod` and run with `go tool`.
- `golangci-lint` is pinned by version: installed to `./bin` locally, and run by its official action in CI.
- npm is the web package manager. Node is pinned in `.node-version` and installed with fnm.
**Consequences:** The same tool versions run locally and in CI with no global installs.

## ADR-022 Amounts as NUMERIC(78,0) in minor units
**Status:** Accepted, 2026-10-09. Amended by ADR-028 (column type).
**Context:** `BIGINT` cannot hold every `uint256` (for example large wei values). Floats are forbidden near money.
**Decision:** All amount columns are `NUMERIC(78,0)` in minor units, with the asset (and so its scale) on the row. Go represents them as `money.Amount` (a `big.Int` paired with an asset). Rates are stored as integer numerator and denominator.
**Consequences:** Exact arithmetic everywhere, with explicit rounding at conversion, where the rounding rule is recorded.

## ADR-023 Test-money guard checks the live chain ID
**Status:** Accepted, 2026-10-09.
**Context:** An allowlist on configured chain IDs alone would trust config. A config that says 31337 while its RPC URL points at mainnet would pass.
**Decision:** At startup, each EVM RPC is queried for `eth_chainId` and must match the configured network. An RPC that cannot answer, or a network without a live check, fails closed. Error messages show the RPC host only.
**Consequences:** Services need their chain RPC reachable at startup. The Compose stack orders Anvil before the API.

## ADR-024 Repository lives on the WSL filesystem
**Status:** Accepted, 2026-10-09.
**Context:** Docker bind mounts, `node_modules` and Go builds are much slower on `/mnt/c`.
**Decision:** The repository lives at `~/waybill` inside WSL Ubuntu, and all commands run there.
**Consequences:** Windows editors open it through `\\wsl.localhost\Ubuntu\home\…` or a WSL remote.

## ADR-025 No error colour; one accent only
**Status:** Accepted, 2026-10-09.
**Context:** The design reserves one accent colour for "delivered". Errors conventionally use red, which would add a second accent and make colour carry meaning.
**Decision:** No red. Errors are shown with ink, the `PROBLEM` stamp shape, a "!" glyph, a thick left rule on invalid fields, and plain words.
**Consequences:** Status never depends on colour perception (WCAG 1.4.1), and print and greyscale keep their meaning. Green on screen always means "delivered". If usability testing shows errors are missed, this record is superseded rather than quietly changed.

## ADR-026 Accept the ESLint `braces` advisory as a development-only risk
**Status:** Accepted, 2026-10-09.
**Context:** `npm audit` reports five high-severity findings, all from `braces` (GHSA-vfj7-8cjw-p6xm, a denial of service through deeply nested glob patterns). They reach the project through `eslint-config-next` → `@next/eslint-plugin-next` → `fast-glob` → `micromatch`. The advisory covers every published version, and npm's only offered fix is downgrading to the Next.js 14 ESLint config.
**Decision:** Accept it. The package runs only at lint time, on glob patterns written in this repository, and never ships to users.
**Consequences:** `npm audit` is not a CI gate for now. This record is revisited when a fixed `braces` or plugin release exists; the check is `npm audit --omit=dev`, which must stay clean.

## ADR-027 Secret scanning with gitleaks in CI
**Status:** Accepted, 2026-10-09.
**Context:** "No secrets in the repository" needs a check, not only a `.gitignore`.
**Decision:** `make secrets` runs the gitleaks container (`ghcr.io/gitleaks/gitleaks:v8.30.1`) over the full git history with the default rules plus `.gitleaks.toml`. The allowlist names exact fake strings in exact files, never whole rules. CI runs it in the `secrets` job. It is a CI tool only, not a code dependency.
**Consequences:** A committed secret fails CI even when later removed from the tip, because history is scanned. A real leak still requires rotating the key; the scan only detects it.

## ADR-028 Amounts use a `minor_units` domain, not NUMERIC(78,0)
**Status:** Accepted, 2026-10-09 (owner approved the ledger schema).
**Context:** ADR-022 chose `NUMERIC(78,0)`. PostgreSQL coerces a value to the column's scale **before** `CHECK` constraints run, so `INSERT … VALUES (1.5)` into a `numeric(78,0)` column with `CHECK (v = trunc(v))` silently stores `2`. Verified on PostgreSQL 17. Silent rounding of money is exactly what the project forbids.
**Decision:** Every amount column uses `CREATE DOMAIN minor_units AS numeric CHECK (scale(VALUE) = 0 AND abs(VALUE) < 1e78)`. Unconstrained `numeric` keeps the value as sent, and the domain rejects any fractional scale. The 78-digit bound (any `uint256`) is unchanged. Go still uses `money.Amount`; sqlc maps the domain to it.
**Consequences:** A fractional amount is an error at insert, never a rounded value (`TestLedger_FractionalAmountRejected`). The integer-only intent of ADR-022 now actually holds.
