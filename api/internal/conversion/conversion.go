// Package conversion lets a contractor convert their USDC balance to naira
// at a quoted rate and have it sent to their bank (docs/design/conversion.md,
// ADR-044).
//
//   - Quote fixes a rate and the naira amount until the quote expires.
//   - Accept, before expiry, books the conversion (the same four legs as an
//     invoice payout's conversion step) and queues a payout that starts
//     converted, so the existing payout pipeline sends it. After expiry it
//     is refused: Waybill never re-quotes on accept.
package conversion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/payout"
	"github.com/JoshBlazer/waybill/api/internal/rates"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// Errors.
var (
	ErrNotFound        = errors.New("conversion: not found")
	ErrInvalidAmount   = errors.New("conversion: amount must be a USDC amount of at least 1")
	ErrInsufficient    = errors.New("conversion: more than the available balance")
	ErrNoBankAccount   = errors.New("conversion: no verified bank account")
	ErrExpired         = errors.New("conversion: the quote has expired")
	ErrRateUnavailable = errors.New("conversion: no naira rate right now")
)

// Minimum is the smallest conversion: 1 USDC.
var Minimum = money.FromInt64(1_000_000)

// QuoteTTL is how long a conversion quote holds. It is short on purpose: a
// long quote on a balance would hand the contractor a free option against
// the market.
const QuoteTTL = 2 * time.Minute

// Service runs conversions.
type Service struct {
	Pool   *pgxpool.Pool
	Quoter *rates.Quoter // its TTL is the quote's lifetime; see QuoteTTL
	Now    func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Conversion is one conversion with its quote, and its payout once accepted.
type Conversion struct {
	ID        uuid.UUID
	State     string // quoted, expired (a quote past its time) or accepted
	USDC      money.Amount
	Naira     money.Amount
	Rate      money.Amount // kobo per 1 USDC
	ExpiresAt time.Time
	CreatedAt time.Time
	Accepted  *time.Time
	Payout    *store.Payout
}

func (s *Service) view(c store.Conversion, rate money.Amount, expires time.Time, p *store.Payout) Conversion {
	state := c.State
	if state == "quoted" && !s.now().Before(expires) {
		state = "expired"
	}
	return Conversion{ID: c.ID, State: state, USDC: c.UsdcAmount, Naira: c.NairaAmount, Rate: rate,
		ExpiresAt: expires, CreatedAt: c.CreatedAt, Accepted: c.AcceptedAt, Payout: p}
}

func available(ctx context.Context, q *store.Queries, contractor uuid.UUID, asset money.Asset, bucket string) (money.Amount, error) {
	b, err := q.GetBalanceByCode(ctx, fmt.Sprintf("liability:contractor:%s:%s:%s", contractor, asset.Code, bucket))
	if err != nil {
		return money.Zero, err
	}
	return b.Neg(), nil // a liability: what is owed to the contractor
}

// Quote fixes a rate for converting amount (a decimal USDC string).
func (s *Service) Quote(ctx context.Context, contractor uuid.UUID, amount string) (Conversion, error) {
	usdc, err := money.ParseDecimal(amount, money.USDC.Scale)
	if err != nil || usdc.Cmp(Minimum) < 0 {
		return Conversion{}, ErrInvalidAmount
	}
	q := store.New(s.Pool)
	if _, err := q.GetBankAccount(ctx, contractor); errors.Is(err, pgx.ErrNoRows) {
		return Conversion{}, ErrNoBankAccount
	} else if err != nil {
		return Conversion{}, err
	}
	have, err := available(ctx, q, contractor, money.USDC, "available")
	if err != nil {
		return Conversion{}, err
	}
	if have.Cmp(usdc) < 0 {
		return Conversion{}, ErrInsufficient // checked again, for real, on accept
	}
	qt, err := s.Quoter.Quote(ctx, invoice.NairaPair)
	if err != nil {
		return Conversion{}, fmt.Errorf("%w: %w", ErrRateUnavailable, err)
	}
	naira := rates.Convert(usdc, money.USDC, qt.Rate)
	var out Conversion
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.InsertRateQuote(ctx, store.InsertRateQuoteParams{
			ID: qt.ID, FromAsset: string(qt.Pair.From), ToAsset: string(qt.Pair.To), Mid: qt.Mid, Rate: qt.Rate,
			SpreadBps: int32(qt.SpreadBps), Source: qt.Source, ObservedAt: qt.ObservedAt, QuotedAt: qt.QuotedAt, ExpiresAt: qt.ExpiresAt, //nolint:gosec // 0 to 9999
		}); err != nil {
			return err
		}
		c, err := q.InsertConversion(ctx, store.InsertConversionParams{
			ID: uuid.New(), ContractorID: contractor, UsdcAmount: usdc, NairaAmount: naira, RateQuoteID: qt.ID,
		})
		if err != nil {
			return err
		}
		out = s.view(c, qt.Rate, qt.ExpiresAt, nil)
		return nil
	})
	return out, err
}

// Get returns one of the contractor's conversions.
func (s *Service) Get(ctx context.Context, db store.DBTX, contractor, id uuid.UUID) (Conversion, error) {
	q := store.New(db)
	row, err := q.GetConversion(ctx, store.GetConversionParams{ID: id, ContractorID: contractor})
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversion{}, ErrNotFound
	}
	if err != nil {
		return Conversion{}, err
	}
	c := store.Conversion{ID: row.ID, ContractorID: row.ContractorID, UsdcAmount: row.UsdcAmount, NairaAmount: row.NairaAmount,
		RateQuoteID: row.RateQuoteID, State: row.State, AcceptedAt: row.AcceptedAt, CreatedAt: row.CreatedAt}
	var p *store.Payout
	if row.State == "accepted" {
		po, err := q.GetPayoutByConversion(ctx, &row.ID)
		if err != nil {
			return Conversion{}, err
		}
		p = &po
	}
	return s.view(c, row.Rate, row.ExpiresAt, p), nil
}

