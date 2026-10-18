package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/conversion"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// Contractor balance and conversion (docs/design/conversion.md, ADR-044).

func conversionJSON(c conversion.Conversion) Conversion {
	out := Conversion{
		Id: c.ID, State: ConversionState(c.State), Amount: amount(c.USDC, money.USDC), Naira: amount(c.Naira, money.NGN),
		Rate: amount(c.Rate, money.NGN), ExpiresAt: c.ExpiresAt, CreatedAt: c.CreatedAt, AcceptedAt: c.Accepted,
	}
	if p := c.Payout; p != nil {
		out.Payout = &ConversionPayout{State: p.State, Reference: p.Reference}
	}
	return out
}

// GetBalance returns the contractor's balances and recent conversions.
func (s *Server) GetBalance(ctx context.Context, _ GetBalanceRequestObject) (GetBalanceResponseObject, error) {
	c, _ := auth.ContractorFrom(ctx)
	b, err := s.Conversions.Balance(ctx, c)
	if err != nil {
		return nil, err
	}
	out := Balance{
		UsdcAvailable: amount(b.USDCAvailable, money.USDC), UsdcPending: amount(b.USDCPending, money.USDC),
		NairaOnItsWay: amount(b.NGNOnItsWay, money.NGN), Conversions: make([]Conversion, 0, len(b.Recent)),
	}
	for _, r := range b.Recent {
		out.Conversions = append(out.Conversions, conversionJSON(r))
	}
	return GetBalance200JSONResponse(out), nil
}

// QuoteConversion fixes a rate for converting part of the USDC balance.
func (s *Server) QuoteConversion(ctx context.Context, req QuoteConversionRequestObject) (QuoteConversionResponseObject, error) {
	c, _ := auth.ContractorFrom(ctx)
	if req.Body == nil {
		return QuoteConversion422ApplicationProblemPlusJSONResponse(problem(422, "validation", "Send the amount of USDC to convert.", "")), nil
	}
	q, err := s.Conversions.Quote(ctx, c, req.Body.Amount)
	switch {
	case errors.Is(err, conversion.ErrInvalidAmount):
		return QuoteConversion422ApplicationProblemPlusJSONResponse(problem(422, "validation", "Convert at least 1 USDC, written like 40.50.", "")), nil
	case errors.Is(err, conversion.ErrInsufficient):
		return QuoteConversion422ApplicationProblemPlusJSONResponse(problem(422, "insufficient-balance", "That is more than your available balance.", "")), nil
	case errors.Is(err, conversion.ErrNoBankAccount):
		return QuoteConversion422ApplicationProblemPlusJSONResponse(problem(422, "no-bank-account",
			"Add a verified bank account before converting.", "PUT /v1/me/bank-account")), nil
	case errors.Is(err, conversion.ErrRateUnavailable):
		s.logger().WarnContext(ctx, "quote conversion: no naira rate", "err", err)
		return QuoteConversion503ApplicationProblemPlusJSONResponse(problem(503, "rate-unavailable",
			"We can't get a naira rate right now. Nothing was converted.", "Try again in a few minutes.")), nil
	case err != nil:
		return nil, err
	}
	return QuoteConversion201JSONResponse(conversionJSON(q)), nil
}

// GetConversion returns one of the contractor's conversions.
func (s *Server) GetConversion(ctx context.Context, req GetConversionRequestObject) (GetConversionResponseObject, error) {
	c, _ := auth.ContractorFrom(ctx)
	got, err := s.Conversions.Get(ctx, s.Pool, c, req.Id)
	if errors.Is(err, conversion.ErrNotFound) {
		return GetConversion404ApplicationProblemPlusJSONResponse(problem(404, "not-found", "There is no such conversion.", "")), nil
	}
	if err != nil {
		return nil, err
	}
	return GetConversion200JSONResponse(conversionJSON(got)), nil
}

var (
	errConversionNotFound = errors.New("conversion not found")
	errConversionExpired  = errors.New("conversion expired")
	errConversionShort    = errors.New("conversion short")
)

// AcceptConversion books an unexpired quote and queues its payout. A
// repeated Idempotency-Key returns the first answer.
func (s *Server) AcceptConversion(ctx context.Context, req AcceptConversionRequestObject) (AcceptConversionResponseObject, error) {
	contractor, _ := auth.ContractorFrom(ctx)
	key := req.Params.IdempotencyKey
	if !validIdempotencyKey(key) {
		return AcceptConversion409ApplicationProblemPlusJSONResponse(problem(409, "bad-request", "Idempotency-Key must be 1 to 255 characters.", "")), nil
	}
	reqHash := sha256.Sum256([]byte("accept-conversion:" + req.Id.String()))
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
		got, err := s.Conversions.Accept(ctx, tx, contractor, req.Id)
		switch {
		case errors.Is(err, conversion.ErrNotFound):
			return errConversionNotFound
		case errors.Is(err, conversion.ErrExpired):
			return errConversionExpired
		case errors.Is(err, conversion.ErrInsufficient):
			return errConversionShort
		case err != nil:
			return err
		}
		if body, err = json.Marshal(conversionJSON(got)); err != nil {
			return err
		}
		status := int32(http.StatusOK)
		return q.CompleteIdempotencyKey(ctx, store.CompleteIdempotencyKeyParams{Principal: principal, Key: key, StatusCode: &status, ResponseBody: body})
	})
	switch {
	case errors.Is(err, errConversionNotFound):
		return AcceptConversion404ApplicationProblemPlusJSONResponse(problem(404, "not-found", "There is no such conversion.", "")), nil
	case errors.Is(err, errConversionExpired):
		return AcceptConversion409ApplicationProblemPlusJSONResponse(problem(409, "quote-expired",
			"This rate has expired, so nothing was converted.", "Ask for a new quote: POST /v1/me/conversions")), nil
	case errors.Is(err, errConversionShort):
		return AcceptConversion422ApplicationProblemPlusJSONResponse(problem(422, "insufficient-balance",
			"Your available balance is now less than this conversion.", "")), nil
	case errors.Is(err, errKeyReused):
		return AcceptConversion409ApplicationProblemPlusJSONResponse(problem(409, "idempotency-key-reused",
			"This Idempotency-Key was already used with a different request.", "")), nil
	case err != nil:
		return nil, err
	}
	var out Conversion
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return AcceptConversion200JSONResponse(out), nil
}
