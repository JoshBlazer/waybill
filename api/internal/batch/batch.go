// Package batch runs batch payouts: an organisation pays several
// contractors' wallets in one BatchPayout transaction, from its funded USDC
// balance, with a second person approving runs above the threshold
// (docs/design/teams.md, ADR-043).
//
// Every state change goes through statemachine.BatchTransition and is
// recorded in batch_events in the same transaction. Approver ≠ creator
// (I17) is checked here and refused again by the database.
package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/org"
	"github.com/JoshBlazer/waybill/api/internal/outbox"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// MaxLines is BatchPayout.MAX_ITEMS.
const MaxLines = 200

// DuplicateWindow is how far back a repeated (contractor, reference) is a
// duplicate.
const DuplicateWindow = 30 * 24 * time.Hour

// Errors. Handlers map them to HTTP answers.
var (
	ErrForbidden      = org.ErrForbidden
	ErrNotFound       = errors.New("batch: no such run in this organisation")
	ErrInvalid        = errors.New("batch: the run is not valid")
	ErrSelfApproval   = errors.New("batch: nobody approves their own run")
	ErrWrongState     = errors.New("batch: the run cannot do that in its current state")
	ErrNotReady       = errors.New("batch: some lines cannot be paid")
	ErrNoFunds        = errors.New("batch: the organisation's available balance is less than the run's total")
	ErrOverDailyLimit = errors.New("batch: the run is over what the vault may still pay out today")
	ErrNoContract     = errors.New("batch: no BatchPayout contract on this network")
)

// LineInput is one line as a maker enters it.
type LineInput struct {
	ContractorID uuid.UUID
	Amount       string // USDC, decimal
	Reference    string
}

