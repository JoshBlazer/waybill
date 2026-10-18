// Package reconcile checks the ledger against the chain and the payment
// provider, and stores a report (docs/design/reconciliation.md).
//
// Checks:
//   - R1: the ledger is internally sound (trial balance, balance cache,
//     balance rules).
//   - R2, R3: vault and deposit custody against the chain, read at one
//     block height, after money in flight is accounted for.
//   - R4: every final payment and paid run matches its transaction receipt.
//   - R5: every settled payout and paid card checkout matches the provider.
//
// A balance that does not match once is "unsettled": a posting can land
// just after the chain is read. It is a difference only if the previous run
// found it unsettled too.
package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/batch"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/paystack"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/sweep"
)

// Chain is the part of an EVM client reconciliation reads.
type Chain interface {
	CallContract(ctx context.Context, msg ethereum.CallMsg, block *big.Int) ([]byte, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error)
}

// Provider is the part of the Paystack client reconciliation reads.
type Provider interface {
	Verify(ctx context.Context, ref string) (paystack.TransferStatus, error)
	VerifyTransaction(ctx context.Context, ref string) (paystack.ChargeStatus, error)
}

// Network is one EVM network to reconcile.
type Network struct {
	Chain      Chain
	Deployment deployments.Deployment
}

// Reconciler runs reports.
type Reconciler struct {
	Pool     *pgxpool.Pool
	Networks map[safety.Network]Network
	Provider Provider // nil: R5 is skipped
	Log      *slog.Logger
	Now      func() time.Time
	// ProviderConcurrency caps calls to the provider at once. Default 4.
	ProviderConcurrency int
}

// Status of one check.
type Status string

// The statuses a check can have.
const (
	OK         Status = "ok"
	Unsettled  Status = "unsettled"  // a balance is off once; checked again next run
	Difference Status = "difference" // something the job cannot explain
	Skipped    Status = "skipped"    // nothing to compare against
)

// InFlight is money the chain and the ledger disagree on for a known,
// temporary reason.
type InFlight struct {
	What   string `json:"what"`
	Ref    string `json:"ref"`
	Amount string `json:"amount"` // minor units, signed: + the chain holds more than the ledger
	amount money.Amount
}

func inFlight(what, ref string, a money.Amount) InFlight {
	return InFlight{What: what, Ref: ref, Amount: a.String(), amount: a}
}

// Check is one line of the report.
type Check struct {
	ID          string     `json:"id"` // R1 … R5, with the network for chain checks
	Name        string     `json:"name"`
	Status      Status     `json:"status"`
	Ledger      string     `json:"ledger,omitempty"`    // balance checks, minor units
	Chain       string     `json:"chain,omitempty"`     // balance checks, minor units
	Explained   string     `json:"explained,omitempty"` // sum of InFlight
	Unexplained string     `json:"unexplained,omitempty"`
	Checked     int        `json:"checked,omitempty"` // record checks
	InFlight    []InFlight `json:"inFlight,omitempty"`
	Problems    []string   `json:"problems,omitempty"`
	Note        string     `json:"note,omitempty"`
}

// Report is one reconciliation run.
type Report struct {
	ID          uuid.UUID                 `json:"id"`
	StartedAt   time.Time                 `json:"startedAt"`
	FinishedAt  time.Time                 `json:"finishedAt"`
	AsOf        map[safety.Network]uint64 `json:"asOf"`
	Since       time.Time                 `json:"since"` // record checks cover changes from here
	Checks      []Check                   `json:"checks"`
	Differences int                       `json:"differences"`
}

// Unsettled reports whether any check needs another run to decide.
func (r Report) Unsettled() bool {
	for _, c := range r.Checks {
		if c.Status == Unsettled {
			return true
		}
	}
	return false
}

