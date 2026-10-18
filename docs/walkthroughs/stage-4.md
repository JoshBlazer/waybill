# Stage 4 walkthrough — Teams

Stage 3 paid one contractor at a time. Stage 4 lets a payer team pay many contractors at once, with a second person approving large runs. It gives both sides the paperwork they need, lets contractors turn a stablecoin balance into naira, and proves on a schedule that the books match the chain and the provider.

## What was built

| Piece | Where | Why |
|---|---|---|
| Organisations, members, roles | `internal/org`, `internal/auth`, migration 00012, ADR-043 | A payer organisation has members with `admin`, `maker` and `approver` roles and an approval threshold. Member keys are a separate kind from contractor keys. The middleware refuses a key of the wrong kind with 401, so neither can call the other's operations. The last admin cannot be removed or demoted. |
| Funding addresses | `internal/watcher`, `internal/sweep`, `evm.SaltForOrg` | Each organisation gets its own CREATE2 forwarder. Deposits are credited pending, then available at finality, and swept into the vault exactly like invoice payments. |
| Batch-run state machine | `internal/statemachine/batch.go` | Pure, and every one of the 100 (state, event) pairs is tested. Money leaves only after an approval, and a sent run cannot be cancelled. |
| Batch pay | `internal/batch` | Every line is checked: a verified contractor, a wallet past its 24-hour wait, the vault limits, no repeat payment in 30 days, and enough funds. The run is approved by someone other than its creator (refused in SQL too), reserved in the ledger, and paid in one `BatchPayout.payBatch` transaction. It is booked out of the vault only at finality, and a reverted run returns its money. |
| Web for teams | `web/src/app/org`, `web/src/app/account/wallet` | The organisation page, building a run, a run page that offers each member only what their role allows, a demo switch between the two seeded members, and the contractor's payout wallet. |
| Reconciliation | `internal/reconcile`, `waybill reconcile`, migration 00013 | The ledger is checked against itself, the chain and Paystack. Each report is stored. The worker runs it every 15 minutes, and CI runs it after the end-to-end tests. |
| Receipts and exports | `internal/pdf`, `internal/documents`, API 0.7.0 | Invoice and receipt PDFs come from a standard-library PDF writer. Receipts are also served as JSON. The CSV export covers the caller's own balances. |
| Conversion | `internal/conversion`, migration 00014, `/account/balance` | A two-minute quote on part of the USDC balance. Accepting it books the conversion and queues a payout through the existing pipeline. |

## How a run of 650.50 USDC is paid

1. **Funding.** The organisation sends test USDC to its funding address. The watcher credits `liability:org:{id}:USDC:pending`, then `…:available` at finality. The sweep moves it into the vault.
2. **Build.** A maker posts the lines. Each line is checked and a problem is written on the line, so a bad line never silently disappears. The total, 650.50, is above the 500 threshold, so the run waits in `awaiting_approval`.
3. **Approve.** The creator's own approval is refused in code (`ErrSelfApproval`). If code were wrong, the `approvals_not_creator` `CHECK` would still refuse it. The second member approves.
4. **Send.** Wallets and the day's vault room are checked again. The total moves from the organisation's available balance to `liability:batch:{id}:USDC:in-flight`, and an outbox job is queued, in one transaction.
5. **Pay.** The job waits until vault custody covers every run in flight. A run sent before its funding was swept used to revert, and this wait came from that finding. The transaction manager then sends `payBatch(token, batchId, items)`.
6. **Final.** Once mined and `Confirmations` deep, the run is `confirming`. At finality the in-flight liability is settled against vault custody, and the run is `paid`.
7. **Prove.** Reconciliation reads the vault's `balanceOf` at one block height and compares it with `custody:vault`. Any money still in flight is listed by record. It then checks the run's `BatchPaid` event against its lines and total.

## Exit check

The roadmap asks that "an automated test runs a batch with dual approval to completion, and the reconciliation report shows zero differences".

| Requirement | Test | Result |
|---|---|---|
| A dual-approval run is refused to its creator, approved by a second member, and paid to two wallets in one transaction | `TestExitCheck_BatchWithDualApprovalCompletes` (Anvil) | pass |
| After that run, reconciliation shows zero differences | `TestExitCheck_ReconciliationAfterDualApprovalRunIsClean` (Anvil): vault 349.5 USDC on chain and in the ledger, 2 records checked | pass |
| The same on the full stack after every end-to-end flow | CI step `make reconcile` after `make e2e` | 0 differences |

