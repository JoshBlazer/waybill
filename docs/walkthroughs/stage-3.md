# Stage 3 walkthrough — Naira settlement

Stage 2 paid a contractor in a stablecoin. Stage 3 lets the contractor be paid in naira to a Nigerian bank account instead, at a rate locked when the invoice is made. It also lets a payer without crypto pay the link by card or bank transfer. Paystack in test mode is the provider for both directions.

## What was built

| Piece | Where | Why |
|---|---|---|
| Rates | `internal/rates`, ADR-039 | ExchangeRate-API's open USD→NGN rate, parsed as decimal text into exact kobo per USDC (no floats), with a spread. A quote lasts 48 hours; a rate older than 26 hours is refused. With no rate, a naira invoice is refused (503) and *hold* still works. |
| Bank-name verification | `internal/bankverify`, ADR-040 | Paystack's account resolution returns the bank's name for the account. Every part of the contractor's legal name must appear in it. A failed lookup is "could not verify", never "verified". |
| Bank accounts | `PUT /v1/me/bank-account`, `/account/bank` | Saved only after the names match. Payouts to a changed account wait 24 hours, against account takeover. The number is shown only as its last four digits. |
| Payout state machine | `internal/statemachine/payout.go`, [design](../design/payout.md) | Pure. All 130 (state, event) pairs are tested, and 14 are legal. A timeout is `unknown`, never `failed`. The only way out of `unknown` is asking the provider by reference. |
| Payouts | `internal/payout`, `internal/paystack` | One per settled naira invoice, converted at the locked rate (re-quoted for a late payment). `submitting` is committed before the provider call. The reference is the payout id every time, so Paystack itself refuses a second transfer. |
| Webhooks | `internal/webhook`, `POST /v1/providers/paystack/webhook` | The signature is checked before the body is read, the raw event is stored, and each event id is processed once. They only make things faster: every payout and checkout is confirmed by asking Paystack by reference, because the local stack has no public URL. |
| Card and bank pay-in | `internal/checkout`, ADR-042 | A Paystack test checkout at the locked naira amount. It is confirmed only by verifying the reference, then settled in one transaction. The payment page offers it only when `checkout.Service.Payable` says the link can be paid in naira right now. |
| Underpayment | `POST /v1/invoices/{id}/accept-shortfall` | An underpaid invoice waits for a top-up, or the contractor accepts what arrived and it settles at that amount. |

## How a ₦155,000 payout is made

1. **Create.** The contractor asks for 100 USDC paid out in naira. The invoice locks a rate for 48 hours (₦1,550 per USDC in the exit-check test), and the tracking page shows it.
2. **Pay.** The payer pays the link on-chain. The watcher takes the invoice to `settled` at finality, as in stage 2.
3. **Convert.** The worker creates the payout and moves it `queued → converted`. In the same transaction the ledger moves 100 USDC out of the contractor's balance and ₦155,000.00 into NGN clearing, at the quote's exact numbers.
4. **Submit.** `converted → submitting` is committed on its own. Only then does the worker call Paystack, with the payout id as the reference.
5. **Answer.** An acceptance moves it to `submitted`; an outright refusal to `failed`; a timeout or a 5xx to `unknown`. From `unknown`, the worker asks Paystack about the reference. "It has it" becomes `submitted`; "nothing" goes back to `converted`, to be sent again with the same reference.
6. **Done.** Paystack reports success, the payout is `succeeded`, and the naira moves from clearing to paid out against the provider float. Every step on the tracking page is done.

## Exit check

The roadmap asks that "a link is paid and a test-mode naira payout completes end to end, proven by an automated test against Paystack test mode".