// Options for one run.
type Options struct {
	Full bool // record checks cover everything, not only changes since the last clean run
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run checks everything once and stores the report.
func (r *Reconciler) Run(ctx context.Context, opts Options) (Report, error) {
	q := store.New(r.Pool)
	rep := Report{ID: uuid.New(), StartedAt: r.now(), AsOf: map[safety.Network]uint64{}}
	since, err := q.LastCleanReconciliationStart(ctx)
	if err != nil {
		return Report{}, err
	}
	if opts.Full {
		since = time.Unix(0, 0).UTC()
	}
	rep.Since = since
	prev := map[string]Status{}
	if last, err := q.LatestReconciliationRun(ctx); err == nil {
		var old Report
		if err := json.Unmarshal(last.Report, &old); err == nil {
			for _, c := range old.Checks {
				prev[c.ID] = c.Status
			}
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Report{}, err
	}

	ledgerCheck, err := r.checkLedger(ctx)
	if err != nil {
		return Report{}, err
	}
	rep.Checks = append(rep.Checks, ledgerCheck)

	nets := make([]safety.Network, 0, len(r.Networks))
	for n := range r.Networks {
		nets = append(nets, n)
	}
	sort.Slice(nets, func(i, j int) bool { return nets[i] < nets[j] })
	for _, n := range nets {
		cs, height, err := r.checkCustody(ctx, n, r.Networks[n])
		if err != nil {
			return Report{}, fmt.Errorf("reconcile %s: custody: %w", n, err)
		}
		rep.AsOf[n] = height
		for _, c := range cs {
			if c.Status == Unsettled && (prev[c.ID] == Unsettled || prev[c.ID] == Difference) {
				c.Status = Difference
				c.Note = "unexplained on two runs in a row"
			}
			rep.Checks = append(rep.Checks, c)
		}
		rc, err := r.checkChainRecords(ctx, n, r.Networks[n], since)
		if err != nil {
			return Report{}, fmt.Errorf("reconcile %s: records: %w", n, err)
		}
		rep.Checks = append(rep.Checks, rc)
	}

	pc, err := r.checkProvider(ctx, since)
	if err != nil {
		return Report{}, err
	}
	rep.Checks = append(rep.Checks, pc)

	for _, c := range rep.Checks {
		if c.Status == Difference {
			rep.Differences += max(1, len(c.Problems))
		}
	}
	rep.FinishedAt = r.now()
	if err := r.store(ctx, rep); err != nil {
		return Report{}, err
	}
	return rep, nil
}

// RunSettled runs once and, if a balance is unsettled, calls wait (which
// should let at least one finality period pass) and runs again, so the
// second report decides it.
func (r *Reconciler) RunSettled(ctx context.Context, opts Options, wait func(context.Context) error) (Report, error) {
	rep, err := r.Run(ctx, opts)
	if err != nil || !rep.Unsettled() {
		return rep, err
	}
	if err := wait(ctx); err != nil {
		return rep, err
	}
	return r.Run(ctx, opts)
}

func (r *Reconciler) store(ctx context.Context, rep Report) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	asOf, err := json.Marshal(rep.AsOf)
	if err != nil {
		return err
	}
	return store.New(r.Pool).InsertReconciliationRun(ctx, store.InsertReconciliationRunParams{
		ID: rep.ID, StartedAt: rep.StartedAt, FinishedAt: rep.FinishedAt, AsOf: asOf,
		Differences: int32(rep.Differences), Report: body, //nolint:gosec // a count of checks
	})
}

// R1.
func (r *Reconciler) checkLedger(ctx context.Context) (Check, error) {
	c := Check{ID: "R1", Name: "Ledger is internally sound", Status: OK}
	tb, err := ledger.TrialBalance(ctx, r.Pool)
	if err != nil {
		return c, err
	}
	for asset, v := range tb {
		if !v.IsZero() {
			c.Problems = append(c.Problems, fmt.Sprintf("postings in %s sum to %s, not zero", asset, v))
		}
	}
	if err := ledger.CheckBalanceCache(ctx, r.Pool); err != nil {
		c.Problems = append(c.Problems, err.Error())
	}
	bad, err := store.New(r.Pool).ListAccountsBreakingRule(ctx)
	if err != nil {
		return c, err
	}
	for _, b := range bad {
		c.Problems = append(c.Problems, fmt.Sprintf("%s has balance %s, against its rule", b.Code, b.Balance))
	}
	sort.Strings(c.Problems)
	if len(c.Problems) > 0 {
		c.Status = Difference
	}
	return c, nil
}

var (
	balanceOf     = crypto.Keccak256([]byte("balanceOf(address)"))[:4]
	transferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	sweptTopic    = crypto.Keccak256Hash([]byte("Swept(bytes32,address,address,uint256)"))
	batchPaid     = crypto.Keccak256Hash([]byte("BatchPaid(bytes32,address,uint256,uint256)"))
)

func tokenBalance(ctx context.Context, c Chain, token, who common.Address, block *big.Int) (money.Amount, error) {
	data := append(append([]byte{}, balanceOf...), common.LeftPadBytes(who.Bytes(), 32)...)
	out, err := c.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, block)
	if err != nil {
		return money.Zero, err
	}
	if len(out) != 32 {
		return money.Zero, fmt.Errorf("balanceOf(%s): %d bytes", who.Hex(), len(out))
	}
	return money.FromBig(new(big.Int).SetBytes(out))
}

