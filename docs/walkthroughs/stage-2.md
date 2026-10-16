# Stage 2 walkthrough — Hardening

Stage 1 moved money in the happy case. Stage 2 makes each way it can go wrong a tested, recorded path: chains that change their minds, payers who pay the wrong amount, transactions that get stuck or would revert, workers that crash, and contractors' servers that fail.

## What was built

| Piece | Where | Why |
|---|---|---|
| Reorg handling | `api/internal/watcher`, migration 00004 | The watcher remembers the hash of the last block of every scanned range. Each tick it compares that, and the block of every open payment, with the node. On a mismatch it walks back to the common ancestor and, in one transaction, marks replaced payments `reorged`, reverses their ledger entries, re-derives each invoice from what is left and rewinds the cursor (I8). |
| Payment edge cases | `watcher.confirm`/`finalize`, `statemachine`, `invoice.ExpireDue` | The confirmed total is classified only when no payment is still unconfirmed, so a payment in parts is not called short. Paying twice ends `overpaid` with everything credited. Invoices now expire, and late payments are accepted. |
| Outbox and worker | `api/internal/outbox`, `cmd/waybill/worker.go`, ADR-037 | Background work is written in the same transaction as the change that causes it, and leased with `FOR UPDATE SKIP LOCKED`. It is retried with backoff (per-topic policies) and goes `dead` for a person after the last attempt. |
| API keys and webhooks | `api/internal/httpapi`, `api/internal/webhook`, ADR-038 | Contractors manage their own keys. Invoice events are emitted in the state-change transaction, signed `t=…,v1=HMAC`, retried for about a day, and every attempt is logged (I13). An SSRF guard checks the URL at registration and the resolved address when dialing. |
| Nonces and fees | `api/internal/txmgr` (`nonce.go`, `fees.go`, `lease.go`) | One writer per key under a database-clock lease. Nonces are the larger of the stored and the node's count, assigned under a row lock (I10). Replacement fees are the smallest a node accepts, both raised (I11). |
| Sending transactions | `txmgr.Manager`, migration 00008 | Gas is estimated before a nonce is assigned. A revert is recorded and nothing is sent; an unreachable node records nothing (I12). Every hash is recorded **before** it is sent. Stuck attempts are replaced, and receipts finish the row and run the purpose's hook in the same transaction. |
| Sweeping | `api/internal/sweep` | Final deposits move into the vault through `deployAndSweep`, never while another payment to the invoice is unconfirmed. The ledger moves deposit custody to vault custody by the amount in the on-chain `Swept` event. |
| `BatchPayout` | `contracts/src/BatchPayout.sol` | Pays up to 200 recipients in one transaction, each through `Vault.payout`, so the payout-id guard, limits and pause apply per item. A batch pays everything or nothing (I16). |
| Real contracts in Go tests | `api/internal/testchain` | Anvil in testcontainers, and the compiled contracts from `contracts.json` (`make fixtures`, checked in CI), so sweeping is tested against the real factory and vault. |

## How a final deposit reaches the vault

1. **Final.** The watcher makes a payment final once the chain's `finalized` block passes it (a depth on Anvil). The ledger moves the amount from the contractor's pending balance to available.
2. **Job.** Every 15 s the worker adds a `sweep.deposit` outbox job for each final payment without one. The dedupe key `sweep:<payment id>` means each payment gets one job, however many workers run.
3. **Wait if needed.** If any payment to the invoice is still detected or confirmed, the job retries later. Sweeping moves the forwarder's whole balance, and the ledger has not counted those payments yet.
4. **Submit.** The transaction manager estimates `deployAndSweep(keccak256(invoice id), token)`. If it would revert, for example because the signer lacks `SWEEPER_ROLE`, the row is recorded `failed` with the reason, no nonce is used, nothing is sent, and the job goes dead. Otherwise it takes the key's lease and runs `NextNonce` in the same transaction as the new row. It then signs, records the hash, and sends.
5. **Watch.** The manager's tick checks every recorded hash for a receipt. If none appears within `StuckAfter`, it sends a replacement at the same nonce with both fees raised, stopping at the fee cap.
6. **Record.** Once the receipt is `Confirmations` deep, the row becomes `mined`. In the same database transaction the sweep hook reads the factory's `Swept` event and posts `sweep.confirmed`: `custody:vault` up and `custody:deposit` down by exactly what moved. What the contractor is owed does not change.