// Service manages batch runs. Limits is optional: without it the vault's
// limits are enforced only on-chain.
type Service struct {
	Pool        *pgxpool.Pool
	Deployments map[safety.Network]deployments.Deployment
	Limits      map[safety.Network]VaultLimits
	Now         func() time.Time
	Log         *slog.Logger
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Run is a batch with its lines and approvals.
type Run struct {
	store.Batch
	Lines     []store.ListBatchLinesRow
	Approvals []store.Approval
	TxHash    *string
	Threshold money.Amount
}

// NeedsApproval reports whether the run's total is above the threshold.
func (r Run) NeedsApproval() bool { return r.Total.Cmp(r.Threshold) > 0 }

func chainBatchID(id uuid.UUID) [32]byte {
	return crypto.Keccak256Hash([]byte("batch:"), id[:])
}

func payoutID(id uuid.UUID, line int) [32]byte {
	return crypto.Keccak256Hash([]byte(fmt.Sprintf("batch-line:%s:%d", id, line)))
}

// network picks the run's network: the one asked for, or the only one
// with a BatchPayout contract.
func (s *Service) network(want string) (safety.Network, deployments.Deployment, error) {
	if want != "" {
		d, ok := s.Deployments[safety.Network(want)]
		if !ok || d.BatchPayout == ([20]byte{}) {
			return "", deployments.Deployment{}, ErrNoContract
		}
		return safety.Network(want), d, nil
	}
	var found []safety.Network
	for n, d := range s.Deployments {
		if d.BatchPayout != ([20]byte{}) {
			found = append(found, n)
		}
	}
	slices.Sort(found)
	if len(found) == 0 {
		return "", deployments.Deployment{}, ErrNoContract
	}
	return found[0], s.Deployments[found[0]], nil
}

func requireRole(ctx context.Context, db store.DBTX, p auth.Principal, roles ...string) (store.OrgMember, error) {
	m, err := org.Caller(ctx, db, p)
	if err != nil {
		return store.OrgMember{}, err
	}
	for _, r := range roles {
		if org.HasRole(m, r) {
			return m, nil
		}
	}
	return store.OrgMember{}, ErrForbidden
}

// Create builds a run and checks every line. A run whose lines all pass
// becomes ready (or awaiting approval above the threshold); otherwise it
// stays a draft with each problem stated. Makers only.
func (s *Service) Create(ctx context.Context, tx pgx.Tx, p auth.Principal, network string, in []LineInput) (Run, error) {
	m, err := requireRole(ctx, tx, p, org.RoleMaker)
	if err != nil {
		return Run{}, err
	}
	if len(in) == 0 || len(in) > MaxLines {
		return Run{}, fmt.Errorf("%w: a run has 1 to %d lines", ErrInvalid, MaxLines)
	}
	net, _, err := s.network(network)
	if err != nil {
		return Run{}, err
	}
	q := store.New(tx)
	o, err := q.GetOrg(ctx, p.OrgID)
	if err != nil {
		return Run{}, err
	}
	type line struct {
		in      LineInput
		amount  money.Amount
		wallet  *string
		problem *string
	}
	lines := make([]line, len(in))
	total := money.FromInt64(0)
	seen := map[string]bool{}
	for i, l := range in {
		amt, err := money.ParseDecimal(strings.TrimSpace(l.Amount), money.USDC.Scale)
		if err != nil || amt.Sign() <= 0 {
			return Run{}, fmt.Errorf("%w: line %d: the amount must be a positive USDC amount with at most 6 decimals", ErrInvalid, i+1)
		}
		ref := strings.TrimSpace(l.Reference)
		if ref == "" || len(ref) > 140 {
			return Run{}, fmt.Errorf("%w: line %d: the reference must be 1 to 140 characters", ErrInvalid, i+1)
		}
		in[i].Reference = ref
		total = total.Add(amt)
		lines[i] = line{in: in[i], amount: amt}
		problem, wallet, err := s.checkLine(ctx, q, p.OrgID, net, in[i], amt)
		if err != nil {
			return Run{}, err
		}
		key := l.ContractorID.String() + "\x00" + ref
		if problem == "" && seen[key] {
			problem = "This contractor and reference appear twice in this run."
		}
		seen[key] = true
		if problem != "" {
			lines[i].problem = &problem
		}
		lines[i].wallet = wallet
	}

	id := uuid.New()
	cb := chainBatchID(id)
	var runProblem *string
	available, err := s.available(ctx, q, p.OrgID)
	if err != nil {
		return Run{}, err
	}
	if total.Cmp(available) > 0 {
		msg := fmt.Sprintf("The total is more than the organisation's available balance (%s USDC). Fund the organisation first.", available.Format(money.USDC.Scale))
		runProblem = &msg
	}
	b, err := q.InsertBatch(ctx, store.InsertBatchParams{
		ID: id, OrgID: p.OrgID, CreatedBy: m.ID, Network: string(net), AssetCode: string(money.USDC.Code),
		Total: total, ChainBatchID: cb[:], Failure: runProblem,
	})
	if err != nil {
		return Run{}, err
	}
	allOK := runProblem == nil
	for i, l := range lines {
		code, err := invoice.NewTrackingCode()
		if err != nil {
			return Run{}, err
		}
		pid := payoutID(id, i+1)
		if err := q.InsertBatchLine(ctx, store.InsertBatchLineParams{
			BatchID: id, LineNo: int32(i + 1), ContractorID: l.in.ContractorID, WalletAddress: l.wallet,
			Amount: l.amount, Reference: l.in.Reference, Problem: l.problem, PayoutID: pid[:], TrackingCode: code,
		}); err != nil {
			return Run{}, err
		}
		if l.problem != nil {
			allOK = false
		}
	}
	if err := s.event(ctx, q, b.ID, "created", "", statemachine.BatchDraft, &m.ID, map[string]any{"lines": len(lines), "total": total.String()}); err != nil {
		return Run{}, err
	}
	if allOK {
		st, err := s.apply(ctx, q, b, statemachine.EvBatchChecked, &m.ID, nil)
		if err != nil {
			return Run{}, err
		}
		if total.Cmp(o.ApprovalThreshold) > 0 {
			b.State = string(st)
			if _, err := s.apply(ctx, q, b, statemachine.EvBatchNeedsApproval, &m.ID, map[string]any{"threshold": o.ApprovalThreshold.String()}); err != nil {
				return Run{}, err
			}
		}
	}
	return s.load(ctx, tx, p.OrgID, id)
}

// checkLine returns why a line cannot be paid ("" when it can) and the
// contractor's wallet on the network.
func (s *Service) checkLine(ctx context.Context, q *store.Queries, orgID uuid.UUID, net safety.Network, l LineInput, amt money.Amount) (string, *string, error) {
	c, err := q.GetContractorByID(ctx, l.ContractorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, fmt.Errorf("%w: no contractor has the id %s", ErrInvalid, l.ContractorID)
	}
	if err != nil {
		return "", nil, err
	}
	if c.VerifiedAt == nil {
		return "This contractor's name is not verified yet.", nil, nil
	}
	w, err := q.GetContractorWallet(ctx, store.GetContractorWalletParams{ContractorID: c.ID, Network: string(net)})
	if errors.Is(err, pgx.ErrNoRows) {
		return "This contractor has no payout wallet on this network.", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	wallet := w.Address
	if s.now().Before(w.PayoutsAllowedFrom) {
		return "This contractor changed their wallet less than 24 hours ago; it can be paid from " +
			w.PayoutsAllowedFrom.UTC().Format(time.RFC3339) + ".", &wallet, nil
	}
	n, err := q.CountRecentLinePayments(ctx, store.CountRecentLinePaymentsParams{OrgID: orgID, ContractorID: c.ID, Reference: l.Reference})
	if err != nil {
		return "", nil, err
	}
	if n > 0 {
		return "This contractor was already paid with this reference in the last 30 days.", &wallet, nil
	}
	if lim, ok := s.Limits[net]; ok && lim != nil {
		perTx, _, err := lim.Limits(ctx)
		if err != nil {
			s.log().Warn("batch: could not read vault limits; the chain will enforce them", "err", err)
		} else if amt.Cmp(perTx) > 0 {
			return fmt.Sprintf("The amount is over the vault's limit per payment (%s USDC).", perTx.Format(money.USDC.Scale)), &wallet, nil
		}
	}
	return "", &wallet, nil
}

func (s *Service) available(ctx context.Context, q *store.Queries, orgID uuid.UUID) (money.Amount, error) {
	b, err := q.GetBalanceByCode(ctx, orgAccount(orgID, "available"))
	if err != nil {
		return money.Amount{}, err
	}
	return b.Neg(), nil
}

func orgAccount(orgID uuid.UUID, which string) string {
	return fmt.Sprintf("liability:org:%s:%s:%s", orgID, money.USDC.Code, which)
}

func batchAccount(id uuid.UUID) string {
	return fmt.Sprintf("liability:batch:%s:%s:in-flight", id, money.USDC.Code)
}

// apply moves b by ev and records the event. It returns the new state.
func (s *Service) apply(ctx context.Context, q *store.Queries, b store.Batch, ev statemachine.BatchEvent, member *uuid.UUID, data map[string]any) (statemachine.BatchState, error) {
	from := statemachine.BatchState(b.State)
	to, err := statemachine.BatchTransition(from, ev)
	if err != nil {
		return from, fmt.Errorf("%w: %w", ErrWrongState, err)
	}
	var failure *string
	if f, ok := data["failure"].(string); ok {
		failure = &f
	} else if to == statemachine.BatchReady || to == statemachine.BatchAwaitingApproval || to == statemachine.BatchApproved {
		failure = nil
	} else {
		failure = b.Failure
	}
	if err := q.SetBatchState(ctx, store.SetBatchStateParams{ID: b.ID, State: string(to), Failure: failure}); err != nil {
		return from, err
	}
	return to, s.event(ctx, q, b.ID, string(ev), from, to, member, data)
}

func (s *Service) event(ctx context.Context, q *store.Queries, id uuid.UUID, typ string, from, to statemachine.BatchState, member *uuid.UUID, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return q.InsertBatchEvent(ctx, store.InsertBatchEventParams{
		BatchID: id, Type: typ, FromState: string(from), ToState: string(to), MemberID: member, Data: raw,
	})
}

// Get returns one of the organisation's runs.
func (s *Service) Get(ctx context.Context, orgID, id uuid.UUID) (Run, error) {
	return s.load(ctx, s.Pool, orgID, id)
}

// List returns the organisation's 50 newest runs, without their lines.
func (s *Service) List(ctx context.Context, orgID uuid.UUID) ([]store.Batch, error) {
	return store.New(s.Pool).ListBatches(ctx, orgID)
}

func (s *Service) load(ctx context.Context, db store.DBTX, orgID, id uuid.UUID) (Run, error) {
	q := store.New(db)
	b, err := q.GetBatch(ctx, store.GetBatchParams{ID: id, OrgID: orgID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	r := Run{Batch: b}
	if r.Lines, err = q.ListBatchLines(ctx, id); err != nil {
		return Run{}, err
	}
	if r.Approvals, err = q.ListApprovals(ctx, id); err != nil {
		return Run{}, err
	}
	o, err := q.GetOrg(ctx, orgID)
	if err != nil {
		return Run{}, err
	}
	r.Threshold = o.ApprovalThreshold
	if b.OutboundTxID != nil {
		t, err := q.GetOutboundTx(ctx, *b.OutboundTxID)
		if err != nil {
			return Run{}, err
		}
		r.TxHash = t.MinedHash
	}
	return r, nil
}

// lock loads a run of the caller's organisation for update.
func lock(ctx context.Context, q *store.Queries, orgID, id uuid.UUID) (store.Batch, error) {
	b, err := q.GetBatchForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && b.OrgID != orgID) {
		return store.Batch{}, ErrNotFound
	}
	return b, err
}

// Approve records an approval by someone other than the creator and makes
// the run sendable. Approvers only.
func (s *Service) Approve(ctx context.Context, p auth.Principal, id uuid.UUID) (Run, error) {
	return s.decide(ctx, p, id, "approved", statemachine.EvBatchApproved)
}

// Reject records a refusal; the run can never be sent. Approvers only.
func (s *Service) Reject(ctx context.Context, p auth.Principal, id uuid.UUID) (Run, error) {
	return s.decide(ctx, p, id, "rejected", statemachine.EvBatchRejected)
}

func (s *Service) decide(ctx context.Context, p auth.Principal, id uuid.UUID, decision string, ev statemachine.BatchEvent) (Run, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		m, err := requireRole(ctx, tx, p, org.RoleApprover)
		if err != nil {
			return err
		}
		q := store.New(tx)
		b, err := lock(ctx, q, p.OrgID, id)
		if err != nil {
			return err
		}
		if b.CreatedBy == m.ID {
			return ErrSelfApproval // I17; the database refuses it too
		}
		if statemachine.BatchState(b.State) != statemachine.BatchAwaitingApproval {
			return fmt.Errorf("%w: it is %s", ErrWrongState, b.State)
		}
		if err := q.InsertApproval(ctx, store.InsertApprovalParams{
			BatchID: b.ID, CreatorID: b.CreatedBy, ApproverID: m.ID, Decision: decision,
		}); err != nil {
			return err
		}
		_, err = s.apply(ctx, q, b, ev, &m.ID, nil)
		return err
	})
	if err != nil {
		return Run{}, err
	}
	return s.Get(ctx, p.OrgID, id)
}

// Cancel withdraws a run that has not been sent. Makers and admins.
func (s *Service) Cancel(ctx context.Context, p auth.Principal, id uuid.UUID) (Run, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		m, err := requireRole(ctx, tx, p, org.RoleMaker, org.RoleAdmin)
		if err != nil {
			return err
		}
		q := store.New(tx)
		b, err := lock(ctx, q, p.OrgID, id)
		if err != nil {
			return err
		}
		_, err = s.apply(ctx, q, b, statemachine.EvBatchCancel, &m.ID, nil)
		return err
	})
	if err != nil {
		return Run{}, err
	}
	return s.Get(ctx, p.OrgID, id)
}

