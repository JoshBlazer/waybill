// Package invoice creates invoices and loads the views shown on payment
// links and tracking pages.
package invoice

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/btc"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/rates"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/tracking"
)

// Errors returned to API callers as 422s.
var (
	ErrInvalidAmount      = errors.New("invoice: invalid amount")
	ErrInvalidDescription = errors.New("invoice: description must be 1 to 500 characters")
	ErrInvalidPayout      = errors.New("invoice: payout must be hold or naira")
	ErrUnsupportedAsset   = errors.New("invoice: unsupported asset")
	ErrNoBankAccount      = errors.New("invoice: naira payouts need a verified bank account")
	// ErrRateUnavailable is a 503: no rate could be found, so no naira
	// invoice is made (I12). Hold invoices are unaffected.
	ErrRateUnavailable = errors.New("invoice: no naira rate available right now")
	ErrNoNetworks      = errors.New("invoice: no payment network is configured")
	ErrNotFound        = errors.New("invoice: not found")
	// ErrBTCHoldOnly: Bitcoin is receive-only this stage, so a BTC invoice
	// is held as BTC (ADR-045).
	ErrBTCHoldOnly = errors.New("invoice: a BTC invoice is held in BTC; naira payout is for USDC invoices")
)

// DefaultTTL is how long a payment link stays open.
const DefaultTTL = 7 * 24 * time.Hour

// Service creates invoices.
type Service struct {
	Deployments map[safety.Network]deployments.Deployment
	TTL         time.Duration
	Now         func() time.Time
	// Quoter locks the naira rate of naira invoices (ADR-039). Nil means
	// naira invoices are refused with ErrRateUnavailable.
	Quoter *rates.Quoter
	// BTC derives receive addresses for BTC invoices, per Bitcoin network
	// (ADR-045). Empty means BTC invoices are refused.
	BTC map[safety.Network]*btc.Deriver
}

// NairaPair is the conversion a naira invoice locks.
var NairaPair = rates.Pair{From: money.USDC.Code, To: money.NGN.Code}

// Input is a validated-by-schema create request.
type Input struct {
	Description string
	Amount      string // major units, for example "125.50"
	Asset       money.AssetCode
	Payout      string // "hold" or "naira"
}

// DepositAddress is where to pay on one network.
type DepositAddress struct {
	Network safety.Network
	Address string // EIP-55
	Token   string // EIP-55
}

// Invoice is a created or loaded invoice with everything the API shows.
type Invoice struct {
	store.Invoice
	Asset            money.Asset
	ContractorName   string
	Verified         bool
	DepositAddresses []DepositAddress
	Events           []tracking.Event
	LatestEventID    int64
	Quote            *rates.Quote // naira invoices only
	Payout           *store.Payout
	PayoutEvents     []store.PayoutEvent
}

// NairaAmount is what the contractor receives at the locked rate, in kobo,
// rounded down. Zero for hold invoices.
func (inv Invoice) NairaAmount() money.Amount {
	if inv.Quote == nil {
		return money.Zero
	}
	return rates.Convert(inv.Amount, inv.Asset, inv.Quote.Rate)
}

