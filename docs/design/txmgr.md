# Design note: transaction manager nonces and fees

**Status:** designed. Signatures and failing tests are in place; the owner implements `nonce.go` and `fees.go` (hand-written module, see CLAUDE.md).
**Code:** `api/db/migrations/00006_signer_nonces.sql`, `api/db/queries/txmgr.sql`, `api/internal/txmgr/` (provided: `txmgr.go`, `lease.go`; hand-written: `nonce.go`, `fees.go`).
**Done means:** `go test ./internal/txmgr/` passes: 6 nonce tests (including the I10 concurrency test), 2 lease and schema tests, and 5 fee tests (3 of them `rapid` properties, including I11).

## 1. What the transaction manager is for

Waybill sends transactions from its own keys: sweeping deposit forwarders into the vault and paying out batches. Two facts about EVM accounts make this the riskiest code that touches a chain:

- **Nonces.** Every transaction from an account carries a nonce, and the chain accepts nonces strictly in order: 0, 1, 2, … Two transactions with the same nonce compete, and only one can ever be mined. A skipped nonce (a gap) blocks every later transaction until it is filled.
- **Fees.** An EIP-1559 transaction names two caps: `maxFeePerGas` (the most it will pay per gas, base fee included) and `maxPriorityFeePerGas` (the tip to the block producer). If the base fee rises above the max fee, the transaction waits in the mempool, possibly for ever. The fix is a *replacement*: the same nonce, with higher fees.

Stage 1 met both problems by hand on Base Sepolia. A public node reported a stale pending nonce, so one send failed with "nonce too low" and was resent with an explicit `--nonce`. The transaction manager exists so that never needs a person.

## 2. Who writes what

| Piece | Written by | File |
|---|---|---|
| `signer_nonces` table and its never-decreases trigger | assistant | `00006_signer_nonces.sql` |
| `AcquireLease`: single-writer lease per key | assistant | `lease.go` |
| `NextNonce` | **owner** | `nonce.go` |
| `InitialFees`, `Replacement` | **owner** | `fees.go` |
| Outbound transaction and attempt tables, broadcast, receipt watching, deciding when a transaction is stuck | assistant, next roadmap item | — |

The hand-written part is pure decision logic plus one locked read-modify-write. Everything that talks to a node is plumbing and stays out of it, so the decisions can be tested without a chain.

## 3. The lease (provided)

One row per `(signer, network)` in `signer_nonces`. `AcquireLease(ctx, db, signer, network, owner, ttl)` takes the lease if nobody holds it, renews it if `owner` already does, and takes it over once it has expired; otherwise it returns `ErrLeaseHeld`. Times come from the database clock (`statement_timestamp()`), so two workers with skewed clocks still agree.

A worker renews well before `ExpiresAt`. If it crashes, it stops renewing, the lease expires, and another worker takes over. The new holder reads `next_nonce` from the same row, so it continues the sequence rather than starting again.

## 4. `NextNonce(ctx, tx, lease, chainPending) (uint64, error)`

Runs **inside the caller's transaction**: the same transaction that inserts the outbound transaction row carrying the nonce. If that transaction rolls back, nothing was recorded or broadcast with the nonce, so it was never really handed out, and the next call must return it again.

Steps:

1. Lock the key's row: `LockSignerNonce` (`SELECT … FOR UPDATE`). A second caller for the same key now waits until the first transaction ends. This lock is what makes I10 hold under concurrency.
2. No row: `ErrNoLease`. `lease_owner ≠ lease.Owner`: `ErrNotLeaseHolder`. `lease_expires_at ≤ db_now`: `ErrLeaseExpired`. Errors must be checkable with `errors.Is`.
3. `nonce = max(next_nonce, chainPending)`.
4. `SetSignerNextNonce(nonce + 1)` and return `nonce`.

Why the maximum, in both directions:

- **Stored ahead of the node** is normal. Nonces were assigned but not broadcast yet, or this node has not seen them (load-balanced public RPCs lag; see stage 1). Going back to the node's number would hand the same nonce to two transactions.
- **Node ahead of stored** means the key sent transactions outside Waybill. Using the stored number would fail with "nonce too low".

The database backs this up. `next_nonce` cannot decrease (trigger), and the outbound transaction table in the next item is unique on `(signer, network, nonce)`.

### Questions worth thinking through before coding