// Send reserves the run's total from the organisation's balance, records
// the run as sending and queues its transaction, in one database
// transaction. A run at or below the threshold may be sent by any maker,
// including its creator; above it, only once someone else approved it.
func (s *Service) Send(ctx context.Context, p auth.Principal, id uuid.UUID) (Run, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		m, err := requireRole(ctx, tx, p, org.RoleMaker)
		if err != nil {
			return err
		}
		q := store.New(tx)
		b, err := lock(ctx, q, p.OrgID, id)
		if err != nil {
			return err
		}
		o, err := q.GetOrg(ctx, p.OrgID)
		if err != nil {
			return err
		}
		switch statemachine.BatchState(b.State) {
		case statemachine.BatchReady:
			if b.Total.Cmp(o.ApprovalThreshold) > 0 {
				// The threshold was lowered after the run was checked: it
				// now needs an approval like any other large run.
				_, err := s.apply(ctx, q, b, statemachine.EvBatchNeedsApproval, &m.ID, map[string]any{"threshold": o.ApprovalThreshold.String()})
				if err != nil {
					return err
				}
				return fmt.Errorf("%w: the run is above the threshold and needs an approver", ErrWrongState)
			}
			st, err := s.apply(ctx, q, b, statemachine.EvBatchApproved, &m.ID, map[string]any{"belowThreshold": true})
			if err != nil {
				return err
			}
			b.State = string(st)
		case statemachine.BatchApproved:
		case statemachine.BatchDraft:
			return ErrNotReady
		default:
			return fmt.Errorf("%w: it is %s", ErrWrongState, b.State)
		}
		if err := s.recheck(ctx, q, b); err != nil {
			return err
		}
		if err := s.reserve(ctx, tx, b); err != nil {
			return err
		}
		if _, err := s.apply(ctx, q, b, statemachine.EvBatchSubmit, &m.ID, nil); err != nil {
			return err
		}
		_, err = outbox.Enqueue(ctx, tx, Topic, Job{BatchID: b.ID}, outbox.Options{DedupeKey: "batch:" + b.ID.String()})
		return err
	})
	if err != nil {
		return Run{}, err
	}
	return s.Get(ctx, p.OrgID, id)
}

