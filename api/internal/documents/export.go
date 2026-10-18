package documents

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// Owner is whose accounts an export covers: exactly one of the two is set.
type Owner struct {
	ContractorID uuid.UUID
	OrgID        uuid.UUID
}

// accounts are the owner's available-balance accounts, one per asset: the
// money that is theirs. Pending deposits appear when they become final,
// and a run's reservation is money out (its return, if it fails, money
// in).
func (o Owner) accounts() map[string]money.Asset {
	if o.OrgID != uuid.Nil {
		return map[string]money.Asset{"liability:org:" + o.OrgID.String() + ":USDC:available": money.USDC}
	}
	id := o.ContractorID.String()
	return map[string]money.Asset{
		"liability:contractor:" + id + ":USDC:available": money.USDC,
		"liability:contractor:" + id + ":NGN:available":  money.NGN,
	}
}

// MaxExportDays is the longest range one export covers.
const MaxExportDays = 366

// ExportHeader is the CSV header row.
var ExportHeader = []string{"date", "waybill_reference", "description", "asset", "money_in", "money_out", "balance", "chain_tx", "provider_reference"}

// Export writes the owner's postings in [from, to) as CSV: one row per
// posting, oldest first, with money in and money out in separate columns
// and the balance after each row.
func (s *Service) Export(ctx context.Context, o Owner, from, to time.Time, w io.Writer) error {
	if !from.Before(to) || to.Sub(from) > MaxExportDays*24*time.Hour {
		return ErrInvalidRange
	}
	accts := o.accounts()
	codes := make([]string, 0, len(accts))
	for c := range accts {
		codes = append(codes, c)
	}
	q := store.New(s.Pool)
	opening, err := q.SumPostingsBefore(ctx, store.SumPostingsBeforeParams{Codes: codes, FromTime: from})
	if err != nil {
		return err
	}
	// Balances are what the caller is owed: the liability's balance negated.
	balance := map[string]money.Amount{}
	for _, r := range opening {
		balance[r.AccountCode] = r.Total.Neg()
	}
	rows, err := q.ListExportPostings(ctx, store.ListExportPostingsParams{Codes: codes, FromTime: from, ToTime: to})
	if err != nil {
		return err
	}
	refs, err := s.lookupRefs(ctx, rows)
	if err != nil {
		return err
	}

	cw := csv.NewWriter(w)
	if err := cw.Write(ExportHeader); err != nil {
		return err
	}
	for _, r := range rows {
		asset := accts[r.AccountCode]
		in, out := "", ""
		if r.Amount.Sign() < 0 { // a credit to a liability: more is owed to the caller
			in = r.Amount.Neg().Format(asset.Scale)
		} else {
			out = r.Amount.Format(asset.Scale)
		}
		bal := balance[r.AccountCode].Sub(r.Amount)
		balance[r.AccountCode] = bal
		ref := refs[r.RefType+":"+r.RefID]
		rec := []string{
			r.CreatedAt.UTC().Format(time.RFC3339),
			ref.waybill,
			describe(r.Kind, asset),
			string(asset.Code),
			in, out,
			bal.Format(asset.Scale),
			ref.chainTx,
			ref.provider,
		}
		for i := range rec {
			rec[i] = cell(rec[i])
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// cell neutralises a value a spreadsheet would run as a formula.
func cell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func describe(kind string, asset money.Asset) string {
	base, reversed := strings.CutSuffix(kind, ".reversal")
	var d string
	switch base {
	case "deposit.final":
		d = "Payment received"
	case "checkout.paid":
		d = "Card or bank transfer received"
	case "payout.converted", "conversion.accepted":
		if asset.Code == money.NGN.Code {
			d = "Converted from USDC"
		} else {
			d = "Converted to naira"
		}
	case "payout.sent":
		d = "Sent to bank"
	case "batch.reserved":
		d = "Payment run"
	default:
		d = base
	}
	if reversed {
		if base == "batch.reserved" {
			return "Payment run failed; money returned"
		}
		return "Reversed: " + d
	}
	return d
}

type refInfo struct{ waybill, chainTx, provider string }

// lookupRefs finds, for each entry's reference, the Waybill reference a
// person recognises and the chain or provider reference behind it.
func (s *Service) lookupRefs(ctx context.Context, rows []store.ListExportPostingsRow) (map[string]refInfo, error) {
	ids := map[string][]uuid.UUID{}
	for _, r := range rows {
		if id, err := uuid.Parse(r.RefID); err == nil {
			ids[r.RefType] = append(ids[r.RefType], id)
		}
	}
	out := map[string]refInfo{}
	q := store.New(s.Pool)
	code := func(c *string, fallback string) string {
		if c != nil {
			return *c
		}
		return fallback
	}
	if len(ids["payment"]) > 0 {
		rs, err := q.PaymentRefs(ctx, ids["payment"])
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out["payment:"+r.ID.String()] = refInfo{waybill: code(r.TrackingCode, "funding"), chainTx: r.TxHash}
		}
	}
	if len(ids["payout"]) > 0 {
		rs, err := q.PayoutRefs(ctx, ids["payout"])
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out["payout:"+r.ID.String()] = refInfo{waybill: code(r.TrackingCode, "conversion"), provider: r.Reference}
		}
	}
	if len(ids["checkout"]) > 0 {
		rs, err := q.CheckoutRefs(ctx, ids["checkout"])
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out["checkout:"+r.ID.String()] = refInfo{waybill: r.TrackingCode, provider: r.Reference}
		}
	}
	if len(ids["batch"]) > 0 {
		rs, err := q.BatchRefs(ctx, ids["batch"])
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out["batch:"+r.ID.String()] = refInfo{waybill: "run " + r.ID.String(), chainTx: code(r.MinedHash, "")}
		}
	}
	return out, nil
}

// ParseRange reads an export's from and to dates (YYYY-MM-DD, UTC; to is
// exclusive).
func ParseRange(from, to string) (time.Time, time.Time, error) {
	f, err1 := time.Parse(time.DateOnly, from)
	t, err2 := time.Parse(time.DateOnly, to)
	if err1 != nil || err2 != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: dates are YYYY-MM-DD", ErrInvalidRange)
	}
	return f, t, nil
}
