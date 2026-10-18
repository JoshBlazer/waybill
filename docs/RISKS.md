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
| Bank account name does not match the contractor. | Name verification against the bank before payout details can be saved. | `TestBankAccount_RefusalsSaveNothing` (mismatch, not found, bank unreachable) *(exists)* |
| Deposit address mapped to the wrong invoice. | The address is derived from `keccak(invoiceId)` and re-derived on-chain by the factory. The mapping is unique in the database. | Foundry `test_PredictMatchesDeployed`, `testFuzz_DistinctSaltsGiveDistinctAddresses` *(exist)*; Go `TestDepositAddress_Deterministic` *(stage 1)* |
| A contractor changes bank details right before a payout (account takeover). | Changes need re-verification, and payouts to new details wait 24 h. | `TestBankAccount_VerifiedSaveAndCoolingOff`, `TestPayout_WaitsForTheBankAccountCoolingOff` *(exist)* |
| Tracking codes are guessed, leaking names and amounts. | 80-bit random codes. Not-found responses never reveal near-misses. Rate limiting. | `TestTrackingCode_Entropy` *(stage 1)* |

## 3. Paying twice

| Risk | Guard | Proof |
|---|---|---|
| A client retries a create request. | Idempotency keys, stored in the same transaction as the effect. | I6 tests *(stage 1)* |
| A worker crashes after calling the provider but before recording it. | The provider reference equals our payout id, so the provider rejects the duplicate. The `unknown` state reconciles before any resend. | `TestPayout_CrashAfterSubmitDoesNotDoublePay`, `TestPayout_NotFoundIsResubmittedWithTheSameReference`, `TestPayout_ConcurrentWorkersPayOnce` *(exist)* |
| A webhook is replayed. | Unique `(provider, event_id)`, processed once; the event only triggers a verify by reference. | `TestPaystackWebhook_ReplayProcessedOnce`, `TestTick_ChargeCallbackIsCheckedNotTrusted`, `TestPayout_CallbacksAreCheckedAndProcessedOnce` *(exist)* |
| A replaced EVM transaction and the original both mine. | Both share one nonce, so only one can mine. Every hash is recorded before it is sent, and watched. | `TestTxMgr_StuckTransactionIsReplaced` *(exists)* |
| A batch payout is resubmitted. | `Vault.payout` rejects a reused payout id on-chain, whichever contract calls it. | `test_RevertWhen_PayoutIdReused`, `test_RevertWhen_PayoutIdReusedAcrossBatches`, `test_RevertWhen_BatchResubmitted`, `invariant_PayoutIdPaidAtMostOnce` *(exist)* |
| The same deposit log is processed twice. | Unique `(network, tx_hash, log_index)`. | I9 test *(stage 1)* |

## 4. Crediting money that is not there

| Risk | Guard | Proof |
|---|---|---|
| A reorg removes a payment that was credited. | Credits move to *available* only at *final*. Confirmed-but-reorged payments are reversed. | `TestWatcher_ReorgReversesConfirmedPayment` (I8) *(exists)* |
| A sweep logs an amount different from what moved. | Events are emitted before external calls; the factory reverts unless the sweep moves exactly the announced amount. | `test_RevertWhen_SweepMovesDifferentAmountThanAnnounced` *(exists)* |
| Fake token: a payer sends a token with the same symbol from another contract. | Only `chain_assets` contract addresses are recognised. | `TestWatcher_IgnoresUnknownToken` *(stage 1)* |
| An RPC lies or errors and a payment is assumed. | Confirmations read from the chain the watcher follows. RPC errors never advance state. Cross-checking with a second RPC *(stage 5)*. | I12 tests *(stage 2, 5)* |
| A forged provider webhook says "paid". | HMAC verification on the raw body, before parsing; and a callback never marks anything paid by itself: Waybill asks Paystack by reference. | `TestPaystackWebhook_BadSignatureRejected`, `TestTick_ChargeCallbackIsCheckedNotTrusted` *(exist)* |
| A ledger entry that does not balance. | Database trigger at commit. | I3 test *(stage 1)* |
| Someone edits history to hide a loss. | Append-only triggers and revoked grants. | I4 test *(stage 1)* |

## 5. Losing value in conversion

| Risk | Guard | Proof |
|---|---|---|
| Rounding drift across many conversions. | Integer math with one documented rounding rule: round down to the contractor; the sub-kobo remainder stays with Waybill's FX position. | `TestConvert_RoundsDownByLessThanOneMinorUnit` (`rapid`) *(exists)* |
| A stale rate is honoured after a market move. | Quotes expire; late payments are re-quoted. | `TestPayout_LatePaymentIsRequoted`, `TestQuoter_NoQuoteWhenTheRateCannotBeFound` *(exist)* |
| Float arithmetic slips in. | `money.Amount`; an AST test forbids floats in money packages. | I2 tests *(stage 1)* |

