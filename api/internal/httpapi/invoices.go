package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// CreateInvoice creates an invoice. The idempotency key is claimed in the
// same transaction as the invoice (invariant I6): a concurrent request with
// the same key waits for this one to commit and then gets its response; if
// this one rolls back, the key is free again.
func (s *Server) CreateInvoice(ctx context.Context, req CreateInvoiceRequestObject) (CreateInvoiceResponseObject, error) {
	contractor, ok := auth.ContractorFrom(ctx)
	if !ok {
		return nil, errors.New("createInvoice: no authenticated contractor") // middleware bug
	}
	if req.Body == nil {
		return CreateInvoice400ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(
			problem(400, "bad-request", "A request body is required.", ""))}, nil
	}
	key := req.Params.IdempotencyKey
	if len(key) < 1 || len(key) > 255 {
		return CreateInvoice400ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(
			problem(400, "bad-request", "Idempotency-Key must be 1 to 255 characters.", ""))}, nil
	}
	canonical, err := json.Marshal(req.Body) // struct field order makes this canonical
	if err != nil {
		return nil, err
	}
	reqHash := sha256.Sum256(canonical)
	principal := "contractor:" + contractor.String()

	var (
		created *Invoice
		replay  *store.IdempotencyKey
	)
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		_, err := q.ClaimIdempotencyKey(ctx, store.ClaimIdempotencyKeyParams{Principal: principal, Key: key, RequestHash: reqHash[:]})
		if errors.Is(err, pgx.ErrNoRows) {
			rec, err := q.GetIdempotencyRecord(ctx, store.GetIdempotencyRecordParams{Principal: principal, Key: key})
			if err != nil {
				return err
			}
			replay = &rec
			return nil
		}
		if err != nil {
			return err
		}
		inv, err := s.Invoices.Create(ctx, tx, contractor, invoice.Input{
			Description: req.Body.Description,
			Amount:      req.Body.Amount,
			Asset:       money.AssetCode(req.Body.Asset),
			Payout:      string(req.Body.Payout),
		})
		if err != nil {
			return err
		}
		out := s.invoiceJSON(inv)
		body, err := json.Marshal(out)
		if err != nil {
			return err
		}
		status := int32(http.StatusCreated)
		if err := q.CompleteIdempotencyKey(ctx, store.CompleteIdempotencyKeyParams{
			Principal: principal, Key: key, StatusCode: &status, ResponseBody: body,
		}); err != nil {
			return err
		}
		created = &out
		return nil
	})

	switch {
	case errors.Is(err, invoice.ErrInvalidAmount), errors.Is(err, invoice.ErrInvalidDescription),
		errors.Is(err, invoice.ErrInvalidPayout), errors.Is(err, invoice.ErrUnsupportedAsset),
		errors.Is(err, invoice.ErrBTCHoldOnly):
		return CreateInvoice422ApplicationProblemPlusJSONResponse(problem(422, "validation", "The invoice is not valid.", err.Error())), nil
	case errors.Is(err, invoice.ErrNoNetworks):
		return CreateInvoice422ApplicationProblemPlusJSONResponse(problem(422, "no-network",
			"No network for this asset is configured on this server.", "BTC invoices need a Bitcoin network (ADR-045).")), nil
	case errors.Is(err, invoice.ErrNoBankAccount):
		return CreateInvoice422ApplicationProblemPlusJSONResponse(problem(422, "no-bank-account",
			"Add a verified bank account before asking for naira.", "PUT /v1/me/bank-account, or choose \"hold\".")), nil
	case errors.Is(err, invoice.ErrRateUnavailable):
		s.logger().WarnContext(ctx, "create invoice: no naira rate", "err", err)
		return CreateInvoice503ApplicationProblemPlusJSONResponse(problem(503, "rate-unavailable",
			"We can't get a naira rate right now, so no invoice was created.", "Try again in a few minutes, or choose \"hold\".")), nil
	case err != nil:
		return nil, err
	}

	if replay != nil {
		if !bytes.Equal(replay.RequestHash, reqHash[:]) {
			return CreateInvoice422ApplicationProblemPlusJSONResponse(problem(422, "idempotency-key-reused",
				"This Idempotency-Key was already used with a different request.", "")), nil
		}
		var out Invoice
		if err := json.Unmarshal(replay.ResponseBody, &out); err != nil {
			return nil, fmt.Errorf("createInvoice: stored response: %w", err)
		}
		return CreateInvoice201JSONResponse(out), nil
	}
	return CreateInvoice201JSONResponse(*created), nil
}

// GetPayment returns what a payer sees on a payment link.
func (s *Server) GetPayment(ctx context.Context, req GetPaymentRequestObject) (GetPaymentResponseObject, error) {
	inv, err := invoice.Load(ctx, s.Pool, s.Tokens, req.Code)
	if errors.Is(err, invoice.ErrNotFound) {
		return GetPayment404ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(notFound())}, nil
	}
	if err != nil {
		return nil, err
	}
	return GetPayment200JSONResponse(PaymentLink{
		TrackingCode:     inv.TrackingCode,
		ContractorName:   inv.ContractorName,
		Verified:         inv.Verified,
		Description:      inv.Description,
		Amount:           amountJSON(inv),
		DepositAddresses: depositJSON(inv),
		State:            InvoiceState(inv.State),
		ExpiresAt:        inv.ExpiresAt,
		Naira:            s.payableInNaira(inv),
	}), nil
}