// minedAt returns the receipt of the first of hashes mined at or below
// height, or nil.
func minedAt(ctx context.Context, c Chain, hashes []string, height uint64) (*types.Receipt, error) {
	for _, h := range hashes {
		rc, err := c.TransactionReceipt(ctx, common.HexToHash(h))
		if errors.Is(err, ethereum.NotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if rc.BlockNumber != nil && rc.BlockNumber.Uint64() <= height {
			return rc, nil
		}
	}
	return nil, nil
}

func sweptAmount(rc *types.Receipt, factory common.Address) *big.Int {
	sum := new(big.Int)
	for _, l := range rc.Logs {
		if l.Address == factory && len(l.Topics) == 4 && l.Topics[0] == sweptTopic && len(l.Data) == 32 {
			sum.Add(sum, new(big.Int).SetBytes(l.Data))
		}
	}
	return sum
}

// R2 and R3. Both are read at the same height: the head when the check
// starts. Everything the ledger has booked happened at or below it, since
// bookings wait for confirmations.
func (r *Reconciler) checkCustody(ctx context.Context, n safety.Network, nw Network) ([]Check, uint64, error) {
	head, err := nw.Chain.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	height := head.Number.Uint64()
	at := new(big.Int).SetUint64(height)
	d := nw.Deployment
	q := store.New(r.Pool)
	net := strings.ReplaceAll(string(n), ":", "_")

	vault := Check{ID: "R2:" + string(n), Name: "Vault custody on " + string(n)}
	deposit := Check{ID: "R3:" + string(n), Name: "Deposit custody on " + string(n)}

	// Pending sweeps and runs: their transaction may be mined already.
	attempts, err := q.ListPendingTxAttempts(ctx, store.ListPendingTxAttemptsParams{Network: string(n), Purposes: []string{sweep.Purpose, batch.Purpose}})
	if err != nil {
		return nil, 0, err
	}
	type pending struct {
		purpose, ref string
		hashes       []string
	}
	var order []uuid.UUID
	byTx := map[uuid.UUID]*pending{}
	for _, a := range attempts {
		p, ok := byTx[a.ID]
		if !ok {
			p = &pending{purpose: a.Purpose, ref: a.RefID}
			byTx[a.ID] = p
			order = append(order, a.ID)
		}
		p.hashes = append(p.hashes, a.Hash)
	}
	for _, id := range order {
		p := byTx[id]
		rc, err := minedAt(ctx, nw.Chain, p.hashes, height)
		if err != nil {
			return nil, 0, err
		}
		if rc == nil || rc.Status != types.ReceiptStatusSuccessful || p.purpose != sweep.Purpose {
			continue // runs are listed below, from the batch's own state
		}
		amt, err := money.FromBig(sweptAmount(rc, d.Factory))
		if err != nil || amt.IsZero() {
			continue
		}
		ref := "outbound_tx " + id.String()
		vault.InFlight = append(vault.InFlight, inFlight("sweep mined, not yet booked", ref, amt))
		deposit.InFlight = append(deposit.InFlight, inFlight("sweep mined, not yet booked", ref, amt.Neg()))
	}

	// Runs paid on chain but not yet booked out of the vault (at finality).
	runs, err := q.ListBatchesAwaitingFinality(ctx, string(n))
	if err != nil {
		return nil, 0, err
	}
	for _, b := range runs {
		paid := false
		switch {
		case b.TxState == "mined" && b.MinedBlock != nil && uint64(*b.MinedBlock) <= height: //nolint:gosec // block numbers are positive
			paid = true
		case b.TxState == "pending":
			var hs []string
			if p, ok := byTx[b.TxID]; ok {
				hs = p.hashes
			}
			rc, err := minedAt(ctx, nw.Chain, hs, height)
			if err != nil {
				return nil, 0, err
			}
			paid = rc != nil && rc.Status == types.ReceiptStatusSuccessful
		}
		if paid {
			vault.InFlight = append(vault.InFlight, inFlight("run paid, not yet final", "batch "+b.ID.String(), b.Total.Neg()))
		}
	}

	// Deposits are booked at first sighting. One seen in a block above the
	// height read here is in the ledger but not yet in the balance read.
	above, err := q.ListPaymentsAbove(ctx, store.ListPaymentsAboveParams{Network: string(n), Height: int64(height)}) //nolint:gosec // block height
	if err != nil {
		return nil, 0, err
	}
	for _, p := range above {
		deposit.InFlight = append(deposit.InFlight, inFlight("deposit booked above the height read", "payment "+p.ID.String(), p.Amount.Neg()))
	}

	// The chain.
	inVault, err := tokenBalance(ctx, nw.Chain, d.Token, d.Vault, at)
	if err != nil {
		return nil, 0, err
	}
	addrs, err := q.ListDepositAddressesByNetwork(ctx, string(n))
	if err != nil {
		return nil, 0, err
	}
	inDeposits := money.Zero
	for _, a := range addrs {
		b, err := tokenBalance(ctx, nw.Chain, d.Token, common.HexToAddress(a), at)
		if err != nil {
			return nil, 0, err
		}
		inDeposits = inDeposits.Add(b)
	}

	// The ledger.
	ledgerVault, err := q.GetBalanceByCode(ctx, fmt.Sprintf("custody:vault:%s:%s", net, money.USDC.Code))
	if err != nil {
		return nil, 0, err
	}
	ledgerDeposit, err := q.GetBalanceByCode(ctx, fmt.Sprintf("custody:deposit:%s:%s", net, money.USDC.Code))
	if err != nil {
		return nil, 0, err
	}
	compare(&vault, ledgerVault, inVault)
	compare(&deposit, ledgerDeposit, inDeposits)
	return []Check{vault, deposit}, height, nil
}

// compare fills a balance check: chain = ledger + in flight, or the rest is
// unexplained.
func compare(c *Check, inLedger, onChain money.Amount) {
	explained := money.Zero
	for _, f := range c.InFlight {
		explained = explained.Add(f.amount)
	}
	rest := onChain.Sub(inLedger).Sub(explained)
	c.Ledger, c.Chain, c.Explained, c.Unexplained = inLedger.String(), onChain.String(), explained.String(), rest.String()
	c.Status = OK
	if !rest.IsZero() {
		c.Status = Unsettled
		c.Problems = []string{fmt.Sprintf("chain %s, ledger %s, in flight %s: %s unexplained", onChain, inLedger, explained, rest)}
	}
}

// R4.
func (r *Reconciler) checkChainRecords(ctx context.Context, n safety.Network, nw Network, since time.Time) (Check, error) {
	c := Check{ID: "R4:" + string(n), Name: "On-chain records on " + string(n), Status: OK}
	q := store.New(r.Pool)
	pays, err := q.ListFinalPaymentsSince(ctx, store.ListFinalPaymentsSinceParams{Network: string(n), Since: since})
	if err != nil {
		return c, err
	}
	for _, p := range pays {
		c.Checked++
		rc, err := nw.Chain.TransactionReceipt(ctx, common.HexToHash(p.TxHash))
		if errors.Is(err, ethereum.NotFound) {
			c.Problems = append(c.Problems, fmt.Sprintf("payment %s: transaction %s not found", p.ID, p.TxHash))
			continue
		}
		if err != nil {
			return c, err
		}
		if why := matchPayment(rc, p); why != "" {
			c.Problems = append(c.Problems, fmt.Sprintf("payment %s: %s", p.ID, why))
		}
	}
	runs, err := q.ListPaidBatchesSince(ctx, store.ListPaidBatchesSinceParams{Network: string(n), Since: since})
	if err != nil {
		return c, err
	}
	for _, b := range runs {
		c.Checked++
		if b.MinedHash == nil {
			c.Problems = append(c.Problems, fmt.Sprintf("batch %s: paid with no mined transaction", b.ID))
			continue
		}
		rc, err := nw.Chain.TransactionReceipt(ctx, common.HexToHash(*b.MinedHash))
		if errors.Is(err, ethereum.NotFound) {
			c.Problems = append(c.Problems, fmt.Sprintf("batch %s: transaction %s not found", b.ID, *b.MinedHash))
			continue
		}
		if err != nil {
			return c, err
		}
		if why := matchBatch(rc, nw.Deployment.BatchPayout, b); why != "" {
			c.Problems = append(c.Problems, fmt.Sprintf("batch %s: %s", b.ID, why))
		}
	}
	if len(c.Problems) > 0 {
		c.Status = Difference
	}
	return c, nil
}

func matchPayment(rc *types.Receipt, p store.ListFinalPaymentsSinceRow) string {
	if rc.Status != types.ReceiptStatusSuccessful {
		return "its transaction reverted"
	}
	if !strings.EqualFold(rc.BlockHash.Hex(), p.BlockHash) {
		return fmt.Sprintf("mined in block %s, recorded in %s", strings.ToLower(rc.BlockHash.Hex()), p.BlockHash)
	}
	for _, l := range rc.Logs {
		if p.LogIndex < 0 || l.Index != uint(p.LogIndex) {
			continue
		}
		switch {
		case !strings.EqualFold(l.Address.Hex(), p.TokenAddress):
			return "the log is from another token"
		case len(l.Topics) != 3 || l.Topics[0] != transferTopic:
			return "the log is not a Transfer"
		case !strings.EqualFold(common.BytesToAddress(l.Topics[2].Bytes()).Hex(), p.ToAddress):
			return "the transfer went to another address"
		}
		got, err := money.FromBig(new(big.Int).SetBytes(l.Data))
		if err != nil || !got.Equal(p.Amount) {
			return fmt.Sprintf("the chain moved %s, the ledger booked %s", new(big.Int).SetBytes(l.Data), p.Amount)
		}
		return ""
	}
	return fmt.Sprintf("no log %d in its transaction", p.LogIndex)
}

func matchBatch(rc *types.Receipt, payout common.Address, b store.ListPaidBatchesSinceRow) string {
	if rc.Status != types.ReceiptStatusSuccessful {
		return "its transaction reverted"
	}
	id := common.BytesToHash(b.ChainBatchID)
	for _, l := range rc.Logs {
		if l.Address != payout || len(l.Topics) != 3 || l.Topics[0] != batchPaid || l.Topics[1] != id || len(l.Data) != 64 {
			continue
		}
		count := new(big.Int).SetBytes(l.Data[:32])
		total, err := money.FromBig(new(big.Int).SetBytes(l.Data[32:]))
		if err != nil || !total.Equal(b.Total) || count.Int64() != b.Lines {
			return fmt.Sprintf("the chain paid %s lines totalling %s; the run has %d totalling %s", count, new(big.Int).SetBytes(l.Data[32:]), b.Lines, b.Total)
		}
		return ""
	}
	return "no BatchPaid event for this run"
}

// R5.
func (r *Reconciler) checkProvider(ctx context.Context, since time.Time) (Check, error) {
	c := Check{ID: "R5", Name: "Provider records", Status: OK}
	if r.Provider == nil {
		c.Status, c.Note = Skipped, "no payment provider configured"
		return c, nil
	}
	q := store.New(r.Pool)
	payouts, err := q.ListSettledPayoutsSince(ctx, since)
	if err != nil {
		return c, err
	}
	checkouts, err := q.ListPaidCheckoutsSince(ctx, since)
	if err != nil {
		return c, err
	}
	limit := r.ProviderConcurrency
	if limit <= 0 {
		limit = 4
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		sem      = make(chan struct{}, limit)
	)
	report := func(problem string, err error) {
		mu.Lock()
		defer mu.Unlock()
		c.Checked++
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if problem != "" {
			c.Problems = append(c.Problems, problem)
		}
	}
	for _, p := range payouts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			st, err := r.Provider.Verify(ctx, p.Reference)
			if errors.Is(err, paystack.ErrNotFound) {
				report(fmt.Sprintf("payout %s: recorded %s; the provider has no such transfer", p.ID, p.State), nil)
				return
			}
			if err != nil {
				report("", fmt.Errorf("payout %s: verify: %w", p.ID, err))
				return
			}
			report(matchTransfer(p, st), nil)
		}()
	}
	for _, ch := range checkouts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			st, err := r.Provider.VerifyTransaction(ctx, ch.Reference)
			if errors.Is(err, paystack.ErrNotFound) {
				report(fmt.Sprintf("checkout %s: recorded paid; the provider has no such transaction", ch.ID), nil)
				return
			}
			if err != nil {
				report("", fmt.Errorf("checkout %s: verify: %w", ch.ID, err))
				return
			}
			problem := ""
			if st.Status != "success" || !st.Amount.Equal(ch.PaidAmount) {
				problem = fmt.Sprintf("checkout %s: provider says %s for %s kobo; recorded paid %s kobo", ch.ID, st.Status, st.Amount, ch.PaidAmount)
			}
			report(problem, nil)
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return c, firstErr
	}
	sort.Strings(c.Problems)
	if len(c.Problems) > 0 {
		c.Status = Difference
	}
	return c, nil
}

func matchTransfer(p store.ListSettledPayoutsSinceRow, st paystack.TransferStatus) string {
	want := map[string]string{"succeeded": "success", "failed": "failed", "reversed": "reversed"}[p.State]
	if st.Status != want {
		return fmt.Sprintf("payout %s: recorded %s; provider says %s", p.ID, p.State, st.Status)
	}
	if p.State == "succeeded" && !st.Amount.Equal(p.NairaAmount) {
		return fmt.Sprintf("payout %s: sent %s kobo; provider says %s", p.ID, p.NairaAmount, st.Amount)
	}
	return ""
}