| Requirement | Test | Result |
|---|---|---|
| A link is paid and a naira payout completes, with every step real except the provider | `TestExitCheck_LinkPaidAndNairaPayoutCompletes` (Anvil, fake Paystack), in every CI run | pass: ₦155,000.00 `succeeded`, tracking steps done, trial balance zero |
| Paystack test mode: resolve a test account and create a recipient | `TestLive_TransferRoundTrip` with Zenith test account `057 / 0000000000` | pass (resolved as "Test") |
| Paystack test mode: a checkout started and verified by reference | `TestLive_CheckoutStartAndVerify` | pass (unpaid, reported `abandoned`) |
| Paystack test mode: the transfer itself | `TestExitCheck_LinkPaidAndNairaPayoutCompletes` with the test key | **refused by Paystack** |

**Result (2026-10-19): not yet passed.** Paystack answered the transfer with HTTP 400, "You cannot initiate third party payouts as a starter business". This restriction is on the Paystack business account, not in Waybill's code, and it applies in test mode too. A different bank account would not get past it. Waybill handled the refusal as designed: the payout went to `failed` (an outright refusal), not `unknown`, and nothing was sent. The check passes once the Paystack business is upgraded past "starter" in the dashboard and the same test is run again:

```bash
set -a; . deploy/.env; set +a
cd api && PAYSTACK_TEST_BANK_CODE=057 go test ./internal/payout -run TestExitCheck -v
```

## Found along the way

- **Test bank accounts are not interchangeable.** Bank code `001` resolved, but recipient creation on it failed. Real banks allow only 3 resolves a day in test mode. Paystack's documented Zenith test account, `057 / 0000000000`, resolves and creates a recipient.
- **Webhooks could not be the source of truth.** Paystack sends them only to a public HTTPS URL, and the local stack has none. Confirming by reference was planned as the backup; it became the main path, and webhooks only speed it up.
- **Checkout needs a payer email.** Payers have no accounts, so checkouts send `pay-<tracking code>@example.com`. A live deployment would ask the payer.

## Decisions worth defending

- **`unknown` is a state.** A timeout means "I don't know", and treating it as `failed` invites a retry that can pay twice. Only the provider, asked by reference, can say what happened.
- **The payout id is the reference.** A resubmission after `reconciled_not_found` carries the same reference, so even a mistaken second send is refused by Paystack.
- **The rate is locked when the invoice is made.** The contractor knows the naira amount before the payer pays. A late payment is re-quoted rather than paid at a stale rate.
- **Exact kobo, never floats.** The rate is parsed from the JSON text into an integer number of kobo per USDC, so a conversion is exact and the ledger always balances.
- **Names must match, and a changed account waits a day.** These two checks block the common takeover: an attacker changes the bank account just before a payout.
- **ExchangeRate-API over a crypto price feed.** It needs no key, and a daily mid rate is honest for test money. Treating USDC as exactly 1 USD is a recorded risk.

## Interview questions

1. Why is `submitting` committed in its own transaction before the Paystack call? Describe the crash that would pay twice without it.
2. A transfer times out and the payout is `unknown`. Reconciliation asks Paystack and gets "not found", but the transfer actually arrives a second later. What stops a double payment?
3. A `transfer.success` webhook arrives while the payout is still `submitting`, because the HTTP response was lost. Which transitions does the worker apply, and in what order?
4. Why does `reconciled_not_found` go to `converted` and not `failed`? What would a person do with a `failed` payout that the machine cannot?
5. The table has 14 legal pairs out of 130. How do the tests make it impossible to add an event and forget a pair?
6. The rate is parsed into kobo per USDC as an integer. Show a rate and an amount where float64 would give the wrong kobo, and say what the ledger would do with that.
7. A contractor changes their bank account an hour before a large payout. Walk through what happens, and why 24 hours rather than none or a week.
8. The name check requires every part of the legal name to appear in the bank's name, in any order, with no fuzzy matching. Give a false rejection and a false acceptance. Which is worse here?
9. Paystack refused the transfer for a "starter business". Should that be `failed` or `unknown`, and what would change if Paystack had answered with a 500 instead?
10. Card pay-in is confirmed only by verifying the reference, never by the redirect back to the payment page. What attack does that stop?
