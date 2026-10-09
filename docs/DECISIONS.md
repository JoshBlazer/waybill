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

## ADR-029 Idempotency key claimed in the effect's own transaction
**Status:** Accepted, 2026-10-09.
**Context:** A common design writes an "in progress" idempotency row in its own transaction, does the work, then completes the row. A crash between the steps leaves a stuck key, and returning the original response requires a lock or polling.
**Decision:** The handler inserts the key with `ON CONFLICT DO NOTHING` inside the same transaction as the invoice and stores the response there before committing. A concurrent request with the same key blocks on the unique index until the first commits, then reads the stored response. If the first rolls back (validation error, crash), the key never existed.
**Consequences:** There are no stuck keys and no polling, and a failed request does not burn its key. A retry after a timeout replays the original. Proven by `TestCreateInvoice_ConcurrentSameKeyCreatesOne` and `TestCreateInvoice_FailedRequestDoesNotBurnKey`.

## ADR-030 Contract deployments are verified on-chain at startup
**Status:** Accepted, 2026-10-09.
**Context:** Deposit addresses are computed off-chain from the factory and implementation addresses. A wrong address in configuration would hand payers addresses that no deployed contract can sweep.
**Decision:** `deploy/` records addresses in `contracts/deployments/<chainId>.json`. The API and watcher refuse to start unless the chain confirms the factory's implementation, the factory's `predict()` agrees with Waybill's own CREATE2 computation, and the token has six decimals.
**Consequences:** A misconfiguration stops the service instead of losing funds. Local Anvil files are generated per run and not committed; public test network files are committed.

## ADR-031 Live updates through a same-origin SSE proxy; each event is the full view
**Status:** Accepted, 2026-10-09.
**Context:** The browser should not need the API's address or a CORS policy. Missed events on a flaky mobile connection must not leave the page wrong.
**Decision:** The web app exposes `/api/track/{code}/events`, which streams the API's SSE response through unchanged. Every event carries the complete tracking view, so a reconnecting client needs only the latest event, and `Last-Event-ID` needs no replay log.
**Consequences:** Events are a few hundred bytes larger than deltas, which is negligible next to the simplicity. The tracking page stays correct after any disconnection.

## ADR-032 End-to-end tests run in the Playwright container
**Status:** Accepted, 2026-10-09.
**Context:** Chromium needs system libraries that the development machine cannot install without root, and CI should run the same thing.
**Decision:** `make e2e` runs `mcr.microsoft.com/playwright:v1.64.0-noble` as the invoking user on the Compose network, against `web:3000` and `anvil:8545`. Tests pay through Anvil's unlocked dev account over JSON-RPC.
**Consequences:** No host setup, and identical behaviour locally and in CI. The image is large (about 2 GB) and is pulled once.

## ADR-033 JavaScript budget set from measurement
**Status:** Accepted, 2026-10-09. Revises the budget table in DESIGN.md §6.
**Context:** DESIGN.md set a 90 KB gzip JavaScript budget for the tracking page without measuring. On the running stack the page ships 177 KB, of which Waybill's own code is about 2 KB; the rest is the React DOM and Next.js runtime that any App Router page with a client component loads.
**Decision:** The budget is 185 KB gzip of initial JavaScript per page: the framework baseline plus about 15 KB for Waybill's code. `make budget` enforces it in CI, and also fails if wallet code appears in any initial load. The tracking page stays fully readable as server-rendered HTML before scripts run.
**Consequences:** The budget now guards Waybill's own growth instead of being permanently violated. Reaching 90 KB would mean leaving the App Router or removing all client components, which costs more than it gains. LCP, INP and CLS remain the user-facing measures, via Lighthouse once approved.

## ADR-034 Contracts image compiled at build time
**Status:** Accepted, 2026-10-09.
**Context:** The local deploy container downloaded the Solidity compiler at run time. A transient DNS failure broke `make up`, and `make down --volumes` discarded the compiler cache each time.
**Decision:** `contracts/Dockerfile` copies the sources and runs `forge build` at image build time. The one-shot `contracts` service only broadcasts the deployment to Anvil, as the image's non-root user.
**Consequences:** `make up` needs no network once images are built, and Docker's layer cache survives `make down`. The image rebuilds when contract sources change.

## ADR-035 Accessibility and performance checks: axe in Playwright, Lighthouse CI in the test container
**Status:** Accepted, 2026-10-09 (tooling approved by the owner).
**Context:** DESIGN.md §5 and §6 promise WCAG 2.1 AA and LCP, TBT and CLS budgets checked in CI. Both need tooling beyond the stack. `@lhci/cli` 0.15.1 pulls in about 17 packages with published advisories (puppeteer, proxy-agent and others), all build-time only.
**Decision:** `@axe-core/playwright` (pinned, dev dependency) runs in `e2e/accessibility.spec.ts` on every thin-slice page and interactive state, with tags `wcag2a`, `wcag2aa`, `wcag21a`, `wcag21aa`; any violation fails `make e2e`. Lighthouse CI is not a web dependency: `make lighthouse` runs `npx @lhci/cli@0.15.1` inside the pinned Playwright container, using its Chromium, against the Compose stack, three runs per page, median asserted against `web/lighthouserc.json`. Reports stay local (`upload.target: filesystem`); nothing is sent to a public server.
**Consequences:** `package-lock.json` and `npm audit` stay free of Lighthouse's dependency tree. Simulated throttling multiplies the host's real CPU speed, so results depend on the machine: on a loaded 4-core WSL host Lighthouse reported a `benchmarkIndex` of 250 to 740 and failed the TBT budget by a wide margin. The CI runner is the reference machine; local runs are indicative. If CI also fails, the fix is in the pages, not the budget.

## ADR-036 Lighthouse budgets set from measurement
**Status:** Accepted, 2026-10-09 (owner chose this over report-only or a client-component-free tracking page). Revises the LCP and TBT rows in DESIGN.md §6.
**Context:** The first CI runs (GitHub runner, Lighthouse benchmarkIndex 1,900 to 2,700) measured, as medians of three runs: tracking page LCP 2.44 to 2.45 s against a 2.0 s budget, and TBT 157 to 302 ms per run against 200 ms on every page. Individual LCP runs fall at about 1.9 s or about 2.5 s on every page, and include 2.50 s and 2.52 s on the tracking page. Unthrottled, the tracking page paints its largest text at 89 ms with CLS near zero. The simulated cost is the React and Next.js baseline (ADR-033), not Waybill's code. Removing the font preloads changed nothing measurable and was reverted.
**Decision:** LCP ≤ 2.6 s on the tracking page and ≤ 3.0 s elsewhere; TBT ≤ 350 ms on every page. CLS and total-transfer budgets are unchanged. The step remains a hard CI gate. The tracking page's 2.6 s replaces the 2.5 s first proposed, because 2.5 s sits inside the measured spread and would fail at random.
**Consequences:** The gate catches regressions, such as a heavy dependency or a blocking script, without failing on noise. Meeting the original figures would need a tracking page with no client components (live updates via a small plain script), which is the recorded alternative if real-device measurements show it matters.
