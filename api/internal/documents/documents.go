// Package documents builds invoices, receipts and the accountant export
// (docs/design/exports.md). Every figure comes from the ledger and the
// event tables at the time of the request, never from a cached copy, so a
// receipt and the export cannot disagree.
package documents

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// Errors.
var (
	ErrNotFound     = errors.New("documents: not found")
	ErrNotSettled   = errors.New("documents: not settled yet")
	ErrInvalidRange = errors.New("documents: invalid date range")
)

// Service builds documents.
type Service struct {
	Pool   *pgxpool.Pool
	Tokens map[safety.Network]string // token contract per network, for invoice.Load
	WebURL string                    // for payment links on invoices
}

// PaymentLine is one final on-chain payment.
type PaymentLine struct {
	Network string
	TxHash  string
	Amount  money.Amount
	FinalAt time.Time
}

// CardLine is a card or bank-transfer payment through the provider.
type CardLine struct {
	Reference string
	Amount    money.Amount // kobo
	PaidAt    time.Time
}

// PayoutLine is the naira sent for an invoice.
type PayoutLine struct {
	State     string
	Naira     money.Amount // kobo
	Rate      *money.Amount
	Reference string
	SentAt    *time.Time
}

// InvoiceReceipt is what arrived for a settled invoice and what was sent on.
type InvoiceReceipt struct {
	InvoiceID      uuid.UUID
	TrackingCode   string
	ContractorName string
	Description    string
	Asset          money.Asset
	Amount         money.Amount // invoiced
	Received       money.Amount // sum of final on-chain payments, in Asset
	SettledAt      time.Time
	Payments       []PaymentLine
	Card           *CardLine
	Payout         *PayoutLine
}

// Invoice returns one of the contractor's invoices.
func (s *Service) Invoice(ctx context.Context, contractor, id uuid.UUID) (invoice.Invoice, error) {
	inv, err := invoice.LoadForContractor(ctx, s.Pool, s.Tokens, id, contractor)
	if errors.Is(err, invoice.ErrNotFound) {
		return invoice.Invoice{}, ErrNotFound
	}
	return inv, err
}