// payableInNaira is the naira a payer can pay by card or bank transfer, or
// nil when startCheckout would refuse.
func (s *Server) payableInNaira(inv invoice.Invoice) *PaymentLinkNaira {
	if s.Checkout == nil || s.Checkout.Payable(inv) != nil {
		return nil
	}
	return &PaymentLinkNaira{
		Amount:    Amount{Asset: string(money.NGN.Code), Minor: inv.NairaAmount().String(), Scale: int(money.NGN.Scale)},
		ExpiresAt: inv.Quote.ExpiresAt.UTC(),
	}
}

// GetTracking returns the four-step tracking view.
func (s *Server) GetTracking(ctx context.Context, req GetTrackingRequestObject) (GetTrackingResponseObject, error) {
	inv, err := invoice.Load(ctx, s.Pool, s.Tokens, req.Code)
	if errors.Is(err, invoice.ErrNotFound) {
		// Not an invoice: perhaps a line of a batch run.
		l, lerr := store.New(s.Pool).GetBatchLineByTrackingCode(ctx, req.Code)
		if errors.Is(lerr, pgx.ErrNoRows) {
			return GetTracking404ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(notFound())}, nil
		}
		if lerr != nil {
			return nil, lerr
		}
		return GetTracking200JSONResponse(lineTrackingJSON(l)), nil
	}
	if err != nil {
		return nil, err
	}
	return GetTracking200JSONResponse(trackingJSON(inv)), nil
}

// lineTrackingJSON is a batch line in the tracking view: the payment is
// received when its transaction is mined and confirmed when final. The
// contractor keeps the stablecoin, so converted and sent to bank are
// skipped (docs/design/teams.md §7).
func lineTrackingJSON(l store.GetBatchLineByTrackingCodeRow) Tracking {
	st := statemachine.BatchState(l.BatchState)
	state := InvoiceStateOpen
	received, confirmed := Waiting, Waiting
	var at *time.Time
	switch st {
	case statemachine.BatchSending:
		received = Current
	case statemachine.BatchConfirming:
		state, received, confirmed = InvoiceStateConfirmed, Done, Current
	case statemachine.BatchPaid:
		u := l.BatchUpdatedAt.UTC()
		state, received, confirmed, at = InvoiceStateSettled, Done, Done, &u
	case statemachine.BatchFailed, statemachine.BatchRejected, statemachine.BatchCancelled:
		state = InvoiceStateCancelled
	}
	return Tracking{
		TrackingCode: l.TrackingCode, ContractorName: l.ContractorName, Amount: usdcAmount(l.Amount),
		InvoiceState: state, UpdatedAt: l.BatchUpdatedAt.UTC(),
		Steps: []TrackingStep{
			{Step: TrackingStepStepReceived, Status: received},
			{Step: TrackingStepStepConfirmed, Status: confirmed, At: at},
			{Step: TrackingStepStepConverted, Status: Skipped},
			{Step: TrackingStepStepSentToBank, Status: Skipped},
		},
	}
}

// notFound deliberately says nothing about whether a similar code exists.
func notFound() Problem {
	return problem(404, "not-found", "We can't find a payment with this tracking number.", "")
}

func (s *Server) invoiceJSON(inv invoice.Invoice) Invoice { return InvoiceJSON(s.WebURL, inv) }

// InvoiceJSON is an invoice as the API returns it. Webhook events carry the
// same shape.
func InvoiceJSON(webURL string, inv invoice.Invoice) Invoice {
	return Invoice{
		Id:               inv.ID,
		TrackingCode:     inv.TrackingCode,
		PayUrl:           webURL + "/pay/" + inv.TrackingCode,
		TrackUrl:         webURL + "/t/" + inv.TrackingCode,
		State:            InvoiceState(inv.State),
		Description:      inv.Description,
		Amount:           amountJSON(inv),
		Payout:           InvoicePayout(inv.PayoutMode),
		DepositAddresses: depositJSON(inv),
		ExpiresAt:        inv.ExpiresAt.UTC(),
		CreatedAt:        inv.CreatedAt.UTC(),
		Quote:            quoteJSON(inv),
		PayoutStatus:     payoutJSON(inv),
	}
}

func amountJSON(inv invoice.Invoice) Amount {
	return Amount{Asset: string(inv.Asset.Code), Minor: inv.Amount.String(), Scale: int(inv.Asset.Scale)}
}

func depositJSON(inv invoice.Invoice) []DepositAddress {
	out := make([]DepositAddress, 0, len(inv.DepositAddresses))
	for _, d := range inv.DepositAddresses {
		a := DepositAddress{Network: string(d.Network), Address: d.Address}
		if d.Token != "" {
			tok := d.Token
			a.Token = &tok
		} else {
			// Bitcoin: BIP21, with the amount in BTC.
			uri := "bitcoin:" + d.Address + "?amount=" + strings.TrimRight(strings.TrimRight(inv.Amount.Format(inv.Asset.Scale), "0"), ".")
			a.Uri = &uri
		}
		out = append(out, a)
	}
	return out
}

func trackingJSON(inv invoice.Invoice) Tracking {
	steps := inv.Steps()
	out := Tracking{
		TrackingCode:   inv.TrackingCode,
		ContractorName: inv.ContractorName,
		Amount:         amountJSON(inv),
		InvoiceState:   InvoiceState(inv.State),
		Steps:          make([]TrackingStep, 0, len(steps)),
		UpdatedAt:      inv.UpdatedAt.UTC(),
	}
	for _, st := range steps {
		var at *time.Time
		if st.At != nil {
			u := st.At.UTC()
			at = &u
		}
		out.Steps = append(out.Steps, TrackingStep{
			Step:   TrackingStepStep(st.Name),
			Status: TrackingStepStatus(st.Status),
			At:     at,
		})
	}
	return out
}
