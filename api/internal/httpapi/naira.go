package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/bankverify"
	"github.com/JoshBlazer/waybill/api/internal/checkout"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// payoutCoolingOff is how long payouts to a changed bank account wait
// (account takeover, RISKS §2).
const payoutCoolingOff = 24 * time.Hour

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func bankAccountJSON(a store.ContractorBankAccount) BankAccount {
	return BankAccount{
		BankCode: a.BankCode, BankName: a.BankName, AccountNumberLast4: a.AccountNumber[len(a.AccountNumber)-4:],
		AccountName: a.AccountName, VerifiedAt: a.VerifiedAt.UTC(), PayoutsAllowedFrom: a.PayoutsAllowedFrom.UTC(),
	}
}

// ListBanks lists banks that can receive payouts.
func (s *Server) ListBanks(ctx context.Context, _ ListBanksRequestObject) (ListBanksResponseObject, error) {
	banks, err := s.Banks.ListBanks(ctx)
	if err != nil {
		s.logger().WarnContext(ctx, "banks: list failed", "err", err)
		return ListBanks503ApplicationProblemPlusJSONResponse(problem(503, "banks-unavailable",
			"We can't load the list of banks right now. Try again in a minute.", "")), nil
	}
	out := BankList{Items: make([]Bank, 0, len(banks))}
	for _, b := range banks {
		out.Items = append(out.Items, Bank{Code: b.Code, Name: b.Name})
	}
	return ListBanks200JSONResponse(out), nil
}

// GetBankAccount returns the contractor's payout account.
func (s *Server) GetBankAccount(ctx context.Context, _ GetBankAccountRequestObject) (GetBankAccountResponseObject, error) {
	contractor, ok := auth.ContractorFrom(ctx)
	if !ok {
		return nil, errors.New("getBankAccount: no authenticated contractor")
	}
	a, err := store.New(s.Pool).GetBankAccount(ctx, contractor)
	if errors.Is(err, pgx.ErrNoRows) {
		return GetBankAccount404ApplicationProblemPlusJSONResponse(problem(404, "not-found", "No bank account yet.", "")), nil
	}
	if err != nil {
		return nil, err
	}
	return GetBankAccount200JSONResponse(bankAccountJSON(a)), nil
}

// SetBankAccount verifies an account against the bank and saves it if the
// bank's name for it matches the contractor's legal name.
func (s *Server) SetBankAccount(ctx context.Context, req SetBankAccountRequestObject) (SetBankAccountResponseObject, error) {
	contractor, ok := auth.ContractorFrom(ctx)
	if !ok {
		return nil, errors.New("setBankAccount: no authenticated contractor")
	}
	if req.Body == nil {
		return SetBankAccount400ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(
			problem(400, "bad-request", "A request body is required.", ""))}, nil
	}
	acct := bankverify.Account{BankCode: req.Body.BankCode, AccountNumber: req.Body.AccountNumber}
	if err := acct.Validate(); err != nil {
		return SetBankAccount400ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(
			problem(400, "bad-request", "The account number must be 10 digits and the bank code 3 to 6 digits.", ""))}, nil
	}
	unavailable := SetBankAccount503ApplicationProblemPlusJSONResponse(problem(503, "bank-unavailable",
		"We couldn't reach the bank to check this account. Nothing was saved; try again in a few minutes.", ""))
	banks, err := s.Banks.ListBanks(ctx)
	if err != nil {
		return unavailable, nil
	}
	bankName := ""
	for _, b := range banks {
		if b.Code == acct.BankCode {
			bankName = b.Name
		}
	}
	if bankName == "" {
		return SetBankAccount422ApplicationProblemPlusJSONResponse(problem(422, "unknown-bank", "We don't know that bank code.", "Use a code from GET /v1/banks.")), nil
	}
	q := store.New(s.Pool)
	c, err := q.GetContractor(ctx, contractor)
	if err != nil {
		return nil, err
	}
	res, err := bankverify.Verify(ctx, s.Resolver, c.LegalName, acct)
	switch {
	case errors.Is(err, bankverify.ErrAccountNotFound):
		return SetBankAccount422ApplicationProblemPlusJSONResponse(problem(422, "account-not-found",
			"The bank has no account with that number.", "Check the number and the bank.")), nil
	case err != nil:
		s.logger().WarnContext(ctx, "bank account: could not verify", "err", err)
		return unavailable, nil
	case !res.Matches:
		return SetBankAccount422ApplicationProblemPlusJSONResponse(problem(422, "name-mismatch",
			"This account is not in your name.",
			fmt.Sprintf("The bank holds this account as %q, which does not match your legal name %q.", res.AccountName, c.LegalName))), nil
	}
	now := s.now()
	allowed := now // the first account can receive payouts at once
	if _, err := q.GetBankAccount(ctx, contractor); err == nil {
		allowed = now.Add(payoutCoolingOff) // a change waits (kept unchanged if the same account is saved again)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	saved, err := q.UpsertBankAccount(ctx, store.UpsertBankAccountParams{
		ContractorID: contractor, BankCode: acct.BankCode, BankName: bankName, AccountNumber: acct.AccountNumber,
		AccountName: res.AccountName, VerifiedAt: now, PayoutsAllowedFrom: allowed,
	})
	if err != nil {
		return nil, err
	}
	return SetBankAccount200JSONResponse(bankAccountJSON(saved)), nil
}