// InvoiceReceipt returns the receipt for one of the contractor's invoices,
// or ErrNotSettled before it is settled.
func (s *Service) InvoiceReceipt(ctx context.Context, contractor, id uuid.UUID) (InvoiceReceipt, error) {
	inv, err := s.Invoice(ctx, contractor, id)
	if err != nil {
		return InvoiceReceipt{}, err
	}
	if inv.State != "settled" {
		return InvoiceReceipt{}, ErrNotSettled
	}
	q := store.New(s.Pool)
	r := InvoiceReceipt{
		InvoiceID: inv.ID, TrackingCode: inv.TrackingCode, ContractorName: inv.ContractorName,
		Description: inv.Description, Asset: inv.Asset, Amount: inv.Amount, Received: money.Zero,
	}
	if r.SettledAt, err = q.InvoiceSettledAt(ctx, inv.ID); err != nil {
		return InvoiceReceipt{}, err
	}
	pays, err := q.ListFinalPaymentsForInvoice(ctx, &inv.ID)
	if err != nil {
		return InvoiceReceipt{}, err
	}
	for _, p := range pays {
		line := PaymentLine{Network: p.Network, TxHash: p.TxHash, Amount: p.Amount}
		if p.FinalAt != nil {
			line.FinalAt = *p.FinalAt
		}
		r.Payments = append(r.Payments, line)
		r.Received = r.Received.Add(p.Amount)
	}
	switch c, err := q.GetPaidCheckoutForInvoice(ctx, inv.ID); {
	case err == nil:
		r.Card = &CardLine{Reference: c.Reference, Amount: c.PaidAmount}
		if c.PaidAt != nil {
			r.Card.PaidAt = *c.PaidAt
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return InvoiceReceipt{}, err
	}
	if p := inv.Payout; p != nil {
		line := &PayoutLine{State: p.State, Naira: p.NairaAmount, Reference: p.Reference}
		if p.RateQuoteID != nil {
			qt, err := q.GetRateQuote(ctx, *p.RateQuoteID)
			if err != nil {
				return InvoiceReceipt{}, err
			}
			line.Rate = &qt.Rate
		}
		for _, e := range inv.PayoutEvents {
			if e.ToState == "succeeded" {
				at := e.CreatedAt
				line.SentAt = &at
			}
		}
		r.Payout = line
	}
	return r, nil
}

// BatchReceiptLine is one paid line of a run.
type BatchReceiptLine struct {
	No             int32
	ContractorName string
	Wallet         string
	Amount         money.Amount
	Reference      string
	TrackingCode   string
}

// BatchReceipt is the receipt for a paid run.
type BatchReceipt struct {
	ID         uuid.UUID
	OrgName    string
	Network    string
	Asset      money.Asset
	Total      money.Amount
	TxHash     string
	PaidAt     time.Time
	CreatedBy  string
	ApprovedBy []string
	SentBy     string
	Lines      []BatchReceiptLine
}

// BatchReceipt returns the receipt for one of the organisation's runs, or
// ErrNotSettled before it is paid.
func (s *Service) BatchReceipt(ctx context.Context, orgID, id uuid.UUID) (BatchReceipt, error) {
	q := store.New(s.Pool)
	b, err := q.GetBatch(ctx, store.GetBatchParams{ID: id, OrgID: orgID})
	if errors.Is(err, pgx.ErrNoRows) {
		return BatchReceipt{}, ErrNotFound
	}
	if err != nil {
		return BatchReceipt{}, err
	}
	if b.State != "paid" {
		return BatchReceipt{}, ErrNotSettled
	}
	o, err := q.GetOrg(ctx, orgID)
	if err != nil {
		return BatchReceipt{}, err
	}
	asset, _ := money.LookupAsset(money.AssetCode(b.AssetCode))
	r := BatchReceipt{ID: b.ID, OrgName: o.Name, Network: b.Network, Asset: asset, Total: b.Total}
	if b.OutboundTxID != nil {
		t, err := q.GetOutboundTx(ctx, *b.OutboundTxID)
		if err != nil {
			return BatchReceipt{}, err
		}
		if t.MinedHash != nil {
			r.TxHash = *t.MinedHash
		}
	}
	name := func(id uuid.UUID) (string, error) {
		m, err := q.GetMember(ctx, store.GetMemberParams{ID: id, OrgID: orgID})
		if err != nil {
			return "", fmt.Errorf("member %s: %w", id, err)
		}
		return m.Name, nil
	}
	if r.CreatedBy, err = name(b.CreatedBy); err != nil {
		return BatchReceipt{}, err
	}
	approvals, err := q.ListApprovals(ctx, b.ID)
	if err != nil {
		return BatchReceipt{}, err
	}
	for _, a := range approvals {
		if a.Decision != "approved" {
			continue
		}
		n, err := name(a.ApproverID)
		if err != nil {
			return BatchReceipt{}, err
		}
		r.ApprovedBy = append(r.ApprovedBy, n)
	}
	events, err := q.ListBatchEvents(ctx, b.ID)
	if err != nil {
		return BatchReceipt{}, err
	}
	for _, e := range events {
		switch {
		case e.ToState == "sending" && e.MemberID != nil:
			if r.SentBy, err = name(*e.MemberID); err != nil {
				return BatchReceipt{}, err
			}
		case e.ToState == "paid":
			r.PaidAt = e.CreatedAt
		}
	}
	lines, err := q.ListBatchLines(ctx, b.ID)
	if err != nil {
		return BatchReceipt{}, err
	}
	for _, l := range lines {
		line := BatchReceiptLine{No: l.LineNo, ContractorName: l.ContractorName, Amount: l.Amount, Reference: l.Reference, TrackingCode: l.TrackingCode}
		if l.WalletAddress != nil {
			line.Wallet = *l.WalletAddress
		}
		r.Lines = append(r.Lines, line)
	}
	return r, nil
}