// Accept books the conversion and queues its payout, inside tx. Accepting
// an accepted conversion returns it and books nothing.
func (s *Service) Accept(ctx context.Context, tx pgx.Tx, contractor, id uuid.UUID) (Conversion, error) {
	q := store.New(tx)
	c, err := q.LockConversion(ctx, store.LockConversionParams{ID: id, ContractorID: contractor})
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversion{}, ErrNotFound
	}
	if err != nil {
		return Conversion{}, err
	}
	if c.State == "accepted" {
		return s.Get(ctx, tx, contractor, id)
	}
	qt, err := q.GetRateQuote(ctx, c.RateQuoteID)
	if err != nil {
		return Conversion{}, err
	}
	now := s.now()
	if !now.Before(qt.ExpiresAt) {
		return Conversion{}, ErrExpired
	}
	if err := post(ctx, tx, c); err != nil {
		return Conversion{}, err
	}
	payoutID := uuid.New()
	if _, err := q.InsertConversionPayout(ctx, store.InsertConversionPayoutParams{
		ID: payoutID, ConversionID: &c.ID, ContractorID: contractor, SourceAmount: c.UsdcAmount,
		NairaAmount: c.NairaAmount, RateQuoteID: &c.RateQuoteID, Reference: payout.Reference(payoutID),
	}); err != nil {
		return Conversion{}, err
	}
	if err := q.InsertPayoutEvent(ctx, store.InsertPayoutEventParams{
		PayoutID: payoutID, Type: "converted", FromState: "queued", ToState: "converted",
		Data: fmt.Appendf(nil, `{"usdc":%q,"naira":%q,"rate":%q,"conversionId":%q}`, c.UsdcAmount, c.NairaAmount, qt.Rate, c.ID),
	}); err != nil {
		return Conversion{}, err
	}
	if err := q.MarkConversionAccepted(ctx, store.MarkConversionAcceptedParams{ID: c.ID, Now: now}); err != nil {
		return Conversion{}, err
	}
	return s.Get(ctx, tx, contractor, id)
}

// post books the conversion: the contractor is owed naira instead of USDC.
// The USDC stays in custody against the FX position, and the naira comes
// from the provider balance, exactly as for an invoice payout.
func post(ctx context.Context, tx pgx.Tx, c store.Conversion) error {
	usdc, ngn := money.USDC.Code, money.NGN.Code
	acct := func(code string, asset money.AssetCode, kind ledger.AccountKind, rule ledger.BalanceRule) (ledger.Account, error) {
		return ledger.EnsureAccount(ctx, tx, ledger.Account{Code: code, Asset: asset, Kind: kind, Rule: rule})
	}
	owedUSDC, err := acct(fmt.Sprintf("liability:contractor:%s:USDC:available", c.ContractorID), usdc, ledger.KindLiability, ledger.NonPositive)
	if err != nil {
		return err
	}
	fx, err := acct("equity:fx-position:USDC", usdc, ledger.KindEquity, ledger.AnyBalance)
	if err != nil {
		return err
	}
	float, err := acct("custody:paystack:NGN", ngn, ledger.KindAsset, ledger.NonNegative)
	if err != nil {
		return err
	}
	owedNGN, err := acct(fmt.Sprintf("liability:contractor:%s:NGN:available", c.ContractorID), ngn, ledger.KindLiability, ledger.NonPositive)
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.Entry{
		IdempotencyKey: "conversion:" + c.ID.String() + ":accepted",
		Kind:           "conversion.accepted",
		RefType:        "conversion",
		RefID:          c.ID.String(),
		Postings: []ledger.Posting{
			{Account: owedUSDC.ID, Asset: usdc, Amount: c.UsdcAmount},
			{Account: fx.ID, Asset: usdc, Amount: c.UsdcAmount.Neg()},
			{Account: float.ID, Asset: ngn, Amount: c.NairaAmount},
			{Account: owedNGN.ID, Asset: ngn, Amount: c.NairaAmount.Neg()},
		},
	})
	if errors.Is(err, ledger.ErrInsufficientFunds) {
		return ErrInsufficient
	}
	return err
}

// Balance is what the contractor holds.
type Balance struct {
	USDCAvailable money.Amount
	USDCPending   money.Amount
	NGNOnItsWay   money.Amount // converted, not yet sent to the bank
	Recent        []Conversion
}

// Balance returns the contractor's balances and their recent conversions.
func (s *Service) Balance(ctx context.Context, contractor uuid.UUID) (Balance, error) {
	q := store.New(s.Pool)
	var b Balance
	var err error
	if b.USDCAvailable, err = available(ctx, q, contractor, money.USDC, "available"); err != nil {
		return b, err
	}
	if b.USDCPending, err = available(ctx, q, contractor, money.USDC, "pending"); err != nil {
		return b, err
	}
	if b.NGNOnItsWay, err = available(ctx, q, contractor, money.NGN, "available"); err != nil {
		return b, err
	}
	rows, err := q.ListConversions(ctx, store.ListConversionsParams{ContractorID: contractor, MaxRows: 20})
	if err != nil {
		return b, err
	}
	for _, r := range rows {
		c, err := s.Get(ctx, s.Pool, contractor, r.ID)
		if err != nil {
			return b, err
		}
		b.Recent = append(b.Recent, c)
	}
	return b, nil
}
