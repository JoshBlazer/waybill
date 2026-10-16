# Stage 1 walkthrough — Thin slice

## What was built

| Piece | Where | Why |
|---|---|---|
| `money` package | `api/internal/money` | Amounts are integers in minor units with an asset and its scale. Property tests (`rapid`) and an AST test that forbids float types in money code keep it that way (I2). |
| Ledger schema | `api/db/migrations/00002_ledger.sql`, [design note](../design/ledger.md) | Double-entry and append-only, enforced by PostgreSQL itself: a deferred constraint trigger checks that every entry balances per asset, triggers reject `UPDATE` and `DELETE`, and each account's `balance_rule` is a CHECK (I3, I4, ADR-028). |
| Posting engine | `api/internal/ledger/post.go` | `Post` validates, deduplicates on an idempotency key (comparing postings as a multiset), checks accounts and assets, and inserts postings in ascending account order so concurrent posts cannot deadlock. `Reverse` posts the negation exactly once (I5). |
| Invoice state machine | `api/internal/statemachine` | Pure functions over (state, event). All 72 pairs are tested, and the package may not import I/O packages (I7). |
| Idempotent requests | `idempotency_keys` | A money-moving request claims its key in the same transaction as its effect, so a crash cannot leave a claimed key without its effect or the reverse (I6, ADR-029). |
| Contracts | `contracts/src` | `ForwarderFactory` deploys a `DepositForwarder` clone per invoice at a CREATE2 address that can be computed before deployment, so a deposit address costs nothing until money arrives. `Vault` has per-transaction and per-day limits and a pause. |
| Deployment check | `api/internal/deployments` | At startup each service asks the chain whether the factory reports the configured implementation, whether its `predict()` matches Waybill's own CREATE2 computation, and whether the token has six decimals. A mismatch refuses startup (ADR-030). |
| API | `POST /v1/invoices`, `GET /v1/pay/{code}`, `GET /v1/track/{code}`, `GET /v1/track/{code}/events` | Generated from `openapi/waybill.yaml`. The events endpoint streams server-sent events fed by PostgreSQL `NOTIFY`. |
| Watcher | `api/internal/watcher` | Scans the token's `Transfer` logs in batches from a stored cursor, records payments to deposit addresses, confirms them after N blocks and makes them final at the chain's `finalized` block (a fixed depth on Anvil). Each step posts a ledger entry and moves the invoice (I8, I9, I12). |
| Web | `web/src/app` | Create an invoice, a payment link with address, amount and copy buttons, and a tracking page that updates live through a same-origin SSE proxy (ADR-031). Wallet connect (`wagmi`/`viem`) loads only after the payer chooses it. |
| Checks | `web/e2e`, `make budget`, `make lighthouse` | Playwright runs the whole path on the Compose stack (ADR-032), axe checks every page and state against WCAG 2.1 AA, and JavaScript and Lighthouse budgets were set from measurement (ADR-033, ADR-035, ADR-036). |
| Base Sepolia | `contracts/deployments/84532.json`, README | The same contracts on a public test network, verified by the services at startup, with one invoice paid there. |

## How a payment becomes money in the ledger

1. **Invoice.** `POST /v1/invoices` stores the invoice and, for each configured network, computes a deposit address from the factory, the implementation and a salt derived from the invoice id. Nothing is deployed yet.
2. **Detected.** The watcher sees a `Transfer` of the configured token to a watched address and records a payment (unique on network, transaction hash and log index, so seeing it twice is a no-op). The invoice moves to `received`. No ledger entry yet: a payment that has just been seen can still disappear.
3. **Confirmed.** After N blocks (2 on Anvil, 3 on public networks) the watcher checks that the payment's block is still the same block (its hash), then posts `deposit.confirmed`: custody +amount, contractor pending −amount. The invoice moves by comparing the confirmed total with the amount due (exact, under or over).
4. **Final.** Once the payment's block is at or below the chain's `finalized` block, the watcher posts `deposit.final` (pending to available) and settles the invoice when no confirmed payment is still waiting.
5. **Live view.** Each invoice event sends `NOTIFY`, the API pushes the full tracking view over SSE, and the page re-renders. A client that reconnects needs only the latest event.

If the node cannot say which block is final, nothing is made final: "couldn't find out" never counts as "yes" (I12).

## Exit check

Run on 2026-10-16.

- **Anvil.** `make e2e` against the Compose stack on `a9a96f9`: 4 passed (axe on every page and state, live tracking, settlement to final, browser wallet).
- **Base Sepolia.** The API and watcher ran from this branch's binary with `WAYBILL_CHAINS=evm:84532=https://sepolia.base.org`, against the local database. Both passed the test-money guard and verified the deployment on-chain. A 25.00 USDC invoice was paid in [`0x42925b1a…af38af69`](https://sepolia.basescan.org/tx/0x42925b1a794017d171ea879f287c292db277bc3c7bc13381614eab86af38af69): detected 36 s after creation, confirmed 4 s later, final about 15 minutes later. The ledger holds `deposit.confirmed` (custody +25.00, contractor pending −25.00) and `deposit.final` (pending +25.00, available −25.00), and the invoice is `settled`.

Two problems appeared only on the public network. The watcher would have scanned from genesis, so deployments now record a start block. The public RPC rejects `eth_getLogs` ranges over 200 blocks, so public networks query 200 blocks at a time. The rejection was retried without advancing the cursor, as I12 requires.

## Decisions worth defending

- **The database enforces the ledger (ADR-028).** Application checks can be skipped by a bug or a manual query; a deferred constraint trigger cannot. The engine still validates first, so callers get clear errors instead of constraint names.
- **Counterfactual deposit addresses.** CREATE2 lets every invoice have its own address without paying gas to create it. The cost is that a wrong factory or implementation address would hand out addresses nobody controls, which is why startup verifies the deployment on-chain.
- **Full view in every event (ADR-031).** Deltas would need a replay log to survive dropped connections on mobile networks; full views need nothing.
- **Budgets from measurement (ADR-033, ADR-036).** The first Lighthouse budgets were guesses and failed on the framework's own baseline. They now come from CI runs, with the 2.6 s tracking-page LCP recorded as a deviation from the 2.5 s target.
- **A start block per deployment.** On its first run the watcher starts at the block recorded at deployment, not at genesis. On Anvil 0 is correct; on Base Sepolia genesis was about 48 million blocks back. The loader refuses a public network without one.

## Interview questions

1. Why is the balance rule enforced with a deferred constraint trigger rather than an immediate one? What would break if it ran after each posting row?
2. `Post` inserts postings in ascending account order. Describe the deadlock this prevents, with two concurrent transfers.
3. The idempotency check compares postings as a multiset. Why not compare the request body byte for byte, and why not as an ordered list?
4. A deposit address exists before any contract is deployed there. How is it computed, who can move money out of it, and what would happen if the configured implementation address were wrong?
5. Why does a payment get no ledger entry when it is first detected? What would a reorg do to a ledger that credited on sight?
6. The watcher confirms a payment only if its block hash is unchanged. Walk through a reorg that replaces the block after detection and before confirmation.
7. What does the watcher do when the RPC returns an error halfway through a batch? Which invariant does that protect, and how is it tested?
8. Why does every SSE event carry the full tracking view? What does `Last-Event-ID` cost with that design compared with deltas?
9. The public Base Sepolia RPC caps `eth_getLogs` at 200 blocks. How did that show up, why did it not lose a payment, and where does the batch size live?
10. The tracking-page LCP budget is 2.6 s, not the 2.5 s the design asked for. How was that number chosen, and when would you tighten it?