## 6. Key compromise and operator error

| Risk | Guard | Proof |
|---|---|---|
| The hot signing key leaks. | Vault per-transaction and daily limits (fail closed when unset); `PAUSER` role, and only the admin can unpause; signer behind an interface so a remote signer can replace it. | `test_RevertWhen_OverDailyLimit`, `test_PauseStopsPayoutsAndOnlyAdminUnpauses`, `testFuzz_DailyTotalNeverExceedsLimit` *(exist)* |
| A private key appears in logs. | Keys live behind the `txmgr.Signer` interface; `LocalSigner` prints only its address. | `TestSecret_NeverAppearsInLogs` *(exists)* |
| Two processes sign with the same key and collide on nonces. | Single-writer lease per key. | `TestTxMgr_ConcurrentSendersNeverShareNonce` (I10) *(exists)* |
| Webhook signing secrets are read from the database. | They must be readable to sign, so they are stored as given (test-mode `whsec_test_…` only), never returned after creation, never logged, and kept out of idempotency records. No KMS encryption at rest (ADR-038). | `TestWebhookEndpoints_IdempotentCreateDoesNotReplaySecret` *(exists)* |
| A webhook URL is used to reach Waybill's own network (SSRF). | https and public hosts only at registration; the dialer re-checks the resolved address; redirects are not followed. | `TestCheckURL`, `TestNewClient_RefusesPrivateAddressesAfterResolution`, `TestWebhook_PrivateAddressAtDeliveryIsRefused` *(exist)* |
| Bitcoin spending key on the server. | Server holds an xpub only. Config rejects private keys. | `TestBTCConfig_RejectsPrivateKey` *(stage 5)* |
| One person approves their own large batch. | Approver ≠ creator in SQL (composite foreign key and `CHECK`) and in code, whatever roles the creator holds. | `TestBatch_SelfApprovalRejected` *(exists)* |
| A stolen contractor key redirects a batch payment to the attacker's wallet. | A new or changed payout wallet waits 24 hours; a run records each wallet when checked and refuses to send if it changed since. | `TestPayoutWallet_SetAndCoolingOff` *(exists)*; the send-time recheck in `batch.Service.Send` |
| A run is sent twice, or two runs spend the same funds. | The total is reserved in the ledger (overdraft-proof) in the same transaction that records `sending`; the run's transaction is idempotent per run; `BatchPayout` and the vault refuse a reused batch or payout id on-chain. | `TestBatch_ConcurrentSendPaysOnce`, `TestBatch_SendReservesFundsOnceAndQueuesOneTransaction` *(exist)* |
| A run's mined transaction is removed by a reorg deeper than the transaction manager's confirmations. | Detected when finalising (the receipt is gone) and logged for a person; the run stays `confirming` and nothing is recorded as paid. Automatic rebroadcast needs the signed transaction kept, which the transaction manager does not do yet. | Known limitation (docs/design/teams.md §5) |
| Paystack float runs short and payouts fail. | Payouts fail into a visible state, never silently. Reconciliation checks every settled payout against the provider by reference and amount. It does not compare the float balance, because in test mode Paystack holds a prefund the ledger never sees (ADR-044); a live float check belongs with treasury work. | `TestReconcile_ProviderDisagreementIsADifference`, `TestReconcile_ProviderAgreesWithSucceededPayout` *(exist)* |

## 7. Stuck or invisible money

| Risk | Guard | Proof |
|---|---|---|
| A transaction is stuck with a low fee. | Replacement with both fees bumped after `StuckAfter`; fees above the cap wait for a person. | `TestTxMgr_ReplacementBumpsBothFees`, `TestTxMgr_StuckTransactionIsReplaced` (I11) *(exist)* |
| Funds arrive at an expired invoice's address. | The watcher keeps watching expired addresses; late payments are accepted (re-quoting comes with rates in stage 3). | `TestInvoice_LatePaymentAccepted` *(exists)* |
| Underpayment leaves funds in limbo. | `underpaid` state with explicit resolution paths; the short amount is credited to the contractor at finality, and a top-up moves the invoice on. The contractor's "accept shortfall" action is stage 3. | `TestWatcher_UnderpaidStaysUnderpaidAtFinality`, `TestWatcher_UnderpaidThenTopUp`, `TestWatcher_TwoPartPaymentIsConfirmedNotStuckUnderpaid` *(exist)* |
| A payout ends in "unknown" forever. | The reconciler queries by reference on a schedule and alerts after a threshold. | `TestPayout_ProviderTimeoutGoesUnknownNotFailed` (reconciled on every tick by reference) *(exists)* |
| Funds sent on the wrong network to the right address. | CREATE2 addresses are reproducible on any network where the factory is deployed at the same address; recovery is documented. | Runbook *(stage 6)* |
