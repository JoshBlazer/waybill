# Waybill — Product

Waybill lets small web3 teams pay their Nigerian contractors in stablecoins, and lets those contractors receive naira in their bank account, with every payment trackable like a parcel.

Waybill runs on test networks and provider sandboxes only. It never touches real funds (see [ARCHITECTURE.md § Test-money guard](ARCHITECTURE.md#test-money-guard)).

## 1. Users

| User | Who they are | What they want |
|---|---|---|
| **Payer** | A founder or operations person at a small crypto-native team (2–30 people) that already pays contractors in USDC or USDT. | To pay the right person, the right amount, once, and to have paperwork an accountant accepts. |
| **Contractor** | A Nigerian freelancer or remote worker paid in stablecoins. | To get naira in their bank at a rate they knew in advance, without peer-to-peer trading, and to see where their money is at every moment. |
| **Approver** | A second person on the payer's team. | To check and approve large payment runs before money moves. |
| **Accountant** (indirect) | Receives exports from the payer. | Invoices, receipts and a ledger export that reconcile to the cent. |

## 2. The problem

Today a contractor sends a wallet address in a chat message. The payer copies it into a wallet, picks a network, and sends. Then:

- The payer has no proof of *who* they paid. A swapped address in a chat is a lost payment.
- The contractor converts through peer-to-peer trading: they find a counterparty, wait, accept whatever rate is offered, and carry counterparty risk.
- Neither side can see the state of a payment in plain words. "Is it sent?" is answered with a block explorer link.
- Paperwork is assembled by hand after the fact.

## 3. What Waybill does

**The contractor can:**
- create an invoice and send it as a payment link;
- choose to receive naira in their bank, or hold the stablecoin in a balance and withdraw later;
- see the naira rate locked at the moment the invoice is created, with an expiry;
- convert between supported assets inside their balance at a quoted rate.

**The payer can:**
- open the link and pay without an account, after seeing the contractor's verified name;
- pay in stablecoin on a supported network, or by card or bank transfer;
- pay a list of contractors in one transaction, with a second person approving runs above a threshold;
- download an invoice, a receipt and an accountant export.

**Both can** follow the payment on a public tracking page with four steps:

1. **Received** — the payment has been seen.
2. **Confirmed** — the payment is safe from being undone.
3. **Converted** — the stablecoin has been changed to naira at the locked rate (skipped when the contractor holds stablecoin).
4. **Sent to bank** — the naira has been sent to the contractor's bank account.

## 4. Journeys

### 4.1 Contractor: first invoice to naira in the bank

1. Signs up with email. Adds legal name and a Nigerian bank account.
2. Waybill verifies that the bank account name matches the legal name (fake verifier until the sandbox provider is chosen; see [DECISIONS.md](DECISIONS.md)).
3. Creates an invoice: description, amount in USD-stablecoin terms, and payout choice: *naira to my bank* or *hold as balance*.
4. If naira: sees the locked rate (for example "₦1,540.25 per USDC, locked for 48 hours") and the naira amount they will receive after fees.
5. Gets a payment link (`/pay/{code}`) and a tracking link (`/t/{code}`). Shares the payment link with the payer.
6. Watches the tracking page move through *received*, *confirmed*, *converted* and *sent to bank*.
7. Receives naira in their bank. A receipt is available to download.

### 4.2 Payer: paying a link

1. Opens the payment link. No account is needed.
2. Sees the contractor's verified name, the amount, what it is for, and when the link expires.
3. Chooses how to pay:
   - **Stablecoin.** Picks a network (Base Sepolia first). Sees a deposit address unique to this invoice, a QR code and the exact amount, or connects a wallet and pays in one click.
   - **Card or bank transfer.** Goes through Paystack's test checkout in naira.
4. Is taken to the tracking page, which updates live.
5. Downloads the invoice and the receipt.

### 4.3 Payer: batch pay with approval

1. Creates an organisation and invites a teammate as an approver.
2. Uploads or builds a list: contractor, amount, reference.
3. Waybill checks each line (verified contractor, open invoice or ad-hoc payment, duplicates) and shows the total.
4. If the total is above the organisation's threshold, the run waits for a different team member to approve it.
5. The run is paid in one on-chain transaction through the batch payout contract.
6. Each contractor gets their own tracking page. The payer gets one receipt for the run and an export.

### 4.4 Contractor: balance and conversion

1. Holds stablecoin in a balance (a ledger balance, not a self-custody wallet).
2. Requests a quote to convert between supported assets (for example USDC to NGN). The quote shows the rate, the fee and its expiry.
3. Accepts before expiry. Waybill posts the conversion to the ledger at exactly the quoted numbers.
4. Withdraws naira to their bank.

### 4.5 Edge journeys, described in plain words to the user

| Situation | What the user sees |
|---|---|
| Payer sends less than asked | "₦X short. The contractor can accept it as is, or you can send the rest." |
| Payer sends more | "Paid in full. The extra ₦X is held for the contractor and listed on the receipt." |
| Payment after the link expired | "This payment arrived after the link expired. The contractor will decide what to do with it." The rate is re-quoted. |
| Same payment seen twice | Nothing changes. Paid once, tracked once. |
| Network takes back a confirmed payment (reorg) | "We saw this payment, but the network has since undone it. We are watching for it again." |
| Bank transfer fails | "Your bank rejected the transfer. Check your account details; we'll retry once you confirm." |
| Provider does not answer | "We're checking with the bank. This can take a few minutes." This is never shown as *failed* or *not sent*. |

## 5. Scope

**In scope:** contractor invoices and payment links; rate lock; stablecoin pay-in on Base Sepolia, then Ethereum Sepolia; card and bank pay-in through Paystack test mode; naira payout through Paystack test mode; contractor balances and conversion; payer organisations, batch pay and dual approval; invoices, receipts and exports; public tracking pages; a public API and outgoing webhooks; Bitcoin receive on regtest and signet; proof of reserves; the "pull the plug" demo; an installable web app.

## 6. Non-goals

- Real funds, mainnets or live provider keys. The services refuse to start with them.
- A trading exchange, order books or market making.
- A self-custody wallet. Contractors do not hold keys in Waybill.
- A native mobile app.
- Payroll features: taxes, pensions, contracts.
- Currencies other than naira on the fiat side.

## 7. What "done" means

Waybill is done when, on the public demo:

1. A non-technical visitor can follow one payment from link to bank on a phone in under a minute, and understand every word on the tracking page.
2. "Pull the plug" can kill a worker mid-payout, force a reorg and replay a webhook, and the screen then shows that no money was lost and nobody was paid twice, with the ledger and on-chain balances to prove it.
3. The proof-of-reserves page shows on-chain holdings at least equal to balances owed, and any contractor can verify their own balance with a Merkle proof.
4. Every claim in the README status table is backed by a test or a command that a reader can run.
5. Every stage exit check in [ROADMAP.md](ROADMAP.md) has been run and has passed.