// Create inserts an invoice, its deposit addresses and a "created" event
// inside tx.
func (s *Service) Create(ctx context.Context, tx pgx.Tx, contractor uuid.UUID, in Input) (Invoice, error) {
	// The API layer does not validate bodies against the schema, so every
	// constraint the database would enforce is checked here first: bad input
	// must be a 422, never a database error.
	if n := utf8.RuneCountInString(strings.TrimSpace(in.Description)); n < 1 || n > 500 {
		return Invoice{}, ErrInvalidDescription
	}
	switch in.Payout {
	case "hold", "naira":
	default:
		return Invoice{}, ErrInvalidPayout
	}
	asset, ok := money.LookupAsset(in.Asset)
	isBTC := ok && asset.Code == money.BTC.Code
	if !ok || (asset.Code != money.USDC.Code && !isBTC) {
		return Invoice{}, fmt.Errorf("%w: %s", ErrUnsupportedAsset, in.Asset)
	}
	if isBTC && in.Payout != "hold" {
		return Invoice{}, ErrBTCHoldOnly
	}
	amount, err := money.ParseDecimal(in.Amount, asset.Scale)
	if err != nil {
		return Invoice{}, fmt.Errorf("%w: %w", ErrInvalidAmount, err)
	}
	if amount.Sign() <= 0 {
		return Invoice{}, fmt.Errorf("%w: must be greater than zero", ErrInvalidAmount)
	}
	if (isBTC && len(s.BTC) == 0) || (!isBTC && len(s.Deployments) == 0) {
		return Invoice{}, ErrNoNetworks
	}
	code, err := NewTrackingCode()
	if err != nil {
		return Invoice{}, err
	}
	ttl := s.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}

	q := store.New(tx)
	var quote *rates.Quote
	if in.Payout == "naira" {
		if quote, err = s.lockRate(ctx, q, contractor); err != nil {
			return Invoice{}, err
		}
	}
	var quoteID *uuid.UUID
	if quote != nil {
		quoteID = &quote.ID
	}
	row, err := q.InsertInvoice(ctx, store.InsertInvoiceParams{
		RateQuoteID:  quoteID,
		ID:           uuid.New(),
		ContractorID: contractor,
		TrackingCode: code,
		Description:  strings.TrimSpace(in.Description),
		AssetCode:    string(asset.Code),
		Amount:       amount,
		PayoutMode:   in.Payout,
		ExpiresAt:    s.Now().Add(ttl),
	})
	if err != nil {
		return Invoice{}, fmt.Errorf("invoice: insert: %w", err)
	}

	var addrs []DepositAddress
	if isBTC {
		if addrs, err = s.btcAddresses(ctx, q, row.ID); err != nil {
			return Invoice{}, err
		}
	}
	salt := evm.SaltForInvoice(row.ID)
	for _, n := range sortedNetworks(s.Deployments) {
		if isBTC {
			break // paid on Bitcoin only
		}
		d := s.Deployments[n]
		addr := d.PredictDepositAddress(salt)
		if err := q.InsertDepositAddress(ctx, store.InsertDepositAddressParams{
			Network: string(n), Address: evm.NormalizeAddress(addr), InvoiceID: row.ID, Salt: salt[:],
		}); err != nil {
			return Invoice{}, fmt.Errorf("invoice: deposit address on %s: %w", n, err)
		}
		addrs = append(addrs, DepositAddress{Network: n, Address: addr.Hex(), Token: d.Token.Hex()})
	}

	ev, err := q.InsertInvoiceEvent(ctx, store.InsertInvoiceEventParams{
		InvoiceID: row.ID, Type: tracking.EventCreated, ToState: string(statemachine.InvoiceOpen), Data: []byte(`{}`),
	})
	if err != nil {
		return Invoice{}, fmt.Errorf("invoice: event: %w", err)
	}
	return Invoice{
		Invoice:          row,
		Asset:            asset,
		DepositAddresses: addrs,
		Events:           []tracking.Event{{Type: tracking.EventCreated, At: ev.CreatedAt}},
		LatestEventID:    ev.ID,
		Quote:            quote,
	}, nil
}

