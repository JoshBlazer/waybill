# Design note: the ledger posting engine

**Status:** schema and tests ready; engine to be hand-written by the project owner.
**Code:** `api/db/migrations/00002_ledger.sql`, `api/internal/ledger/`.
**You implement:** `Entry.Validate`, `Post`, `Reverse` in `api/internal/ledger/post.go`.
**Done means:** `go test ./internal/ledger/` passes (14 schema tests pass already; 18 engine tests fail until the engine exists).

## 1. What the ledger is for

Every movement of value in Waybill is a journal entry: a payment arriving, becoming final, being converted, being paid out, and being reversed by a reorg. Balances are never stored as editable numbers. They are the sum of the entries, so a balance can always be explained line by line.

## 2. The model

- **Account.** A named bucket of one asset, for example `liability:contractor:42:USDC:available` (what we owe contractor 42 in USDC, spendable) or `custody:deposit:evm_84532:USDC` (USDC we hold in deposit addresses on Base Sepolia). Accounts are immutable once created.
- **Entry.** One business event: an idempotency key, a kind (`deposit.confirmed`), a reference to the domain object, and two or more postings.
- **Posting.** One signed amount on one account. **Debit is positive, credit is negative.** All amounts are whole minor units (`money.Amount`).
- **Balance.** The sum of an account's postings, cached in `ledger_balances` by triggers.

Every entry sums to zero **per asset**. A conversion from USDC to NGN is one entry with four postings: two in USDC that cancel, and two in NGN that cancel, linked through `fx:position:*` accounts.

### Balance rules

Each account has a rule on the sign of its balance, enforced by the database on every posting:

| Rule | Meaning | Typical accounts |
|---|---|---|
| `non_negative` | Can never go below zero | Custody (we cannot hold less than nothing) |
| `non_positive` | Can never go above zero | Contractor balances (we cannot owe less than nothing, so nobody is paid more than they are owed) |
| `any` | Unrestricted | Clearing, FX position, test accounts |

The overdraft check is a `CHECK` constraint on the balance cache, so a bug in Go cannot pay someone more than they are owed.

## 3. Who enforces what

The database is the last line. The engine is the first: it exists to give **precise errors early** and to keep concurrent writers from deadlocking.

| Rule | Database | Engine |
|---|---|---|
| Whole minor units, no rounding | `minor_units` domain (`scale(VALUE) = 0`) | `money.Amount` is an integer |
| ≥ 2 postings per entry | deferred trigger `ledger_entry_min_postings` | `ErrTooFewPostings` |
| Non-zero amounts | `CHECK (amount <> 0)` | `ErrZeroAmount` |
| Sum to zero per asset | deferred trigger `ledger_entry_balanced` | `ErrUnbalanced` |
| Posting asset = account asset | composite FK `postings_asset_matches_account` | `ErrAssetMismatch` |
| Account exists | FK | `ErrUnknownAccount` |
| No overdraft | `CHECK ledger_balance_rule` | maps it to `ErrInsufficientFunds` |
| Append-only | triggers reject UPDATE, DELETE, TRUNCATE | never issues them |
| Postings only in the entry's own transaction | trigger `postings_same_transaction` | — |
| Balance cache = sum of postings | trigger-maintained; direct writes rejected | — |
| Reversal exactly mirrors original; at most once; never a reversal of a reversal | deferred trigger, `UNIQUE (reverses_entry_id)` | `ErrAlreadyReversed`, `ErrCannotReverseReversal` |
| Idempotency key unique | `UNIQUE (idempotency_key)` | retry returns original, or `ErrIdempotencyConflict` |
| One account at most once per entry | — (would need a deferred rule check) | `ErrDuplicateAccount` |

Why reject duplicate accounts? The balance-rule `CHECK` runs on each posting as it is inserted. An entry with `-10` then `+10` on the same `non_negative` account holding `5` would fail halfway, even though the entry's net effect is zero. Requiring each account at most once per entry makes the per-posting check exact, and it is never a real limitation: net the amounts before posting.

## 4. `Entry.Validate`: pure checks

No database. Check in this order and return the first failure, so the errors in the tests are predictable:

1. idempotency key non-empty → `ErrEmptyIdempotencyKey`
2. kind non-empty → `ErrEmptyKind`
3. at least two postings → `ErrTooFewPostings`
4. every amount non-zero → `ErrZeroAmount`
5. no account twice → `ErrDuplicateAccount`
6. per-asset sums all zero → `ErrUnbalanced`, wrapped with the asset and its sum. If more than one asset is off, report them in a deterministic order (for example, sorted by code).

