package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/documents"
	"github.com/JoshBlazer/waybill/api/internal/money"
)

// Invoices, receipts and the accountant export (docs/design/exports.md,
// ADR-044).

func (s *Server) documents() *documents.Service {
	return &documents.Service{Pool: s.Pool, Tokens: s.Tokens, WebURL: s.WebURL}
}

var (
	errNoDocument  = problem(http.StatusNotFound, "not-found", "No such document.", "")
	errNotSettled  = problem(http.StatusConflict, "not-settled", "There is no receipt yet: the payment is not complete.", "")
	errExportRange = problem(http.StatusUnprocessableEntity, "validation", "Choose a range of 1 to 366 days: from is included, to is not.", "")
)

func attachment(name string) *string {
	v := `attachment; filename="` + name + `"`
	return &v
}

func amount(a money.Amount, asset money.Asset) Amount {
	return Amount{Asset: string(asset.Code), Minor: a.String(), Scale: int(asset.Scale)}
}

// GetInvoiceDocument returns the invoice as a PDF.
func (s *Server) GetInvoiceDocument(ctx context.Context, req GetInvoiceDocumentRequestObject) (GetInvoiceDocumentResponseObject, error) {
	c, _ := auth.ContractorFrom(ctx)
	inv, err := s.documents().Invoice(ctx, c, req.Id)
	if errors.Is(err, documents.ErrNotFound) {
		return GetInvoiceDocument404ApplicationProblemPlusJSONResponse(errNoDocument), nil
	}
	if err != nil {
		return nil, err
	}
	doc := documents.InvoicePDF(inv, s.WebURL+"/pay/"+inv.TrackingCode)
	return GetInvoiceDocument200ApplicationpdfResponse{
		Body: bytes.NewReader(doc), ContentLength: int64(len(doc)),
		Headers: GetInvoiceDocument200ResponseHeaders{ContentDisposition: attachment("invoice-" + inv.TrackingCode + ".pdf")},
	}, nil
}

func (s *Server) invoiceReceipt(ctx context.Context, id [16]byte) (documents.InvoiceReceipt, *Problem, error) {
	c, _ := auth.ContractorFrom(ctx)
	r, err := s.documents().InvoiceReceipt(ctx, c, id)
	switch {
	case errors.Is(err, documents.ErrNotFound):
		return r, &errNoDocument, nil
	case errors.Is(err, documents.ErrNotSettled):
		return r, &errNotSettled, nil
	}
	return r, nil, err
}

// GetInvoiceReceipt returns the receipt for a settled invoice.
func (s *Server) GetInvoiceReceipt(ctx context.Context, req GetInvoiceReceiptRequestObject) (GetInvoiceReceiptResponseObject, error) {
	r, pr, err := s.invoiceReceipt(ctx, req.Id)
	switch {
	case err != nil:
		return nil, err
	case pr != nil && pr.Status == http.StatusNotFound:
		return GetInvoiceReceipt404ApplicationProblemPlusJSONResponse(*pr), nil
	case pr != nil:
		return GetInvoiceReceipt409ApplicationProblemPlusJSONResponse(*pr), nil
	}
	return GetInvoiceReceipt200JSONResponse(invoiceReceiptJSON(r)), nil
}

// GetInvoiceReceiptPdf returns the receipt for a settled invoice as a PDF.
func (s *Server) GetInvoiceReceiptPdf(ctx context.Context, req GetInvoiceReceiptPdfRequestObject) (GetInvoiceReceiptPdfResponseObject, error) {
	r, pr, err := s.invoiceReceipt(ctx, req.Id)
	switch {
	case err != nil:
		return nil, err
	case pr != nil && pr.Status == http.StatusNotFound:
		return GetInvoiceReceiptPdf404ApplicationProblemPlusJSONResponse(*pr), nil
	case pr != nil:
		return GetInvoiceReceiptPdf409ApplicationProblemPlusJSONResponse(*pr), nil
	}
	doc := r.PDF()
	return GetInvoiceReceiptPdf200ApplicationpdfResponse{
		Body: bytes.NewReader(doc), ContentLength: int64(len(doc)),
		Headers: GetInvoiceReceiptPdf200ResponseHeaders{ContentDisposition: attachment("receipt-" + r.TrackingCode + ".pdf")},
	}, nil
}

