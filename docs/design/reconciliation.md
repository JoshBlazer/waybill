# Reconciliation

**Status:** approved by the owner 2026-10-18 (ADR-044).
**Covers:** the stage 4 roadmap item *Reconciliation job: ledger against chain and provider, with a report*, and the reconciliation half of the stage 4 exit check.

## 1. What it answers

"Does every unit of money the ledger says we hold actually exist where the ledger says it is, and does every payment the ledger records match what the chain or the provider says happened?"

A **difference** is anything the job cannot explain. The exit check needs a report with zero differences after a completed dual-approval run.

## 2. Checks

| # | Check | Compares | Kind |
|---|---|---|---|
| R1 | Ledger is internally sound | Per asset, all postings sum to zero; no account breaks its balance rule | ledger only |
| R2 | Vault custody | `custody:vault:{net}:USDC` against `USDC.balanceOf(vault)` | balance |
| R3 | Deposit custody | `custody:deposit:{net}:USDC` against the sum of `balanceOf` over every deposit address on that network | balance |
| R4 | On-chain records | Each `final` payment: its transaction receipt exists and has a `Transfer` of that amount to that address. Each `paid` run: its receipt has status 1 and its `PaidLine` events match the lines | record |
| R5 | Provider records | Each succeeded or failed payout: Paystack `Verify(reference)` gives the same status and amount. Each paid card checkout: `VerifyTransaction(reference)` gives the same amount | record |

### Balance checks and money in flight

The chain and the ledger never agree at every instant: a deposit is booked when it is confirmed, a sweep a few confirmations after it is mined, and a run only at finality. So R2 and R3 read both balances at **one block height**, the head when the check starts, and compute:

    expected = ledger balance + money in flight

**Money in flight** is a list of named records, each shown in the report:

- sweeps mined but not yet booked (their receipt is read from every hash the transaction manager broadcast);
- runs paid on chain but not yet final;
- deposits seen at or below the height read but not yet confirmed (on chain, not yet booked);
- deposits booked from a block above the height read (booked, not in the balance read).

Any amount left after that is **unexplained**. A posting can land a moment after the chain read, so a single unexplained amount is reported as *unsettled*. It becomes a **difference** only if it is still there on the next run, at least one finality period later.

The CLI's `--settle` flag waits for in-flight work to finish and reruns once. That is what the exit-check test uses.

### What is not compared

- **The Paystack balance.** In test mode it holds the developer's prefund, which the ledger never sees. Provider checks are per record (R5).
- **The FX position** (`equity:fx-position:USDC`). It is treasury work, outside this stage (RISKS).

When no Paystack key is configured, R5 is reported as **skipped**, not as passed.

## 3. How it runs

- **Worker job:** runs at start-up and then every 15 minutes. It logs a summary line and stores each report.
- **CLI:** `waybill reconcile [--settle] [--wait 1m] [--full] [--json]` runs once, prints the report, and exits 1 on any difference. `make reconcile` runs it in the stack's worker container, and CI runs it after the end-to-end tests. The exit-check test calls the same code directly.
- **Concurrency:** R4 and R5 check records changed since the last clean run; `--full` checks all of them. Provider calls run at most 4 at a time.

## 4. Schema

One new table. **No ledger schema change.**

```sql
CREATE TABLE reconciliation_runs (
    id          uuid PRIMARY KEY,
    started_at  timestamptz NOT NULL,
    finished_at timestamptz,
    as_of       jsonb NOT NULL,        -- finalized block per network
    differences integer NOT NULL DEFAULT 0 CHECK (differences >= 0),
    report      jsonb NOT NULL         -- every check: status, figures, in-flight list, differences
);
```

## 5. Public API

None. The report is read through the CLI and the stored rows. An operator endpoint can come with stage 5 operations.

## 6. Tests

- **Exit check:** after the dual-approval run completes, `reconcile --settle` reports zero differences.
- **Negative tests:** each one must show exactly one difference.
  - Mint USDC straight to the vault: R2.
  - Post a balanced entry that credits the vault account with no chain movement: R2.
  - Mark a payment final with the wrong amount: R4.
  - Make the fake provider report a different amount: R5.
- **Timing:** a run paid but not yet final is listed as in flight, not as a difference. So is a deposit seen but not yet confirmed. Once confirmed, the ledger holds it as pending, so it needs no entry.