## Exit check

The roadmap asks that for each failure listed in stage 2, a test forces it and passes, and that the command runs in CI. It was run on 2026-10-16 on `main` at `4b47fce`. CI runs the same commands on every pull request; the Go job runs `go test ./...` without `-short`, so the Docker-backed Anvil tests run there too.

| Failure | Test that forces it | Result |
|---|---|---|
| A reorg removes a confirmed payment, and the same transaction comes back | `TestWatcher_ReorgReversesConfirmedPayment` (Anvil `evm_snapshot`/`evm_revert`) | pass |
| Two senders want nonces for one key at once | `TestTxMgr_ConcurrentSendersNeverShareNonce` | pass |
| A transaction is stuck under the base fee | `TestTxMgr_StuckTransactionIsReplaced` (Anvil, mining paused, base-fee spike); `TestTxMgr_ReplacementBumpsBothFees` (`rapid`) | pass |
| Submit and the tick loop race on a new transaction | `TestTxMgr_SubmitAndTickDoNotDoubleBroadcast` | pass |
| Gas estimation says the call reverts | `TestTxMgr_RevertingCallIsRecordedNotSent`; `TestSweep_WithoutSweeperRoleIsRecordedNotSent` (real factory) | pass |
| Gas estimation cannot reach the node | `TestTxMgr_UnreachableNodeIsNotAFailure` | pass |
| Underpaid, then topped up; paid in two parts | `TestWatcher_UnderpaidThenTopUp`, `TestWatcher_TwoPartPaymentIsConfirmedNotStuckUnderpaid` | pass |
| Overpaid; paid twice | `TestWatcher_OverpaymentIsCreditedInFull`, `TestWatcher_PayerPaysTwiceIsOverpaid` | pass |
| Paid after expiry | `TestInvoice_LatePaymentAccepted` | pass |
| The same log seen twice | `TestWatcher_DuplicateLogIsNoop` | pass |
| Sweeping while a payment is unconfirmed | `TestSweep_WaitsWhileAPaymentIsUnconfirmed` | pass |
| Over the vault limits; vault paused | `test_RevertWhen_OverTransactionLimit`, `test_RevertWhen_OverDailyLimit`, `test_PauseStopsPayoutsAndOnlyAdminUnpauses`, `test_RevertWhen_VaultPaused` | pass |
| A payout id or batch reused | `test_RevertWhen_PayoutIdReusedAcrossBatches`, `test_RevertWhen_BatchResubmitted`; `invariant_PayoutIdPaidAtMostOnce` (256 × 100 calls) | pass |
| A worker crashes holding a job; many workers at once | `TestOutbox_CrashedWorkersJobIsTakenOverAfterLease`, `TestOutbox_ConcurrentWorkersRunEachJobOnce` | pass |
| A webhook endpoint fails, or points into Waybill's network | `TestWebhook_RetriesWithBackoff`, `TestWebhook_PrivateAddressAtDeliveryIsRefused` | pass |
| A key leaks into logs | `TestSecret_NeverAppearsInLogs`, `TestLoadFrom_SignerKeyNeverPrints` | pass |

In total, `go test ./...` passed 184 top-level tests with none failed or skipped. `FOUNDRY_PROFILE=ci forge test` passed 36 tests (fuzz at 2,000 runs; invariants at 256 runs × 100 calls). `make e2e` passed 4 of 4, and on the Compose stack every paid invoice was swept.

**Live on Base Sepolia.** `BatchPayout` was added to the existing deployment and verified on-chain. A 30 USDC invoice was paid. About 20 minutes later, when Base Sepolia's `finalized` block passed the payment, the worker swept it in [`0x6f698e90…3b196`](https://sepolia.basescan.org/tx/0x6f698e90cc12e3fd621c732fd9fa282f0463bc59700102a1ee08fef3ace3b196). The forwarder was deployed and emptied in one transaction, and the ledger moved 30 USDC from deposit custody to vault custody. The nonce was 14, because the key had been used outside Waybill, and `NextNonce` started from the node's count.

