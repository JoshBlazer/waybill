# Invoices, receipts and the accountant export

**Status:** approved by the owner 2026-10-18 (ADR-044).
**Covers:** the stage 4 roadmap item *Invoices, receipts and accountant export (CSV and PDF)*.

## 1. Documents

| Document | For | When it exists | Shows |
|---|---|---|---|
| Invoice | contractor | always | Who, what, amount, currency, the payment link, the locked rate if naira |
| Receipt for an invoice | contractor | invoice `settled` | What arrived: each on-chain payment with its transaction hash, or the card reference. The rate used, the naira sent and the transfer reference |
| Receipt for a run | org member | run `paid` | Each line: contractor, verified name, wallet, amount, reference. The transaction hash, the approver and the sender |
| Accountant export | both | always | One row per ledger posting on the caller's own accounts, in a date range |

- Each document is built from the ledger and the event tables, never from cached figures. A receipt and the export therefore cannot disagree.
- A receipt asked for before it exists gives `409 not_settled`.

## 2. The CSV export

Columns:

    date, waybill_reference, description, asset, money_in, money_out, balance, chain_tx, provider_reference

- Amounts are decimals in the asset's unit (USDC to 6 places, NGN to 2). Money in and money out are separate columns, so no cell holds a negative number.
- Times are UTC in ISO 8601. The range is inclusive of `from` and exclusive of `to`, and at most 366 days.
- **Formula injection:** any cell that starts with `=`, `+`, `-`, `@`, a tab or a carriage return is prefixed with `'`. Descriptions and run references are typed by people.
- **Whose accounts:** the caller's *available* balances, one per asset, i.e. the money that is theirs.
  - A contractor gets `liability:contractor:{id}:USDC:available` and `…:NGN:available`.
  - A member gets `liability:org:{id}:USDC:available`.
  - A deposit appears when it becomes final. A run's reservation is money out; if the run fails, its return is money in. (Built this way rather than also listing the in-flight accounts: an accountant sees each run once.)
- **Auth:** the one export operation accepts either kind of key and exports for whichever it is. It is the only operation that does (`eitherKeyOperations`).

## 3. PDF — decided: option A (owner, 2026-10-18)

The PDFs are text and ruled lines only: one page for an invoice or a receipt for an invoice, more pages for a long run. The options:

| Option | New dependency | Notes |
|---|---|---|
| **A. Small PDF writer in the Go standard library** *(recommended)* | none | About 250 lines. Uses the built-in Helvetica font, so no font is embedded. Text is limited to Latin-1, so amounts read `NGN 1,234.00`, not `₦`. Tests check the byte offsets in the cross-reference table and that each page's text is present. |
| B. `github.com/signintech/gopdf` | 1 (MIT) | Full text layout. Needs a TTF font in the repo, such as an OFL-licensed one. |
| C. No server PDF | none | A print stylesheet on the web receipt pages; the browser's *Save as PDF*. There is no PDF through the API. |

## 4. Public API (OpenAPI 0.7.0)

| Operation | Key | Returns |
|---|---|---|
| `GET /v1/invoices/{id}/document.pdf` | contractor | the invoice |
| `GET /v1/invoices/{id}/receipt` | contractor | the receipt, as JSON |
| `GET /v1/invoices/{id}/receipt.pdf` | contractor | the receipt |
| `GET /v1/batches/{id}/receipt` | member | the receipt, as JSON |
| `GET /v1/batches/{id}/receipt.pdf` | member | the receipt |
| `GET /v1/exports/ledger.csv?from=&to=` | either | the export, for the caller's own accounts |

- Every document says "Test money only" in its header.
- Only the owning contractor or the owning organisation can fetch a document; anyone else gets `404`.

## 5. Web

- A paid run's page links to its PDF receipt.
- An "Export for your accountant" form, with a date range, appears on `/org` and on the contractor's `/account/bank` page.
- Downloads go through same-origin routes that add the demo key on the server, so the browser never holds a key.
- The web app has no contractor invoice page yet, so invoice documents and receipts are reached through the API only.
- A print stylesheet drops forms and buttons from printed pages.

## 6. Tests

- Every receipt total equals the ledger total for its reference.
- The CSV's balance column matches the ledger balance at the end of the range.
- The injection prefix is applied to each dangerous leading character.
- Another owner's document gives `404`.
- Each PDF is structurally valid: the startxref and cross-reference offsets point at their objects, and the expected text appears.
- The end-to-end test downloads a run receipt.
