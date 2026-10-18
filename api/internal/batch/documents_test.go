package batch_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/batch"
	"github.com/JoshBlazer/waybill/api/internal/documents"
	"github.com/JoshBlazer/waybill/api/internal/pdf"
)

// After a dual-approval run is paid: its receipt names who created,
// approved and sent it, lists every line, and its total is what left the
// vault in the ledger; the organisation's export shows the funding in, the
// run out, and closes at the ledger's balance.
func TestDocuments_RunReceiptAndOrganisationExport(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-time.Minute)
	r.fundAndSweep(t, 1000)
	run := r.create(t,
		batch.LineInput{ContractorID: r.contractors[0], Amount: "400", Reference: "October retainer"},
		batch.LineInput{ContractorID: r.contractors[1], Amount: "250.5", Reference: "October retainer"},
	)
	svc := &documents.Service{Pool: r.pool}
	if _, err := svc.BatchReceipt(ctx, r.orgID, run.ID); !errors.Is(err, documents.ErrNotSettled) {
		t.Fatalf("receipt before payment: %v, want ErrNotSettled", err)
	}
	if _, err := r.svc.Approve(ctx, r.approver, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Send(ctx, r.maker, run.ID); err != nil {
		t.Fatal(err)
	}
	r.drive(t, func() bool { return r.state(t, run.ID) == "paid" })

	rec, err := svc.BatchReceipt(ctx, r.orgID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paidOut string
	if err := r.pool.QueryRow(ctx, `SELECT coalesce(sum(p.amount), 0)::text FROM postings p
		JOIN journal_entries e ON e.id = p.entry_id JOIN ledger_accounts a ON a.id = p.account_id
		WHERE e.kind = 'batch.paid' AND e.ref_id = $1 AND a.code = 'custody:vault:evm_31337:USDC'`, run.ID.String()).Scan(&paidOut); err != nil {
		t.Fatal(err)
	}
	if rec.Total.String() != "650500000" || paidOut != "-650500000" {
		t.Fatalf("receipt total %s; the ledger moved %s out of the vault", rec.Total, paidOut)
	}
	if rec.CreatedBy != "Tolu Bakare" || rec.SentBy != "Tolu Bakare" || len(rec.ApprovedBy) != 1 || rec.ApprovedBy[0] != "Chidi Eze" {
		t.Fatalf("people on the receipt: created %q, approved %v, sent %q", rec.CreatedBy, rec.ApprovedBy, rec.SentBy)
	}
	if len(rec.Lines) != 2 || !strings.HasPrefix(rec.TxHash, "0x") || rec.PaidAt.IsZero() {
		t.Fatalf("receipt %+v", rec)
	}
	doc := rec.PDF()
	if err := pdf.Check(doc); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(pdf.TextLines(doc), "\n")
	for _, want := range []string{"Payment run receipt", "Lagoon Labs", "Chidi Eze", "USDC 400.00", "USDC 250.50", "USDC 650.50", rec.TxHash[:20]} {
		if !strings.Contains(text, want) {
			t.Errorf("run receipt PDF lacks %q", want)
		}
	}
	// Another organisation's member cannot see it.
	if _, err := svc.BatchReceipt(ctx, [16]byte{9}, run.ID); !errors.Is(err, documents.ErrNotFound) {
		t.Fatalf("another organisation: %v, want ErrNotFound", err)
	}

	var buf bytes.Buffer
	if err := svc.Export(ctx, documents.Owner{OrgID: r.orgID}, start, time.Now().UTC().Add(time.Minute), &buf); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("export rows %v; want the header, the funding and the run", rows)
	}
	in, out := rows[1], rows[2]
	if in[2] != "Payment received" || in[1] != "funding" || in[4] != "1000.000000" || !strings.HasPrefix(in[7], "0x") {
		t.Fatalf("funding row %v", in)
	}
	if out[2] != "Payment run" || out[1] != "run "+run.ID.String() || out[5] != "650.500000" || out[6] != "349.500000" || out[7] != rec.TxHash {
		t.Fatalf("run row %v", out)
	}
	f, _ := r.orgs.Funding(ctx, r.orgID)
	if f.Available.String() != "349500000" {
		t.Fatalf("ledger available %s; the export closes at 349.5", f.Available)
	}
}