Wrap with `fmt.Errorf("%w: …", ErrX, …)` so `errors.Is` still matches.

## 5. `Post`: steps

`Post` runs inside the **caller's** transaction. The caller writes the domain change (say, the payment's new state) and the ledger entry together, and commits once. If either fails, neither happens.

1. **Validate.** Return its error and write nothing.
2. **Idempotency lookup.** Look up the key (`GetJournalEntryByIdempotencyKey`). If it exists, compare the stored entry with the one requested: same kind, same references, and the same postings as a multiset of `(account, asset, amount)`, ignoring order. If they are the same, return the existing id: this is a retry. If not, return `ErrIdempotencyConflict`: the key was reused for something else, which is a bug upstream.
3. **Load accounts** (`GetAccountsByIDs`). Any id missing → `ErrUnknownAccount`. Any posting whose asset differs from its account's → `ErrAssetMismatch`. Checking here gives a clear error instead of a foreign-key violation.
4. **Insert the entry** (`InsertJournalEntry`).
5. **Insert the postings in ascending account id order** (`InsertPosting`). Each insert fires a trigger that updates that account's `ledger_balances` row, which takes a row lock. If two transactions touch accounts A and B in opposite orders, each holds one lock and waits for the other, and PostgreSQL kills one with a deadlock error (`40P01`). Sorting by account id gives every transaction the same lock order, so this cannot happen. `TestPost_ConcurrentOpposingPostsDoNotDeadlock` proves it.
6. **Map database errors.** A `*pgconn.PgError` with `ConstraintName == "ledger_balance_rule"` becomes `ErrInsufficientFunds`. Return everything else wrapped.

The balance and minimum-postings checks are deferred to `COMMIT`. If Go code is wrong, the caller's commit fails. That is the backstop.

### Questions worth thinking through before coding

- Two requests with the same key race: both miss in step 2 and both insert. One hits the unique constraint on `idempotency_key` and its transaction is aborted. What should the caller see, and what should it do next? (Hint: retrying the whole transaction reaches step 2 and finds the winner.)
- Why compare postings as a multiset and not as an ordered list?
- What changes if `ledger_balances` were updated by Go code instead of a trigger?

## 6. `Reverse`: steps

A reversal undoes an entry by posting its exact negation, linked through `reverses_entry_id`. History is never edited.

1. Load the entry. If it is missing → `ErrEntryNotFound`.
2. If it is itself a reversal → `ErrCannotReverseReversal`. To re-apply, post a new entry.
3. If it has already been reversed (`GetReversalOf`): same idempotency key → return that reversal's id (a retry); different key → `ErrAlreadyReversed`.
4. Build the negated postings, with kind `<original kind>.reversal` and the same references, and post them with `reverses_entry_id` set. That is the same path as `Post`, with the same idempotency, ordering and error mapping.

`ErrInsufficientFunds` from a reversal is the important real case: a deposit is credited, the contractor is paid, and then a reorg removes the deposit. The ledger refuses to make custody negative. The caller must escalate this to a human and must never retry it automatically. That is the "pull the plug" scenario Waybill exists to handle visibly.

## 7. The tests that define "done"

| Test | What it pins down |
|---|---|
| `TestValidate_AcceptsBalancedEntries`, `TestValidate_RejectsEachRule`, `TestValidate_UnbalancedErrorNamesAssetAndSum` | §4, rule by rule |
| `TestValidate_PropertyBalancedAccepted`, `TestValidate_PropertyAnyPerturbationRejected` | `rapid`: every balanced entry passes; changing any one amount fails |
| `TestPost_RecordsEntryAndUpdatesBalances`, `TestPost_InvalidEntryWritesNothing` | the basic path |
| `TestPost_RetryWithSameKeyReturnsOriginal`, `TestPost_SameKeyDifferentEntryConflicts` | §5 step 2 |
| `TestPost_UnknownAccount`, `TestPost_AssetMismatch`, `TestPost_InsufficientFunds` | §5 steps 3 and 6 |
| `TestPost_ConcurrentOpposingPostsDoNotDeadlock` | §5 step 5 |
| `TestReverse_*` | §6 |
| `TestLedger_TrialBalanceAlwaysZero` | invariant I5: random Posts and Reverses keep every trial balance at zero and the cache exact |

The suite was checked against a throwaway implementation, which was then discarded. Every test above passes for a straightforward, correct engine, and the deadlock test fails (27 × `40P01` in 3 runs) if postings are not sorted. The tests are satisfiable, and they bite.

Run them with `cd api && go test ./internal/ledger/` (Docker required). Use `-run TestValidate` for the fast, pure subset while you work.
