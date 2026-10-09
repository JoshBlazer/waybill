# Waybill — Roadmap

Each stage ends only when its exit check has been run and has passed. The exit-check command and its result are recorded in the stage's walkthrough. Every stage ends with a walkthrough of what was built and why, plus ten interview questions about it.

Marks: **[H]** means the project owner writes the implementation by hand. For these tasks the assistant writes the design note, the function signatures and the failing tests, then stops and reviews the owner's code.

## Stage 0 — Foundations

- [x] Product, architecture, design, roadmap, decisions and risks documents; `CLAUDE.md`; README with status table.
- [x] Monorepo scaffold: `api/` (Go), `web/` (Next.js), `contracts/` (Foundry), `openapi/`, `deploy/`, `Makefile`.
- [x] Test-money guard (`api/internal/safety`) with tests: chain allowlist, live chain-ID check, Paystack `sk_test_` check.
- [x] OpenAPI contract with `GET /v1/health`; generated Go server types and TypeScript types; codegen drift check.
- [x] Empty first goose migration, applied in an integration test against real PostgreSQL (testcontainers).
- [x] One passing test in each codebase.
- [x] Compose stack: PostgreSQL, Anvil, migrate, API, web.
- [x] GitHub Actions CI on every push.

**Exit check:** a fresh clone passes `make up`, `make test` and `make lint`, and CI is green.

**Result (2026-10-09): passed.** Fresh clone of `1728ea2` in `/tmp/waybill-fresh`: `make up`, `make test`, `make lint` all exit 0, `curl /v1/health` returns 200, and the working tree stays clean. CI run 37875145536: all five jobs green. See [walkthroughs/stage-0.md](walkthroughs/stage-0.md).

## Stage 1 — Thin slice

Build the whole path end to end before deepening any part of it.

- [x] `money` package: `Amount`, assets and scales. Property tests with `rapid`. AST test that forbids floats in money packages.
- [ ] **[H] Ledger posting engine.** Assistant part done on branch `stage1/ledger-engine`: [design note](design/ledger.md), schema migration `00002_ledger.sql` (14 database-level tests pass), signatures in `internal/ledger/post.go`, 18 failing engine tests. Schema approved (ADR-028). Waiting on: the owner's implementation.
- [x] Invoice state machine (pure) with an exhaustive pair test.
- [ ] Idempotency middleware and `idempotency_keys`.
- [ ] Outbox table and worker leasing loop.
- [x] Contracts: `DepositForwarder`, `ForwarderFactory`, minimal `Vault`. Foundry unit and fuzz tests. Deploy script for Anvil.
- [ ] `POST /v1/invoices`, `GET /v1/pay/{code}`, `GET /v1/track/{code}`, `GET /v1/track/{code}/events` (SSE).
- [ ] Watcher on Anvil: detect, confirm and finalize a deposit; post ledger entries; emit invoice events.
- [ ] Web: create invoice, payment link (stablecoin only, wallet connect with `wagmi`/`viem`), tracking page with live updates.
- [ ] Playwright e2e against the Compose stack: `make e2e`.
- [ ] Lighthouse CI and axe checks in CI, with the budgets from DESIGN.md.
- [ ] Repeat on Base Sepolia: deploy contracts, pay one invoice, record addresses and one transaction hash in the README.

**Exit check:** `make e2e` proves the whole path on Anvil, and the README records Base Sepolia contract addresses and one transaction hash.

## Stage 2 — Hardening

- [ ] Reorg handling in the watcher (block-hash chain, walk back, reverse entries). Test forces a reorg on Anvil with `evm_snapshot`/`evm_revert`.
- [ ] **[H] Transaction manager nonce and fee logic.** The assistant provides: a design note, signatures (`txmgr.NextNonce`, `txmgr.Replacement`, …) and failing tests for I10 and I11, including `rapid` properties.
- [ ] Transaction manager plumbing: single-writer lease, broadcast, attempt recording, receipt watching.
- [ ] Gas estimation failure handling.
- [ ] Underpaid, overpaid, late and duplicate payments.
- [ ] Sweeping to the vault; vault limits and pause.
- [ ] `BatchPayout` contract with the payout-id guard; Foundry invariant tests.
- [ ] Public API keys, and outgoing webhooks with signature, retries and delivery log.

**Exit check:** for each failure listed above, a test forces it and passes. The command runs in CI.

## Stage 3 — Naira settlement *(first public milestone)*

- [ ] Rate provider interface. Fake implementation, then a decision record choosing the sandbox provider.
- [ ] Rate lock at invoice creation; re-quote on expiry or late payment.
- [ ] Identity and bank-name verification interface. Fake implementation, then a decision record choosing the sandbox provider.
- [ ] **[H] Payout state machine.** The assistant provides: a design note, `statemachine.PayoutTransition` signature, and the exhaustive failing pair table.
- [ ] Paystack test-mode payout: transfer recipient, transfer, reconcile by reference, `unknown` state.
- [ ] Paystack incoming webhooks: verify, store raw, process once.
- [ ] Card and bank pay-in through Paystack test checkout.
- [ ] Underpayment resolution flow.

**Exit check:** a link is paid and a test-mode naira payout completes end to end, proven by an automated test against Paystack test mode.

## Stage 4 — Teams

- [ ] Payer organisations, members and roles.
- [ ] Batch pay: build, check, approve (approver ≠ creator), send through `BatchPayout`.
- [ ] Invoices, receipts and accountant export (CSV and PDF).
- [ ] Contractor balance and conversion at a quoted rate.
- [ ] Reconciliation job: ledger against chain and provider, with a report.

**Exit check:** an automated test runs a batch with dual approval to completion, and the reconciliation report shows zero differences.

## Stage 5 — Breadth and operations

- [ ] Ethereum Sepolia as a second EVM network.
- [ ] Bitcoin receive on regtest (opt-in Compose profile) and signet: xpub-only address derivation, watcher, reorgs.
- [ ] RPC failover and node health monitoring.
- [ ] Metrics and traces (OpenTelemetry; exporter chosen in a decision record).

**Exit check:** the same e2e path passes on Ethereum Sepolia and Bitcoin regtest, and failover is proven by a test that kills the primary RPC.

## Stage 6 — Launch

- [ ] Public demo deployment.
- [ ] "Pull the plug" demo: kill worker mid-payout, force reorg, replay webhook, each with before-and-after proof.
- [ ] Proof of reserves page with Merkle proofs.
- [ ] Installable web app (manifest, service worker, offline tracking page).
- [ ] Accessibility audit against WCAG 2.1 AA.
- [ ] Known-risks page (public version of RISKS.md).
- [ ] Write-up.

**Exit check:** the public demo runs all three "pull the plug" scenarios with no lost or doubled money, Lighthouse and axe pass on production, and the write-up is published.
