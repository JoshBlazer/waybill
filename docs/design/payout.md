# Design note: the payout state machine

**Status:** designed. The signature and failing tests are in place; the owner implements `PayoutTransition` (hand-written module, see CLAUDE.md).
**Code:** `api/internal/statemachine/payout.go` (types and signature), tests in `payout_test.go`.
**Done means:** `go test ./internal/statemachine/` passes, including the 4 payout tests.

## 1. What a payout is

When an invoice settles and the contractor chose *naira to my bank*, a payout is created. The payout converts the stablecoin to naira at the rate locked when the invoice was created, then asks the provider (Paystack in test mode) to transfer the naira to the contractor's verified bank account. A contractor who chose *hold* gets a payout that goes straight to `held`.

The state machine is the part that decides what may happen next. Everything that talks to the ledger or the provider is plumbing around it, and the machine itself is pure: no I/O and no clock, enforced by `TestStatemachine_NoIOImports`.

## 2. The table

From `docs/ARCHITECTURE.md` §5.2. Every pair not listed is illegal.

| From | Event | To | Why |
|---|---|---|---|
| `queued` | `converted` | `converted` | The conversion entry is in the ledger at the locked rate. |
| `queued` | `hold_requested` | `held` | The contractor keeps the stablecoin. Terminal. |
| `queued`, `converted` | `cancel` | `cancelled` | Withdrawn before anything was sent. Terminal. |
| `converted` | `submit` | `submitting` | Recorded **before** the provider call, in its own transaction. |
| `submitting` | `provider_accepted` | `submitted` | The provider has the transfer. |
| `submitting` | `provider_rejected` | `failed` | The provider refused the request outright: nothing was sent. |
| `submitting` | `provider_unknown` | `unknown` | Timeout, 5xx or a dropped connection. We do not know. |
| `unknown` | `reconciled_accepted` | `submitted` | Asked by reference: the provider has it. |
| `unknown` | `reconciled_not_found` | `converted` | Asked by reference: it has nothing. Safe to submit again **with the same reference**. |
| `submitted` | `provider_succeeded` | `succeeded` | The bank credited the account. |
| `submitted` | `provider_failed` | `failed` | Accepted, then failed at the bank. |
| `succeeded` | `provider_reversed` | `reversed` | Rare: the bank reversed it. Terminal; a person follows up. |
| `failed` | `retry_approved` | `converted` | A person fixed the cause (for example bank details) and approved a retry. |

`PayoutTransition(from, ev)` returns the next state, or `ErrIllegalTransition` (wrapped; callers use `errors.Is`) with `from` unchanged. As with invoices, callers treat an illegal transition as a bug or an alert, never as something to retry.

## 3. The rules the table encodes

- **"I couldn't find out" is its own state (I12).** A timeout is `unknown`, never `failed`. `failed` invites a retry, and a retry of a transfer that did go through pays twice. The only way out of `unknown` is reconciliation: asking the provider about the transfer by its reference.
- **The reference is the payout id, every time.** The provider rejects a second transfer with a reference it has seen, so even a mistaken resubmission cannot pay twice. `reconciled_not_found → converted` is safe for that reason: the next submission carries the same reference.
- **No cancelling with an accepted transfer outstanding.** `cancel` is legal only from `queued` and `converted`. In those states nothing is with the provider, either because nothing was sent yet or because reconciliation or a failure showed that nothing went through.
- **`succeeded` is not terminal.** Reversals happen. `reversed` is terminal and a person handles it.

## 4. What the caller does around each transition (plumbing, not this module)

- `converted`: post the conversion entry in the same transaction (stablecoin out of the contractor's balance, naira into a clearing account, at the quote's exact numbers).
- `submitting`: commit the state **before** calling the provider. A crash after the call then finds `submitting`, treats it like `unknown`, and reconciles.
- `succeeded`: post the entry that moves naira from clearing to "paid out" against the provider's float.
- Every transition writes a `payout_events` row and notifies the tracking page, as invoice events do.

## 5. Questions worth thinking through before coding

1. Why is `submitting` a state at all, rather than going straight from `converted` to `submitted` after the HTTP call returns?
2. A webhook says `transfer.success` while the payout is still `submitting`, because the HTTP response was lost. Which events should the caller apply, and in what order?
3. Why does `reconciled_not_found` go to `converted` and not to `failed`?
4. What would break if `failed → converted` did not need a person's approval?
5. The table has 14 legal pairs out of 130. How would you make the illegal 116 impossible to forget when a new event is added?

## 6. The tests that define "done"

| Test | What it pins |
|---|---|
| `TestPayout_AllPairs` (**I7**) | all 130 pairs: the 14 legal ones go where the table says; the 116 illegal ones return `ErrIllegalTransition` and leave the state unchanged |
| `TestPayout_ProviderTimeoutGoesUnknownNotFailed` (**I12**) | a timeout is `unknown`; only reconciliation moves an unknown payout, never to `failed` or `submitting` |
| `TestPayout_TerminalStates` | exactly `held`, `reversed` and `cancelled` are terminal |
| `TestPayout_RandomWalksNeverPayWithoutTheProvider` (`rapid`) | over random event sequences: `succeeded` only after an acceptance and a provider success; never cancelled while an acceptance stands; illegal events change nothing |