// recheck repeats the checks that can change between building and
// sending: wallets (and their cooling-off) and the vault's daily room.
func (s *Service) recheck(ctx context.Context, q *store.Queries, b store.Batch) error {
	lines, err := q.ListBatchLines(ctx, b.ID)
	if err != nil {
		return err
	}
	for _, l := range lines {
		w, err := q.GetContractorWallet(ctx, store.GetContractorWalletParams{ContractorID: l.ContractorID, Network: b.Network})
		if err != nil || l.WalletAddress == nil || w.Address != *l.WalletAddress || s.now().Before(w.PayoutsAllowedFrom) {
			return fmt.Errorf("%w: line %d: the contractor's wallet changed after the run was checked; build a new run", ErrNotReady, l.LineNo)
		}
	}
	if lim, ok := s.Limits[safety.Network(b.Network)]; ok && lim != nil {
		_, room, err := lim.Limits(ctx)
		if err != nil {
			s.log().Warn("batch: could not read the vault's daily room; the chain will enforce it", "err", err)
		} else if b.Total.Cmp(room) > 0 {
			return fmt.Errorf("%w (%s USDC left today)", ErrOverDailyLimit, room.Format(money.USDC.Scale))
		}
	}
	return nil
}

// reserve moves the run's total from the organisation's available balance
// to the run's in-flight account. The ledger refuses an overdraft, so funds
// are never promised twice.
func (s *Service) reserve(ctx context.Context, tx pgx.Tx, b store.Batch) error {
	asset := money.USDC.Code
	orgID := b.OrgID
	avail, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: orgAccount(orgID, "available"), Asset: asset, Kind: ledger.KindLiability, Rule: ledger.NonPositive})
	if err != nil {
		return err
	}
	inflight, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: batchAccount(b.ID), Asset: asset, Kind: ledger.KindLiability, Rule: ledger.NonPositive})
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.Entry{
		IdempotencyKey: "batch:" + b.ID.String() + ":reserved",
		Kind:           "batch.reserved",
		RefType:        "batch",
		RefID:          b.ID.String(),
		Postings: []ledger.Posting{
			{Account: avail.ID, Asset: asset, Amount: b.Total},
			{Account: inflight.ID, Asset: asset, Amount: b.Total.Neg()},
		},
	})
	if errors.Is(err, ledger.ErrInsufficientFunds) {
		return ErrNoFunds
	}
	return err
}
