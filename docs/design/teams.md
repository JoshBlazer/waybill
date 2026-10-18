# Design note: teams and batch pay (stage 4)

**Status:** approved 2026-10-18 (ADR-043); being implemented.
**Covers:** the first two stage 4 roadmap items: payer organisations, members and roles; and batch pay (build, check, approve with approver ≠ creator, send through `BatchPayout`). Receipts and exports, contractor conversion and the reconciliation job will each get their own short note.
**Owner decisions (2026-10-18):** money moves **on-chain to contractor wallets** through `BatchPayout`, funded from the organisation's USDC balance. Members sign in with **API keys**, and the web app acts as seeded demo members, as it does for the demo contractor today.

## 1. The journey

1. An organisation exists with an admin. In the demo it is seeded, like the demo contractor.
2. The admin adds members and gives them roles, then sets the approval threshold.
3. The organisation funds its balance by sending test USDC to its own deposit address. The watcher credits it exactly as it credits invoice payments: pending at first sighting, available at finality.
4. A **maker** builds a run: lines of *contractor, amount, reference*.
5. Waybill checks every line and shows the total and each contractor's verified name.
6. If the total is above the threshold, the run waits for an **approver** who is not its creator. At or below the threshold, the creator may send it.
7. Waybill sends the run in **one** `BatchPayout.payBatch` transaction, through the existing transaction manager.
8. Each line has its own public tracking page. The run has one receipt; the receipt comes with the exports item.

## 2. Roles

| Role | May |
|---|---|
| `admin` | Manage members, roles, API keys and the threshold; everything below. |
| `maker` | Build, check, send (at or below threshold) and cancel runs. |
| `approver` | Approve or reject runs above the threshold. |

- A member can hold several roles.
- **Approver ≠ creator** always applies, whatever roles the creator holds (I17). It is enforced twice: in code, and in SQL with a composite foreign key plus a `CHECK` (§4), so a bug in one place cannot let a creator approve their own run.
- The last admin cannot be removed or demoted.

## 3. Checks on each line

A run is refused or held back, with a plain reason on each line, when any of these fails:

- **Contractor verified.** The contractor exists and their legal name is verified.
- **Wallet ready.** The contractor has a registered payout wallet on the run's network, and its 24-hour cooling-off after a change has passed. This is the same rule as bank accounts (RISKS §2, account takeover).
- **Amount.** It is positive and within the vault's per-transaction limit.
- **No duplicate.** The same contractor and reference have not been paid by this organisation in the last 30 days.

And on the run as a whole:

- **Size.** 1–200 lines (`BatchPayout.MAX_ITEMS`).
- **Funds.** The total is at most the organisation's **available** balance.
- **Daily limit.** It fits the vault's remaining daily limit, checked again just before sending.

## 4. Schema (new migration, ledger tables unchanged)

- `payer_orgs`: `id`, `name`, `approval_threshold` (minor units of USDC), `created_at`.
- `org_members`: `id`, `org_id`, `name`, `roles text[]` with a `CHECK` that the roles are from the set, and `removed_at`.
- `api_keys`: `contractor_id` becomes nullable and gains `member_id`, with a `CHECK` that exactly one is set. Existing keys are unchanged.
- `contractor_wallets`: `contractor_id`, `network`, `address`, `added_at`, `payouts_allowed_from`. Unique per `(contractor_id, network)`.
- `deposit_addresses` and `payments`: `invoice_id` becomes nullable and they gain `org_id`, with a `CHECK` that exactly one is set. Funding addresses use the same CREATE2 forwarders, with `salt = keccak256("org:" || org_id)`.
- `batches`: `id`, `org_id`, `created_by`, `network`, `asset_code`, `total`, `state`, `outbound_tx_id`, timestamps. Unique `(id, created_by)` for the approval key below.
- `batch_lines`: `batch_id`, `line_no`, `contractor_id`, `wallet_address` (a snapshot at check time), `amount`, `reference`, `payout_id` (bytes32, unique), `tracking_code` (unique).
- `approvals`: `batch_id`, `creator_id`, `approver_id`, `decision`, `decided_at`.
  - `FOREIGN KEY (batch_id, creator_id) REFERENCES batches (id, created_by)` and `CHECK (approver_id <> creator_id)`. The database itself refuses a self-approval.
- `batch_events`: like `invoice_events`, feeding the tracking pages.

## 5. Batch states