// GetInvoice returns one of the contractor's invoices.
func (s *Server) GetInvoice(ctx context.Context, req GetInvoiceRequestObject) (GetInvoiceResponseObject, error) {
	contractor, ok := auth.ContractorFrom(ctx)
	if !ok {
		return nil, errors.New("getInvoice: no authenticated contractor")
	}
	inv, err := invoice.LoadForContractor(ctx, s.Pool, s.Tokens, req.Id, contractor)
	if errors.Is(err, invoice.ErrNotFound) {
		return GetInvoice404ApplicationProblemPlusJSONResponse(problem(404, "not-found", "There is no such invoice.", "")), nil
	}
	if err != nil {
		return nil, err
	}
	return GetInvoice200JSONResponse(s.invoiceJSON(inv)), nil
}

var (
	errNotUnderpaid = errors.New("invoice is not underpaid")
	errNoInvoice    = errors.New("no such invoice")
)

// AcceptShortfall settles an underpaid invoice as is. Idempotent: a retry
// with the same key returns the first response.
func (s *Server) AcceptShortfall(ctx context.Context, req AcceptShortfallRequestObject) (AcceptShortfallResponseObject, error) {
	contractor, ok := auth.ContractorFrom(ctx)
	if !ok {
		return nil, errors.New("acceptShortfall: no authenticated contractor")
	}
	key := req.Params.IdempotencyKey
	if !validIdempotencyKey(key) {
		return AcceptShortfall409ApplicationProblemPlusJSONResponse(problem(409, "bad-request", "Idempotency-Key must be 1 to 255 characters.", "")), nil
	}
	reqHash := sha256.Sum256([]byte("accept-shortfall:" + req.Id.String()))
	principal := "contractor:" + contractor.String()
	var body []byte
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		_, err := q.ClaimIdempotencyKey(ctx, store.ClaimIdempotencyKeyParams{Principal: principal, Key: key, RequestHash: reqHash[:]})
		if errors.Is(err, pgx.ErrNoRows) {
			rec, err := q.GetIdempotencyRecord(ctx, store.GetIdempotencyRecordParams{Principal: principal, Key: key})
			if err != nil {
				return err
			}
			if !bytes.Equal(rec.RequestHash, reqHash[:]) {
				return errKeyReused
			}
			body = rec.ResponseBody
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := q.GetInvoiceForContractor(ctx, store.GetInvoiceForContractorParams{ID: req.Id, ContractorID: contractor}); errors.Is(err, pgx.ErrNoRows) {
			return errNoInvoice
		} else if err != nil {
			return err
		}
		if _, err := invoice.Apply(ctx, tx, req.Id, statemachine.EvUnderpaymentAccepted, map[string]any{"by": "contractor"}); err != nil {
			if errors.Is(err, statemachine.ErrIllegalTransition) {
				return errNotUnderpaid
			}
			return err
		}
		// Already final: settle now (the watcher settles on finality, which
		// has passed).
		open, err := q.SumPaymentsByState(ctx, store.SumPaymentsByStateParams{InvoiceID: req.Id, States: []string{"detected", "confirmed"}})
		if err != nil {
			return err
		}
		if open.IsZero() {
			if _, err := invoice.Apply(ctx, tx, req.Id, statemachine.EvPaymentFinal, map[string]any{"after": "shortfall accepted"}); err != nil {
				return err
			}
		}
		inv, err := invoice.LoadForContractor(ctx, tx, s.Tokens, req.Id, contractor)
		if err != nil {
			return err
		}
		if body, err = json.Marshal(s.invoiceJSON(inv)); err != nil {
			return err
		}
		status := int32(http.StatusOK)
		return q.CompleteIdempotencyKey(ctx, store.CompleteIdempotencyKeyParams{Principal: principal, Key: key, StatusCode: &status, ResponseBody: body})
	})
	switch {
	case errors.Is(err, errNoInvoice):
		return AcceptShortfall404ApplicationProblemPlusJSONResponse(problem(404, "not-found", "There is no such invoice.", "")), nil
	case errors.Is(err, errNotUnderpaid):
		return AcceptShortfall409ApplicationProblemPlusJSONResponse(problem(409, "not-underpaid", "Only an underpaid invoice can be accepted as paid.", "")), nil
	case errors.Is(err, errKeyReused):
		return AcceptShortfall409ApplicationProblemPlusJSONResponse(problem(409, "idempotency-key-reused", "This Idempotency-Key was already used with a different request.", "")), nil
	case err != nil:
		return nil, err
	}
	var out Invoice
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return AcceptShortfall200JSONResponse(out), nil
}

