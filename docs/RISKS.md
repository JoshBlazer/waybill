# Waybill — Risks to money

Every way this system could lose or misdirect money, and what guards against it. Waybill runs on test networks only, but each risk is designed for as if the money were real, because that is what the project demonstrates.

Columns: **Risk** · **Guard** · **Proof** (test or check; the stage is shown when it is not built yet).

## 1. Touching real funds

| Risk | Guard | Proof |
|---|---|---|
| A service is pointed at a mainnet chain. | Chain allowlist checked at startup. | `TestCheck_RejectsNonTestChains` *(exists)* |
| Config says testnet but the RPC URL is a mainnet node. | Live `eth_chainId` must match config, and failing to answer fails closed. | `TestCheck_LiveChainIDMismatch`, `TestCheck_RPCUnreachableFailsClosed` *(exist)* |
| A live Paystack key is configured. | Key must begin with `sk_test_`. | `TestCheck_RejectsLivePaystackKey` *(exists)* |
| Secrets are committed. | `.env` is git-ignored; only `.env.example` is tracked. `gitleaks` scans every commit in CI; fake keys in tests are allowlisted by exact string. | CI job `secrets`, `make secrets` *(exists)* |

## 2. Paying the wrong person

| Risk | Guard | Proof |
|---|---|---|
| A payer is phished with a look-alike link and pays an attacker. | The payment page shows the contractor's **verified** legal name before any address, and links are on Waybill's domain only. | Playwright test that the name precedes the address *(stage 3)* |
| Bank account name does not match the contractor. | Name verification against the bank before payout details can be saved. | `TestPayout_RequiresVerifiedAccount` *(stage 3)* |
| Deposit address mapped to the wrong invoice. | The address is derived from `keccak(invoiceId)` and re-derived on-chain by the factory. The mapping is unique in the database. | Foundry test `test_PredictMatchesDeployed`; `TestDepositAddress_Deterministic` *(stage 1)* |
| A contractor changes bank details right before a payout (account takeover). | Changes need re-verification, and payouts to new details wait 24 h. | `TestPayout_NewDetailsCoolingOff` *(stage 3)* |
| Tracking codes are guessed, leaking names and amounts. | 80-bit random codes. Not-found responses never reveal near-misses. Rate limiting. | `TestTrackingCode_Entropy` *(stage 1)* |

## 3. Paying twice

| Risk | Guard | Proof |
|---|---|---|
| A client retries a create request. | Idempotency keys, stored in the same transaction as the effect. | I6 tests *(stage 1)* |
| A worker crashes after calling the provider but before recording it. | The provider reference equals our payout id, so the provider rejects the duplicate. The `unknown` state reconciles before any resend. | `TestPayout_CrashAfterSubmitDoesNotDoublePay` *(stage 3)* |
| A webhook is replayed. | Unique `(provider, event_id)`, processed once. | I14 tests *(stage 3)* |
| A replaced EVM transaction and the original both mine. | Both share one nonce, so only one can mine. Every hash is watched. | I10/I11 tests *(stage 2)* |
| A batch payout is resubmitted. | `BatchPayout` rejects a reused payout id on-chain. | `test_RevertWhen_PayoutIdReused` *(stage 2)* |
| The same deposit log is processed twice. | Unique `(network, tx_hash, log_index)`. | I9 test *(stage 1)* |

## 4. Crediting money that is not there

| Risk | Guard | Proof |
|---|---|---|
| A reorg removes a payment that was credited. | Credits move to *available* only at *final*. Confirmed-but-reorged payments are reversed. | I8 test *(stage 2)* |
| Fake token: a payer sends a token with the same symbol from another contract. | Only `chain_assets` contract addresses are recognised. | `TestWatcher_IgnoresUnknownToken` *(stage 1)* |
| An RPC lies or errors and a payment is assumed. | Confirmations read from the chain the watcher follows. RPC errors never advance state. Cross-checking with a second RPC *(stage 5)*. | I12 tests *(stage 2, 5)* |
| A forged provider webhook says "paid". | HMAC verification on the raw body, before parsing. | I14 tests *(stage 3)* |
| A ledger entry that does not balance. | Database trigger at commit. | I3 test *(stage 1)* |
| Someone edits history to hide a loss. | Append-only triggers and revoked grants. | I4 test *(stage 1)* |

## 5. Losing value in conversion

| Risk | Guard | Proof |
|---|---|---|
| Rounding drift across many conversions. | Integer math with one documented rounding rule (round down to the contractor, remainder to a rounding account). | `rapid` property: sum of outputs plus remainder equals input *(stage 3)* |
| A stale rate is honoured after a market move. | Quotes expire; late payments are re-quoted. | `TestRateLock_ExpiredQuoteRequotes` *(stage 3)* |
| Float arithmetic slips in. | `money.Amount`; an AST test forbids floats in money packages. | I2 tests *(stage 1)* |

## 6. Key compromise and operator error

| Risk | Guard | Proof |
|---|---|---|
| The hot signing key leaks. | Vault per-transaction and daily limits; `PAUSER` role; signer behind an interface so a remote signer can replace it. | Foundry tests for limits and pause *(stage 2)* |
| A private key appears in logs. | `secret.String` redacts. No key is ever formatted with `%v`. | I15 test *(stage 1)* |
| Two processes sign with the same key and collide on nonces. | Single-writer lease per key. | I10 test *(stage 2)* |
| Bitcoin spending key on the server. | Server holds an xpub only. Config rejects private keys. | `TestBTCConfig_RejectsPrivateKey` *(stage 5)* |
| One person approves their own large batch. | Approver ≠ creator in SQL and in code. | I17 test *(stage 4)* |
| Paystack float runs short and payouts fail. | Reconciliation compares provider balance with ledger; payouts fail into a visible state, never silently. | `TestReconcile_DetectsShortfall` *(stage 4)* |

## 7. Stuck or invisible money

| Risk | Guard | Proof |
|---|---|---|
| A transaction is stuck with a low fee. | Replacement with bumped fees. | I11 test *(stage 2)* |
| Funds arrive at an expired invoice's address. | The watcher keeps watching expired addresses; late payments are accepted and re-quoted. | `TestInvoice_LatePaymentAccepted` *(stage 2)* |
| Underpayment leaves funds in limbo. | `underpaid` state with explicit resolution paths. | Underpaid tests *(stage 2–3)* |
| A payout ends in "unknown" forever. | The reconciler queries by reference on a schedule and alerts after a threshold. | `TestPayout_UnknownIsReconciled` *(stage 3)* |
| Funds sent on the wrong network to the right address. | CREATE2 addresses are reproducible on any network where the factory is deployed at the same address; recovery is documented. | Runbook *(stage 6)* |