// btcAddresses gives the invoice a fresh receive address on each Bitcoin
// network: the next unused index of that network's xpub.
func (s *Service) btcAddresses(ctx context.Context, q *store.Queries, id uuid.UUID) ([]DepositAddress, error) {
	nets := make([]safety.Network, 0, len(s.BTC))
	for n := range s.BTC {
		nets = append(nets, n)
	}
	slices.Sort(nets)
	var out []DepositAddress
	for _, n := range nets {
		idx, err := q.NextBTCIndex(ctx, string(n))
		if err != nil {
			return nil, err
		}
		addr, err := s.BTC[n].Address(uint32(idx)) //nolint:gosec // the counter is capped below 2^31
		if err != nil {
			return nil, err
		}
		if err := q.InsertBTCAddress(ctx, store.InsertBTCAddressParams{Address: addr, Network: string(n), InvoiceID: id, DerivationIndex: int64(idx)}); err != nil {
			return nil, fmt.Errorf("invoice: address on %s: %w", n, err)
		}
		out = append(out, DepositAddress{Network: n, Address: addr})
	}
	return out, nil
}

// lockRate checks that the contractor can receive naira and stores a quote
// for the invoice to reference.
func (s *Service) lockRate(ctx context.Context, q *store.Queries, contractor uuid.UUID) (*rates.Quote, error) {
	if _, err := q.GetBankAccount(ctx, contractor); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoBankAccount
	} else if err != nil {
		return nil, err
	}
	if s.Quoter == nil {
		return nil, ErrRateUnavailable
	}
	quote, err := s.Quoter.Quote(ctx, NairaPair)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRateUnavailable, err)
	}
	if err := q.InsertRateQuote(ctx, store.InsertRateQuoteParams{
		ID: quote.ID, FromAsset: string(quote.Pair.From), ToAsset: string(quote.Pair.To),
		Mid: quote.Mid, Rate: quote.Rate, SpreadBps: int32(quote.SpreadBps), //nolint:gosec // 0 to 9999
		Source: quote.Source, ObservedAt: quote.ObservedAt, QuotedAt: quote.QuotedAt, ExpiresAt: quote.ExpiresAt,
	}); err != nil {
		return nil, fmt.Errorf("invoice: store quote: %w", err)
	}
	return &quote, nil
}

// loadQuote reads an invoice's quote, if it has one.
func loadQuote(ctx context.Context, q *store.Queries, id *uuid.UUID) (*rates.Quote, error) {
	if id == nil {
		return nil, nil
	}
	r, err := q.GetRateQuote(ctx, *id)
	if err != nil {
		return nil, fmt.Errorf("invoice: quote %s: %w", id, err)
	}
	return &rates.Quote{
		ID: r.ID, Pair: rates.Pair{From: money.AssetCode(r.FromAsset), To: money.AssetCode(r.ToAsset)},
		Mid: r.Mid, Rate: r.Rate, SpreadBps: int64(r.SpreadBps), Source: r.Source,
		ObservedAt: r.ObservedAt, QuotedAt: r.QuotedAt, ExpiresAt: r.ExpiresAt,
	}, nil
}

// LoadForContractor returns the contractor's invoice with this id, as Load
// does; another contractor's invoice is ErrNotFound.
func LoadForContractor(ctx context.Context, db store.DBTX, tokens map[safety.Network]string, id, contractor uuid.UUID) (Invoice, error) {
	row, err := store.New(db).GetInvoiceForContractor(ctx, store.GetInvoiceForContractorParams{ID: id, ContractorID: contractor})
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	if err != nil {
		return Invoice{}, err
	}
	return Load(ctx, db, tokens, row.TrackingCode)
}