1. Could steps 1–4 be one `UPDATE … SET next_nonce = greatest(next_nonce, $pending) + 1 … RETURNING`? What would you lose in error reporting, and is the lock any different?
2. A lease expires *during* a long transaction, after the check in step 2. Another worker takes over and assigns the same nonce. Which constraint stops both transactions being recorded, and what does the loser see?
3. The process crashes after the commit but before the broadcast. Is the nonce lost, and what must the plumbing do on restart?
4. Why does a gap (assigned, never broadcast) block the key, and how is that different from a reused nonce?

## 5. Fees

`Fees{MaxFee, Tip *big.Int}` are wei per gas. Fee functions are pure: they never modify their inputs and never return pointers that alias them (the property tests check both).

### `InitialFees(baseFee, suggestedTip, maxFeeCap) (Fees, error)`

`Tip = suggestedTip` and `MaxFee = 2 × baseFee + Tip`. The base fee can rise at most 12.5 % per block, and 1.125⁶ ≈ 2.03, so doubling survives about six consecutive full blocks. Unused max fee is not charged: the payer pays `baseFee + tip`.

Errors: `ErrInvalidFees` for a nil or negative input; `ErrFeeCapExceeded` when `MaxFee > maxFeeCap`. The cap is a configured ceiling. Hitting it means fees are spiking, and the transaction waits for a person rather than paying whatever the network asks.

### `Replacement(prev, market, bumpPercent, maxFeeCap) (Fees, error)`

A node accepts a replacement only if **both** caps are higher than the transaction they replace by at least the node's price bump. Geth's default is 10 % (`txpool.pricebump`), and it returns `replacement transaction underpriced` otherwise. Raising only the tip, which is a common bug, is rejected.

For each of the two fees, the smallest acceptable value is

    bumped(x) = max(⌈x × (100 + bump) / 100⌉, x + 1)

- **Round up:** integer division rounds down and can land one wei short (`101 × 110 / 100 = 111.1`).
- **`x + 1`:** a zero tip bumped by 10 % is still zero, and an equal fee is not a replacement.

Then:

- `Tip = max(bumped(prev.Tip), market.Tip)`
- `MaxFee = max(bumped(prev.MaxFee), market.MaxFee, Tip)`. A max fee below the tip is invalid.
- `MaxFee > maxFeeCap`: `ErrFeeCapExceeded`.

Errors: `ErrInvalidFees` for a nil or negative value, `prev.MaxFee < prev.Tip`, or `bumpPercent == 0`.

The result is the *smallest* acceptable fee, not merely an acceptable one. Overpaying with test money costs nothing, but a predictable result is what lets the tests pin it exactly.

## 6. The tests that define "done"

| Test | What it pins |
|---|---|
| `TestNextNonce_FirstUseStartsAtChainPending` | first nonce comes from the node, then counts up |
| `TestNextNonce_StoredAheadOfNodeIsKept` | a lagging node never makes a nonce repeat |
| `TestNextNonce_NodeAheadOfStoredSkipsForward` | outside use of the key is respected |
| `TestNextNonce_RolledBackTransactionDoesNotConsume` | a rollback leaves no gap |
| `TestNextNonce_RequiresCurrentLease` | `ErrNoLease`, `ErrNotLeaseHolder`, `ErrLeaseExpired`; failed calls hand nothing out |
| `TestTxMgr_ConcurrentSendersNeverShareNonce` (**I10**) | 16 goroutines × 10 nonces get 0–159 exactly once; 16 more without the lease get none |
| `TestAcquireLease_OneHolderAtATime`, `TestSignerNonces_NextNonceNeverDecreases` | the provided lease and trigger (pass already) |
| `TestInitialFees`, `TestReplacement` | exact values, rounding, zero tip, market wins, cap, every invalid input |
| `TestTxMgr_ReplacementBumpsBothFees` (**I11**, `rapid`) | always accepted by the node rule, at or above market, max ≥ tip, minimal, inputs untouched, no aliasing |
| `TestReplacement_RepeatedBumpsKeepRising` (`rapid`) | 20 replacements in a row are each accepted, even from zero |
| `TestReplacement_CapIsAHardCeiling` (`rapid`) | never above the cap; `ErrFeeCapExceeded` only when the uncapped result is above it |

`TestTxMgr_AllAttemptHashesRecorded` (also I11) belongs to the plumbing and comes with it.