func invoiceReceiptJSON(r documents.InvoiceReceipt) InvoiceReceipt {
	out := InvoiceReceipt{
		InvoiceId: r.InvoiceID, TrackingCode: r.TrackingCode, ContractorName: r.ContractorName, Description: r.Description,
		Amount: amount(r.Amount, r.Asset), Received: amount(r.Received, r.Asset), SettledAt: r.SettledAt,
		Payments: make([]ReceiptPayment, 0, len(r.Payments)),
	}
	for _, p := range r.Payments {
		out.Payments = append(out.Payments, ReceiptPayment{Network: p.Network, TxHash: p.TxHash, Amount: amount(p.Amount, r.Asset), FinalAt: p.FinalAt})
	}
	if c := r.Card; c != nil {
		out.Card = &ReceiptCard{Reference: c.Reference, Amount: amount(c.Amount, money.NGN), PaidAt: c.PaidAt}
	}
	if p := r.Payout; p != nil {
		rp := &ReceiptPayout{State: p.State, Naira: amount(p.Naira, money.NGN), Reference: p.Reference, SentAt: p.SentAt}
		if p.Rate != nil {
			rate := amount(*p.Rate, money.NGN)
			rp.Rate = &rate
		}
		out.Payout = rp
	}
	return out
}

func (s *Server) batchReceipt(ctx context.Context, id [16]byte) (documents.BatchReceipt, *Problem, error) {
	m, _ := auth.MemberFrom(ctx)
	r, err := s.documents().BatchReceipt(ctx, m.OrgID, id)
	switch {
	case errors.Is(err, documents.ErrNotFound):
		return r, &errNoDocument, nil
	case errors.Is(err, documents.ErrNotSettled):
		return r, &errNotSettled, nil
	}
	return r, nil, err
}

// GetBatchReceipt returns the receipt for a paid run.
func (s *Server) GetBatchReceipt(ctx context.Context, req GetBatchReceiptRequestObject) (GetBatchReceiptResponseObject, error) {
	r, pr, err := s.batchReceipt(ctx, req.Id)
	switch {
	case err != nil:
		return nil, err
	case pr != nil && pr.Status == http.StatusNotFound:
		return GetBatchReceipt404ApplicationProblemPlusJSONResponse(*pr), nil
	case pr != nil:
		return GetBatchReceipt409ApplicationProblemPlusJSONResponse(*pr), nil
	}
	out := BatchReceipt{
		Id: r.ID, Organisation: r.OrgName, Network: r.Network, Total: amount(r.Total, r.Asset), TxHash: r.TxHash, PaidAt: r.PaidAt,
		CreatedBy: r.CreatedBy, ApprovedBy: append([]string{}, r.ApprovedBy...), Lines: make([]BatchReceiptLine, 0, len(r.Lines)),
	}
	if r.SentBy != "" {
		out.SentBy = &r.SentBy
	}
	for _, l := range r.Lines {
		out.Lines = append(out.Lines, BatchReceiptLine{LineNo: int(l.No), ContractorName: l.ContractorName, Wallet: l.Wallet,
			Amount: amount(l.Amount, r.Asset), Reference: l.Reference, TrackingCode: l.TrackingCode})
	}
	return GetBatchReceipt200JSONResponse(out), nil
}

// GetBatchReceiptPdf returns the receipt for a paid run as a PDF.
func (s *Server) GetBatchReceiptPdf(ctx context.Context, req GetBatchReceiptPdfRequestObject) (GetBatchReceiptPdfResponseObject, error) {
	r, pr, err := s.batchReceipt(ctx, req.Id)
	switch {
	case err != nil:
		return nil, err
	case pr != nil && pr.Status == http.StatusNotFound:
		return GetBatchReceiptPdf404ApplicationProblemPlusJSONResponse(*pr), nil
	case pr != nil:
		return GetBatchReceiptPdf409ApplicationProblemPlusJSONResponse(*pr), nil
	}
	doc := r.PDF()
	return GetBatchReceiptPdf200ApplicationpdfResponse{
		Body: bytes.NewReader(doc), ContentLength: int64(len(doc)),
		Headers: GetBatchReceiptPdf200ResponseHeaders{ContentDisposition: attachment("run-" + r.ID.String() + ".pdf")},
	}, nil
}

// ExportLedger returns the caller's postings in a date range as CSV.
func (s *Server) ExportLedger(ctx context.Context, req ExportLedgerRequestObject) (ExportLedgerResponseObject, error) {
	var owner documents.Owner
	if m, ok := auth.MemberFrom(ctx); ok {
		owner.OrgID = m.OrgID
	} else {
		owner.ContractorID, _ = auth.ContractorFrom(ctx)
	}
	from, to := req.Params.From.Time, req.Params.To.Time
	var buf bytes.Buffer
	err := s.documents().Export(ctx, owner, from, to, &buf)
	if errors.Is(err, documents.ErrInvalidRange) {
		return ExportLedger422ApplicationProblemPlusJSONResponse(errExportRange), nil
	}
	if err != nil {
		return nil, err
	}
	name := "waybill-" + from.Format(time.DateOnly) + "-to-" + to.Format(time.DateOnly) + ".csv"
	return ExportLedger200TextcsvResponse{
		Body: &buf, ContentLength: int64(buf.Len()),
		Headers: ExportLedger200ResponseHeaders{ContentDisposition: attachment(name)},
	}, nil
}