## Found along the way

- **Payments in parts got stuck.** The first confirmation classified the invoice as underpaid, and `underpaid` had no transition for the second part's confirmation. A test was written first and failed on the old code.
- **Double broadcast within one process.** On the Compose stack the worker logged duplicate-hash errors during sweeps. `Tick` could find a row that `Submit` had just committed, before its first attempt existed. With moving fees that would have sent two competing transactions at one nonce. A test reproduced it in 3 of 3 runs; a mutex fixed it (5 of 5).
- **A signing key in JSON logs.** `TestLoadFrom_SignerKeyNeverPrints` showed that logging a whole `Config` with `slog`'s JSON handler wrote the key. `LogValue` covers top-level attributes only, and `encoding/json` reads nested fields directly. `config.Secret` now also redacts itself in `MarshalJSON` and `MarshalText`.
- **A forge simulation wrote the deployments file.** Script code runs during the simulation too. `AddBatchPayout.s.sol` now writes the file only in `ScriptBroadcast` context; otherwise the real run would have refused, seeing a simulated address.
- **Fixed rounds against an asynchronous Anvil.** Automined blocks are sealed asynchronously, so tests that mined a fixed number of blocks were flaky in CI. They now mine and tick until the transaction finishes.

## Decisions worth defending

- **Store one hash per scanned range, not per block.** Each block commits to its parent, so a replaced block anywhere below changes the newest stored hash. Storing every block on Base Sepolia would cost an RPC call per block (two seconds each) for no extra information. The trade-off is that the walk-back can land a range early. That is harmless: only payments whose own block changed are reorged, and rescanning is idempotent.
- **A reorg returns the invoice to `open`, then replays.** The invoice re-derives its state from the payments still on the chain, rather than the state machine guessing which one vanished.
- **Estimate before the nonce.** A transaction that would revert never takes a nonce, so it cannot leave a gap that blocks every later transaction from the key.
- **Record the hash before sending.** A crash between sending and recording would leave a transaction nobody watches. The other order costs at most a recorded hash that was never sent.
- **Ledger from the event, not the request.** The sweep posts what the factory's `Swept` event says moved, not what Waybill expected to move.
- **Webhook retries can't replay secrets.** A create retried with the same `Idempotency-Key` returns 409 naming the resource. Replaying would mean storing a secret that was promised to be shown once.

## Interview questions

1. The watcher stores the hash of only the last block of each scanned range. Why is that enough to detect a reorg anywhere in the range, and what does the walk-back give up compared with storing every block?
2. A payment's transaction is dropped by a reorg and included again in a later block with the same hash and log index. Walk through what happens to the `payments` rows, the ledger and the invoice, and explain why the unique index excludes `reorged` rows.
3. Why does the watcher refuse to classify an invoice's confirmed total while another payment is still `detected`? Construct the sequence that got stuck before the fix.
4. `NextNonce` returns `max(stored, chainPending)`. Give one real situation for each side being larger, and what goes wrong if you always trust the other side.
5. Why is gas estimated before a nonce is assigned, and why does an estimation *timeout* record nothing while a *revert* records a failed row?
6. Replacement fees are `max(⌈x·(100+b)/100⌉, x+1)` for both caps. Why both caps, why round up, and why the `x + 1`?
7. The transaction manager records each attempt's hash before sending it. What failure does that order protect against, and what does the opposite order risk?
8. The lease kept other processes out, yet one worker could still broadcast the same transaction twice. Explain the race and why a mutex, not the lease, is the right fix.
9. A sweep moves a forwarder's whole balance. Why must it wait for every payment to the invoice to be confirmed, and why does the ledger entry come from the `Swept` event rather than from the payments?
10. A webhook URL passes the registration check but later resolves to `169.254.169.254`. Which part of the design stops the request, and why is checking the URL at registration alone not enough?