// LoadByID returns the invoice with this id, as Load does.
func LoadByID(ctx context.Context, db store.DBTX, tokens map[safety.Network]string, id uuid.UUID) (Invoice, error) {
	code, err := store.New(db).GetInvoiceTrackingCode(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	if err != nil {
		return Invoice{}, err
	}
	return Load(ctx, db, tokens, code)
}

// Load returns the invoice with this tracking code, with contractor name,
// deposit addresses and events.
func Load(ctx context.Context, db store.DBTX, tokens map[safety.Network]string, code string) (Invoice, error) {
	q := store.New(db)
	row, err := q.GetInvoiceByTrackingCode(ctx, code)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	if err != nil {
		return Invoice{}, err
	}
	asset, ok := money.LookupAsset(money.AssetCode(row.AssetCode))
	if !ok {
		return Invoice{}, fmt.Errorf("invoice: unknown asset %s", row.AssetCode)
	}
	das, err := q.ListDepositAddresses(ctx, row.ID)
	if err != nil {
		return Invoice{}, err
	}
	evs, err := q.ListInvoiceEvents(ctx, row.ID)
	if err != nil {
		return Invoice{}, err
	}
	inv := Invoice{
		Invoice: store.Invoice{
			ID: row.ID, ContractorID: row.ContractorID, TrackingCode: row.TrackingCode,
			Description: row.Description, AssetCode: row.AssetCode, Amount: row.Amount,
			PayoutMode: row.PayoutMode, State: row.State, ExpiresAt: row.ExpiresAt,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, RateQuoteID: row.RateQuoteID,
		},
		Asset:          asset,
		ContractorName: row.ContractorName,
		Verified:       row.ContractorVerifiedAt != nil,
	}
	if inv.Quote, err = loadQuote(ctx, q, row.RateQuoteID); err != nil {
		return Invoice{}, err
	}
	if p, err := q.GetPayoutByInvoice(ctx, row.ID); err == nil {
		inv.Payout = &p
		if inv.PayoutEvents, err = q.ListPayoutEvents(ctx, row.ID); err != nil {
			return Invoice{}, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, err
	}
	for _, d := range das {
		n := safety.Network(d.Network)
		inv.DepositAddresses = append(inv.DepositAddresses, DepositAddress{
			Network: n, Address: checksum(d.Address), Token: checksum(tokens[n]),
		})
	}
	btcAddrs, err := q.ListBTCAddressesByInvoice(ctx, row.ID)
	if err != nil {
		return Invoice{}, err
	}
	for _, a := range btcAddrs {
		inv.DepositAddresses = append(inv.DepositAddresses, DepositAddress{Network: safety.Network(a.Network), Address: a.Address})
	}
	for _, e := range evs {
		inv.Events = append(inv.Events, tracking.Event{Type: e.Type, At: e.CreatedAt})
		inv.LatestEventID = e.ID
	}
	return inv, nil
}

// Steps returns the tracking line for the invoice.
func (inv Invoice) Steps() []tracking.Step {
	var p *tracking.Payout
	if inv.Payout != nil {
		p = &tracking.Payout{State: statemachine.PayoutState(inv.Payout.State)}
		for _, e := range inv.PayoutEvents {
			at := e.CreatedAt
			switch statemachine.PayoutState(e.ToState) {
			case statemachine.PayoutConverted:
				if p.ConvertedAt == nil {
					p.ConvertedAt = &at
				}
			case statemachine.PayoutSucceeded:
				p.SentAt = &at
			}
		}
	}
	return tracking.Build(statemachine.InvoiceState(inv.State), inv.PayoutMode, inv.Events, p)
}

// crockford is Crockford's base32 alphabet: no I, L, O or U, so codes read
// aloud or typed from paper are unambiguous.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewTrackingCode returns a random 80-bit code, WB-XXXX-XXXX-XXXX-XXXX.
// 80 bits make guessing a valid code infeasible (docs/RISKS.md §2).
func NewTrackingCode() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("WB")
	bits, nbits := uint32(0), 0
	chars := 0
	for _, x := range b {
		bits = bits<<8 | uint32(x)
		nbits += 8
		for nbits >= 5 {
			if chars%4 == 0 {
				sb.WriteByte('-')
			}
			sb.WriteByte(crockford[(bits>>(nbits-5))&31])
			nbits -= 5
			chars++
		}
	}
	return sb.String(), nil
}

func sortedNetworks(m map[safety.Network]deployments.Deployment) []safety.Network {
	out := make([]safety.Network, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func checksum(lower string) string {
	if lower == "" {
		return ""
	}
	return evm.Checksum(lower)
}
