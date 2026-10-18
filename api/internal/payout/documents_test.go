package payout_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/documents"
	"github.com/JoshBlazer/waybill/api/internal/pdf"
	"github.com/JoshBlazer/waybill/api/internal/safety"
)

func docs(r *rig) *documents.Service {
	return &documents.Service{Pool: r.pool, Tokens: map[safety.Network]string{"evm:31337": strings.ToLower(deployment.Token.Hex())}, WebURL: "http://web.test"}
}

// The receipt of a settled naira invoice shows what arrived, the rate, the
// naira sent and the transfer reference, and its figures are the ledger's.
func TestDocuments_InvoiceReceiptMatchesTheLedger(t *testing.T) {
	r := sentPayout(t)
	ctx := context.Background()
	rec, err := docs(r).InvoiceReceipt(ctx, r.contractor, r.inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Received.String() != "100000000" || len(rec.Payments) != 1 {
		t.Fatalf("received %s in %d payments; want 100 USDC in one", rec.Received, len(rec.Payments))
	}
	// The ledger agrees: the USDC sold for naira is exactly what arrived.
	if fx := r.balance(t, "equity:fx-position:USDC"); fx != "-"+rec.Received.String() {
		t.Fatalf("FX position %s; the receipt says %s arrived", fx, rec.Received)
	}
	p := rec.Payout
	if p == nil || p.Naira.String() != "15500000" || p.State != "succeeded" || p.Reference != r.payout(t).Reference || p.Rate == nil || p.SentAt == nil {
		t.Fatalf("payout on the receipt: %+v", p)
	}

	doc := rec.PDF()
	if err := pdf.Check(doc); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(pdf.TextLines(doc), "\n")
	for _, want := range []string{"Receipt", r.inv.TrackingCode, "USDC 100.00", "NGN 155,000.00", "Test money only. Nothing here is real.", p.Reference} {
		if !strings.Contains(text, want) {
			t.Errorf("receipt PDF lacks %q", want)
		}
	}
}

// An invoice that is not settled has no receipt; another contractor's
// invoice does not exist for this one.
func TestDocuments_NoReceiptBeforeSettlementOrForOthers(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	if _, err := docs(r).InvoiceReceipt(ctx, r.contractor, r.inv.ID); !errors.Is(err, documents.ErrNotSettled) {
		t.Fatalf("open invoice: %v, want ErrNotSettled", err)
	}
	if _, err := docs(r).InvoiceReceipt(ctx, [16]byte{1}, r.inv.ID); !errors.Is(err, documents.ErrNotFound) {
		t.Fatalf("another contractor: %v, want ErrNotFound", err)
	}
}

// The contractor's export: one row per posting on their available
// balances, in and out in separate columns, and a closing balance equal to
// the ledger's.
func TestDocuments_ContractorExport(t *testing.T) {
	r := sentPayout(t)
	var buf bytes.Buffer
	from := time.Now().UTC().Add(-24 * time.Hour)
	if err := docs(r).Export(context.Background(), documents.Owner{ContractorID: r.contractor}, from, from.Add(48*time.Hour), &buf); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rows[0], ",") != strings.Join(documents.ExportHeader, ",") {
		t.Fatalf("header %v", rows[0])
	}
	type row struct{ desc, asset, in, out, bal, provider string }
	var got []row
	for _, x := range rows[1:] {
		got = append(got, row{x[2], x[3], x[4], x[5], x[6], x[8]})
	}
	ref := r.payout(t).Reference
	want := []row{
		{"test.paid", "USDC", "100.000000", "", "100.000000", ""},
		{"Converted to naira", "USDC", "", "100.000000", "0.000000", ref},
		{"Converted from USDC", "NGN", "155000.00", "", "155000.00", ref},
		{"Sent to bank", "NGN", "", "155000.00", "0.00", ref},
	}
	if len(got) != len(want) {
		t.Fatalf("rows %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i+1, got[i], want[i])
		}
	}
	// Closing balances equal the ledger's.
	if r.balance(t, "liability:contractor:"+r.contractor.String()+":NGN:available") != "0" {
		t.Fatal("ledger NGN balance is not 0")
	}

	// A range starting after everything has no rows but the header, and a
	// range of more than 366 days is refused.
	buf.Reset()
	later := time.Now().UTC().Add(time.Hour)
	if err := docs(r).Export(context.Background(), documents.Owner{ContractorID: r.contractor}, later, later.Add(time.Hour), &buf); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Fatalf("%d lines for an empty range; want the header only", n)
	}
	if err := docs(r).Export(context.Background(), documents.Owner{ContractorID: r.contractor}, from, from.Add(367*24*time.Hour), &buf); !errors.Is(err, documents.ErrInvalidRange) {
		t.Fatalf("367 days: %v, want ErrInvalidRange", err)
	}
}
