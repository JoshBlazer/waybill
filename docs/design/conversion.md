# Contractor balance and conversion

**Status:** approved by the owner 2026-10-18 (ADR-044).
**Covers:** the stage 4 roadmap item *Contractor balance and conversion at a quoted rate*.

## 1. The journey

1. A contractor holds a USDC balance. It comes from USDC invoices and from batch-run lines paid to the contractor. Batch lines go to the wallet, so they show in the history but not in the balance.
2. The contractor asks to convert an amount to naira. Waybill quotes a rate and the naira amount, valid until `expiresAt`. This uses the existing rate source and spread. A conversion quote lasts **two minutes** (`conversion.QuoteTTL`), not the 48 hours an invoice locks its rate for: a long quote on money already held would give the contractor a free option against the market.
3. The contractor accepts before expiry. Waybill books the conversion and queues a naira payout to their verified bank account. The payout goes through the existing pipeline, including the 24-hour bank-account wait.
4. After expiry the accept is refused with `409 quote_expired`, and the contractor asks again. Waybill **never** re-quotes silently on accept.

## 2. Rules

- The amount must be at least 1 USDC, and no more than what is available.
  - The check runs inside the accept transaction.
  - The ledger's `ErrInsufficientFunds` is the final guard, so two accepts at once can't overdraw.
- A verified bank account must exist when the contractor asks for a quote.
- Accept takes an `Idempotency-Key`. A repeated accept returns the same conversion and books nothing again; so does accepting an accepted conversion with a new key. The web app uses `accept-{id}` as the key, so a retry after a lost answer is safe.
- A quote belongs to the contractor who asked for it. Another contractor's quote id gives `404`.

## 3. Ledger

There are no new account kinds and no ledger schema change. The accept posts the same four-leg entry as an invoice payout's conversion step (`postConversion`):

| Account | Leg |
|---|---|
| `liability:contractor:{id}:USDC:available` | debit the amount |
| `equity:fx-position:USDC` | credit the amount |
| `custody:paystack:NGN` | debit the naira |
| `liability:contractor:{id}:NGN:available` | credit the naira |

The payout then moves the naira out exactly as it does today.

## 4. Schema (not the ledger tables)

```sql
CREATE TABLE conversions (
    id            uuid PRIMARY KEY,
    contractor_id uuid NOT NULL REFERENCES contractors (id),
    usdc_amount   minor_units NOT NULL CHECK (usdc_amount >= 1000000),
    naira_amount  minor_units NOT NULL CHECK (naira_amount > 0),
    rate_quote_id uuid NOT NULL REFERENCES rate_quotes (id),
    state         text NOT NULL CHECK (state IN ('quoted', 'accepted')),
    accepted_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
```

The payout tables change so that a payout comes from either an invoice or a conversion:

- `payouts.invoice_id` becomes nullable.
- `payouts` gains `conversion_id uuid UNIQUE REFERENCES conversions (id)`.
- A `CHECK` requires exactly one of `invoice_id` and `conversion_id`.
- `payout_events.invoice_id` becomes nullable. The notify trigger only notifies when it is set, because conversion payouts have no public tracking page.

A conversion payout is inserted directly in `converted`, with its naira set, so `PayoutTransition` needs no change.

*Built:* `expired` is not stored. A `quoted` conversion past its quote's `expiresAt` reads as `expired`, and accept checks the time itself, so nothing has to write the state down when a quote lapses.

## 5. Public API (OpenAPI 0.7.0, contractor key)

| Operation | |
|---|---|
| `GET /v1/me/balance` | USDC available and pending, NGN on its way out, and recent conversions |
| `POST /v1/me/conversions` | Body `{amount}`. Returns the quote: `{id, amount, naira, rate, expiresAt}` |
| `POST /v1/me/conversions/{id}/accept` | Books the conversion and returns it with its payout's state |
| `GET /v1/me/conversions/{id}` | One conversion and its payout's state |

## 6. Web

`/account/balance` shows the balance, a convert form, the quote with a visible countdown, and the conversion history. When the quote lapses, the accept button is replaced by "Get a new rate". The page is linked from `/account/bank`.

## 7. Tests

- Accepting before expiry books one entry and queues one payout. Accepting after expiry is refused and books nothing.
- Concurrent accepts of the whole balance: exactly one succeeds.
- A repeated `Idempotency-Key` returns the same result.
- Another contractor's quote gives `404`.
- The payout completes against the fake provider, and the reconciliation report stays at zero differences.