It was run on 2026-10-18 at `035474d`, the conversion branch with `main` merged in; that branch merged into `main` as `233adca` with no further code changes. `go test ./...` passed 289 top-level tests with none failed. The 3 skipped are the live tests that need real keys (two Paystack round trips and the live rate source). CI ran the same suite, `make e2e` and `make reconcile` on every stage 4 pull request (#22 to #29).

`make e2e` passed 7 of 7 on the Compose stack. On that same stack, `waybill reconcile --full` read the chain at block 192 and found 0 differences: vault 70.34 USDC and deposits 7.50 USDC on both sides, and 6 on-chain records checked. Paystack was skipped, with no key.

## Found along the way

- **A run could be sent before its funding reached the vault.** On the local stack a run sent straight after funding reverted with `ERC20InsufficientBalance`. The funding was final but not yet swept. Nothing was lost, since the revert path returned the reservation. The sender now waits while vault custody is less than all runs in flight, and `TestBatch_WaitsForFundingToReachTheVault` covers it.
- **The "Paid in full" stamp failed contrast.** The new end-to-end test was the first to run axe on a delivered stamp: light text on green at 90% opacity fell below WCAG AA. Filled stamps now print solid.
- **A wrong assumption in the first reconciliation draft.** The draft listed unfinal deposits as money in flight. A test showed the ledger already books deposit custody at first sighting (as pending). The real race is the other way round: a deposit booked from a block above the height read. The design note was corrected.
- **Unsettled, then settled.** The first CI run of `make reconcile` found a balance off on its first pass and clean on the rerun 20 seconds later. That is the case the two-run rule exists for. A single run would have reported a false difference.
- **PDF validity is checked by an independent reader.** `pdf.Check` verifies every cross-reference offset and stream length. A sample receipt was also rendered with Mozilla pdf.js with `stopAtErrors`, which parsed and drew it correctly.

## Decisions worth defending

- **On-chain batch pay, not naira batches.** One `payBatch` transaction is atomic: the vault's per-item payout-id guard and limits apply, and the whole run pays or none of it does. Naira batches would mean N provider transfers that can each fail separately.
- **Approver ≠ creator in SQL as well as in code.** A composite foreign key gives `approvals` the creator's id, so a plain `CHECK` can compare the two. A bug in the service cannot approve a run for its own creator.
- **Reserve on send, book out at finality.** Reserving stops a second run spending the same money. Booking out of the vault only at finality means a reorg or a revert never needs a correcting entry for money that did not move.
- **Two runs to call a difference.** Bookings lag the chain by design (confirmations, finality). One mismatch is *unsettled*; a mismatch on two runs a finality period apart is a difference. A person is never paged for a race.
- **No float-balance check against Paystack.** In test mode the Paystack balance holds a prefund the ledger never sees, so a balance check would always differ. Each settled payout and paid checkout is checked by reference and amount instead.
- **A standard-library PDF writer.** It needs no dependency and no font file: documents are text and rules in Helvetica. The cost is Latin-1 text only, so amounts read "NGN 1,234.00".
- **Two-minute conversion quotes.** An invoice locks its rate for 48 hours because the payer needs time. A contractor converting money they already hold does not, and a long quote would be a free option against the market.

## Interview questions

1. The approval rule is enforced in the service and again by `approvals_not_creator`. Why can't a plain `CHECK` on `approvals` alone compare approver and creator, and what does the composite foreign key to `batches (id, created_by)` add?
2. A run is reserved on send but booked out of the vault only at finality. Walk through the ledger for a run that reverts. Then say what happens today if a mined run's receipt disappears in a reorg (RISKS), and what it would take to raise `reorged_out` automatically.
3. Why does the sender wait while vault custody is less than *all* runs in flight, not just this run's total? Construct the failure with two runs and one sweep.
4. `BatchPayout.payBatch` pays every item through `Vault.payout`. What would you lose by transferring tokens to each wallet directly from the batch contract?
5. Reconciliation reads balances at one block height and lists money in flight. For each of the three in-flight kinds, say which side (chain or ledger) is ahead and why.
6. Why is a balance difference reported only when it is seen on two runs, while a record difference (a payment amount that doesn't match its log) is reported at once?
7. Why does reconciliation not compare the Paystack balance with `custody:paystack:NGN`, and what would you add in production to cover a float that runs short?
8. The CSV export covers only available balances, so a pending deposit appears when it becomes final. What does that buy an accountant, and what is lost?
9. Conversion quotes last two minutes while invoice quotes last 48 hours. Argue for each length, and explain why accept never re-quotes.
10. Two accepts of the whole balance arrive at once, each with its own quote. Which part of the system stops the second one, and why is the check in `Quote` not enough?