| From | Event | To |
|---|---|---|
| `draft` | `checked` (all lines pass) | `ready` |
| `ready` | `needs_approval` (total > threshold) | `awaiting_approval` |
| `ready`, `awaiting_approval` | `approved` (by someone else when above threshold) | `approved` |
| `awaiting_approval` | `rejected` | `rejected` (terminal) |
| `draft`, `ready`, `awaiting_approval`, `approved` | `cancel` | `cancelled` (terminal) |
| `approved` | `submit` (recorded **before** broadcast) | `sending` |
| `sending` | `mined` | `confirming` |
| `sending`, `confirming` | `reverted` | `failed` (terminal; the reservation is reversed) |
| `confirming` | `final` | `paid` (terminal) |
| `confirming` | `reorged_out` | `sending` (the transaction manager rebroadcasts the same transaction) |

- The machine is pure: `statemachine.BatchTransition`, with an exhaustive pair table test, like `InvoiceTransition` and `PayoutTransition`.
- Written test-first (CLAUDE.md), under the owner waiver of 2026-10-18.

## 6. Ledger

- **Funding:** at sighting, `custody:deposit:{net}:USDC` → `liability:org:{id}:USDC:pending`. At finality, the forwarder is swept like an invoice's and the amount moves to `liability:org:{id}:USDC:available`.
- **Approve and send:** in one entry, recorded with the `submit` transition, `liability:org:{id}:USDC:available` → `liability:batch:{batch_id}:USDC:in-flight`. Funds are never promised twice. `non_negative` on the org account makes overspending impossible.
- **Final:** `liability:batch:{batch_id}:USDC:in-flight` → `custody:vault:{net}:USDC`, which records the outflow. The contractors are paid on-chain, so no contractor liability is created.
- **Reverted:** the approve-and-send entry is reversed. The money returns to the organisation's available balance.
- New account codes only, no new ledger tables, no ledger schema change.

## 7. Public API (OpenAPI 0.6.0)

**Member-authenticated:**
- `GET /v1/org`.
- `GET` and `POST /v1/org/members`; `PATCH /v1/org/members/{id}` (roles, remove).
- `PATCH /v1/org` (threshold; admin only).
- `GET /v1/org/funding`: deposit address per network, plus pending and available balance.
- `POST /v1/batches` creates and checks a run, with an `Idempotency-Key`. `GET /v1/batches`, `GET /v1/batches/{id}`.
- `POST /v1/batches/{id}/approve`, `/reject`, `/send`, `/cancel`.

**Contractor-authenticated:** `GET` and `PUT /v1/me/payout-wallet`.

**Public:**
- `GET /v1/track/{code}` also serves batch lines. The four tracking steps become `received → confirmed` (the on-chain payment) with `converted` and `sent_to_bank` shown as `skipped`, so the existing `Tracking` schema is unchanged.

**Outgoing webhooks:** `batch.approved`, `batch.paid`, `batch.failed`.

Lines name contractors by id, and the response shows each contractor's verified name so the approver sees who is being paid. In the demo, the seeded contractor's id is shown on its account page.

## 8. Web (demo)

- `/org`: members, roles, threshold and funding (address, balances).
- `/org/batches/new`: build a run, see the checked lines and total.
- `/org/batches/{id}`: approve, reject or send, with a member switcher (demo admin, demo approver) so dual approval can be shown on one screen. The switcher picks which server-held key the server acts with. Server-held keys only; the browser never sees a key.
- `/account/wallet`: the contractor registers a payout wallet.

## 9. Tests and the exit check

- `TestBatch_SelfApprovalRejected` covers I17, in code and with a direct SQL insert.
- Tests for each line check and the funds check.
- `TestBatch_RevertedRunReturnsFunds` and `TestBatch_ConcurrentSendPaysOnce`.
- **Exit check** `TestExitCheck_BatchWithDualApprovalCompletes`, on Anvil with the real contracts:
  1. Fund the organisation and register two contractor wallets.
  2. A maker builds a run of 2 lines above the threshold; the maker's own approval is refused; an approver approves it.
  3. One `payBatch` transaction pays both wallets. The run reaches `paid`.
  4. Both wallets hold the amounts on-chain, the trial balance is zero, and the organisation's balance is reduced by exactly the total.
- The reconciliation report is part of the exit check but has its own roadmap item and note.
- Playwright: build a run, approve it as the other member, and see it paid.

## 10. Not in this note

Receipts and the accountant export (CSV and PDF; PDF may need a new dependency, so I'll ask first), contractor balance conversion, and the reconciliation job.