// StartCheckout starts (or returns the open) Paystack checkout for a naira
// payment link.
func (s *Server) StartCheckout(ctx context.Context, req StartCheckoutRequestObject) (StartCheckoutResponseObject, error) {
	unavailable := StartCheckout503ApplicationProblemPlusJSONResponse(problem(503, "checkout-unavailable",
		"Card and bank transfer payments aren't available right now. You can still pay in stablecoin.", ""))
	if s.Checkout == nil {
		return unavailable, nil
	}
	c, _, err := s.Checkout.Start(ctx, req.Code)
	switch {
	case errors.Is(err, checkout.ErrNotFound):
		return StartCheckout404ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(notFound())}, nil
	case errors.Is(err, checkout.ErrNotPayable):
		return StartCheckout409ApplicationProblemPlusJSONResponse(problem(409, "not-payable-by-card",
			"This link can't be paid by card or bank transfer.", err.Error())), nil
	case errors.Is(err, checkout.ErrUnavailable):
		s.logger().WarnContext(ctx, "checkout: unavailable", "err", err)
		return unavailable, nil
	case err != nil:
		return nil, err
	}
	return StartCheckout200JSONResponse(Checkout{
		AuthorizationUrl: *c.AuthorizationUrl, Reference: c.Reference, ExpiresAt: c.ExpiresAt.UTC(),
		Amount: Amount{Asset: string(money.NGN.Code), Minor: c.Amount.String(), Scale: int(money.NGN.Scale)},
	}), nil
}

type rawBodyKey struct{}

// keepRawBody keeps the exact request body for the Paystack callback: its
// signature covers those bytes, and decoding re-encodes them differently.
func keepRawBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/providers/paystack/webhook" {
			raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				writeProblem(w, http.StatusBadRequest, "bad-request", "Could not read the body.", "")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			r = r.WithContext(context.WithValue(r.Context(), rawBodyKey{}, raw))
		}
		next.ServeHTTP(w, r)
	})
}

// PaystackWebhook checks the signature on the raw body, then stores the
// event once (I14). Processing happens in the worker; this only records.
func (s *Server) PaystackWebhook(ctx context.Context, req PaystackWebhookRequestObject) (PaystackWebhookResponseObject, error) {
	raw, _ := ctx.Value(rawBodyKey{}).([]byte)
	forged := PaystackWebhook401ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(
		problem(401, "bad-signature", "The signature does not match.", ""))}
	if s.Paystack == nil || len(raw) == 0 {
		return forged, nil
	}
	if err := s.Paystack.VerifyWebhook(raw, req.Params.XPaystackSignature); err != nil {
		s.logger().WarnContext(ctx, "paystack webhook: bad signature; ignored")
		return forged, nil
	}
	var ev struct {
		Event string `json:"event"`
		Data  struct {
			ID        json.Number `json:"id"`
			Reference string      `json:"reference"`
		} `json:"data"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&ev); err != nil || ev.Event == "" {
		return forged, nil
	}
	id := ev.Event + ":" + ev.Data.ID.String()
	if ev.Data.ID == "" {
		sum := sha256.Sum256(raw)
		id = ev.Event + ":sha256:" + hex.EncodeToString(sum[:])
	}
	inserted, err := store.New(s.Pool).InsertProviderEvent(ctx, store.InsertProviderEventParams{
		Provider: "paystack", EventID: id, EventType: ev.Event, Raw: raw,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		s.logger().InfoContext(ctx, "paystack webhook: replay of a stored event", "event", id)
	case err != nil:
		return nil, err // Paystack retries on non-2xx
	default:
		s.logger().InfoContext(ctx, "paystack webhook: stored", "event", id, "row", strconv.FormatInt(inserted, 10))
	}
	return PaystackWebhook200Response{}, nil
}

func payoutJSON(inv invoice.Invoice) *Payout {
	p := inv.Payout
	if p == nil {
		return nil
	}
	return &Payout{
		State:         PayoutState(p.State),
		NairaAmount:   Amount{Asset: string(money.NGN.Code), Minor: p.NairaAmount.String(), Scale: int(money.NGN.Scale)},
		FailureReason: p.Failure,
		UpdatedAt:     p.UpdatedAt.UTC(),
	}
}

// quoteJSON and payout fields on invoices.
func quoteJSON(inv invoice.Invoice) *Quote {
	if inv.Quote == nil {
		return nil
	}
	q := inv.Quote
	return &Quote{
		Rate:        Amount{Asset: string(money.NGN.Code), Minor: q.Rate.String(), Scale: int(money.NGN.Scale)},
		NairaAmount: Amount{Asset: string(money.NGN.Code), Minor: inv.NairaAmount().String(), Scale: int(money.NGN.Scale)},
		SpreadBps:   int(q.SpreadBps), Source: q.Source, QuotedAt: q.QuotedAt.UTC(), ExpiresAt: q.ExpiresAt.UTC(),
	}
}
