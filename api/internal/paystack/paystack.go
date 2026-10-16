// Package paystack is a client for the parts of Paystack's API that
// Waybill uses in test mode: resolving bank accounts, creating transfer
// recipients, sending and verifying transfers, and checking webhook
// signatures. Endpoints and rules follow Paystack's API reference
// (https://paystack.com/docs/api/).
//
// Answers are classified for the payout state machine:
//   - a clear refusal from Paystack (HTTP 4xx with status false) is a
//     rejection: nothing was done, and the message says why;
//   - no answer, an HTTP 5xx, or a body that cannot be read is
//     ErrUnknown: Paystack may or may not have acted, and the caller must
//     reconcile by reference, never resubmit blind (I12).
//
// The client refuses any key that is not a test key; the service-level
// test-money guard checks the same thing at startup.
package paystack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/bankverify"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// DefaultBaseURL is Paystack's API.
const DefaultBaseURL = "https://api.paystack.co"

// Errors. ErrRejected and ErrNotFound are answers; ErrUnknown is not.
var (
	ErrRejected = errors.New("paystack: request refused")
	ErrNotFound = errors.New("paystack: not found")
	ErrUnknown  = errors.New("paystack: no usable answer; outcome unknown")
	ErrLiveKey  = errors.New("paystack: refusing a key that is not a test key (sk_test_…)")
)

// WebhookIPs are the addresses Paystack sends webhooks from, in test and
// live mode. Requests from anywhere else are not Paystack.
var WebhookIPs = []string{"52.31.139.75", "52.49.173.169", "52.214.14.220"}

// Client talks to Paystack with one secret key.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// New returns a client for a test-mode secret key. baseURL "" means
// DefaultBaseURL (tests point it at a fake server).
func New(secretKey, baseURL string, timeout time.Duration) (*Client, error) {
	if !strings.HasPrefix(secretKey, safety.PaystackTestKeyPrefix) {
		return nil, ErrLiveKey
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), key: secretKey, http: &http.Client{Timeout: timeout}}, nil
}

// String never shows the key.
func (c *Client) String() string { return "paystack.Client(" + c.baseURL + ")" }

// LogValue keeps the key out of structured logs.
func (c *Client) LogValue() slog.Value { return slog.StringValue(c.String()) }

type envelope struct {
	Status  bool            `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// call sends a request and classifies the answer. A 2xx with status true
// returns its data. A 4xx with a readable body is ErrRejected (or
// ErrNotFound for 404) carrying Paystack's message. Everything else is
// ErrUnknown.
func (c *Client) call(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %s", ErrUnknown, method, path, redact(err.Error(), c.key))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: %s %s: reading body: %w", ErrUnknown, method, path, err)
	}
	var env envelope
	jerr := json.Unmarshal(raw, &env)
	switch {
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusTooManyRequests:
		// 429 is "try later", not a refusal: treated like no answer.
		return fmt.Errorf("%w: %s %s: HTTP %d", ErrUnknown, method, path, resp.StatusCode)
	case jerr != nil:
		return fmt.Errorf("%w: %s %s: HTTP %d with an unreadable body", ErrUnknown, method, path, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, env.Message)
	case resp.StatusCode >= 400 || !env.Status:
		return fmt.Errorf("%w: HTTP %d: %s", ErrRejected, resp.StatusCode, env.Message)
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("%w: %s %s: unexpected data: %w", ErrUnknown, method, path, err)
		}
	}
	return nil
}

func redact(s, key string) string { return strings.ReplaceAll(s, key, "[redacted]") }

// ResolveAccount asks the bank for an account's name (GET /bank/resolve).
// It implements bankverify.Resolver: a refusal is ErrAccountNotFound, no
// answer is bankverify.ErrUnavailable.
func (c *Client) ResolveAccount(ctx context.Context, a bankverify.Account) (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	q := url.Values{"account_number": {a.AccountNumber}, "bank_code": {a.BankCode}}
	var data struct {
		AccountName string `json:"account_name"`
	}
	err := c.call(ctx, http.MethodGet, "/bank/resolve?"+q.Encode(), nil, &data)
	switch {
	case errors.Is(err, ErrRejected), errors.Is(err, ErrNotFound):
		return "", fmt.Errorf("%w: %w", bankverify.ErrAccountNotFound, err)
	case err != nil:
		return "", fmt.Errorf("%w: %w", bankverify.ErrUnavailable, err)
	case strings.TrimSpace(data.AccountName) == "":
		return "", fmt.Errorf("%w: resolved without a name", bankverify.ErrUnavailable)
	}
	return data.AccountName, nil
}

// CreateRecipient registers a NUBAN account for transfers (POST
// /transferrecipient) and returns its recipient code (RCP_…). Paystack
// returns the existing recipient for an account it already has.
func (c *Client) CreateRecipient(ctx context.Context, name string, a bankverify.Account) (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	var data struct {
		RecipientCode string `json:"recipient_code"`
	}
	err := c.call(ctx, http.MethodPost, "/transferrecipient", map[string]string{
		"type": "nuban", "name": name, "account_number": a.AccountNumber, "bank_code": a.BankCode, "currency": "NGN",
	}, &data)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(data.RecipientCode, "RCP_") {
		return "", fmt.Errorf("%w: recipient code %q", ErrUnknown, data.RecipientCode)
	}
	return data.RecipientCode, nil
}

// reference is Paystack's rule: lowercase letters, digits, - and _, at
// most 100 characters. Waybill uses the payout id, so at least 16.
var reference = regexp.MustCompile(`^[a-z0-9_-]{16,100}$`)

// Transfer is a naira transfer to a recipient.
type Transfer struct {
	Reference string       // the payout id: the same on every attempt
	Amount    money.Amount // kobo
	Recipient string       // RCP_…
	Reason    string
}

// TransferStatus is Paystack's view of a transfer.
type TransferStatus struct {
	Status       string // pending, success, failed, reversed, otp, …
	TransferCode string // TRF_…
	Reference    string
	Amount       money.Amount // kobo
}

// Initiate sends a transfer from the Paystack balance (POST /transfer).
// Retrying with the same reference never pays twice: Paystack treats it as
// the same transfer. ErrUnknown means it may or may not have been sent;
// call Verify with the reference before doing anything else.
func (c *Client) Initiate(ctx context.Context, t Transfer) (TransferStatus, error) {
	if !reference.MatchString(t.Reference) {
		return TransferStatus{}, fmt.Errorf("paystack: reference %q must be 16 to 100 lowercase letters, digits, - or _", t.Reference)
	}
	if t.Amount.Sign() <= 0 {
		return TransferStatus{}, fmt.Errorf("paystack: amount %s kobo is not positive", t.Amount)
	}
	var data transferData
	err := c.call(ctx, http.MethodPost, "/transfer", map[string]any{
		"source": "balance", "amount": json.Number(t.Amount.String()), "recipient": t.Recipient,
		"reference": t.Reference, "reason": t.Reason, "currency": "NGN",
	}, &data)
	if err != nil {
		return TransferStatus{}, err
	}
	return data.status(t.Reference)
}

// Verify looks a transfer up by reference (GET /transfer/verify/:reference).
// ErrNotFound means Paystack has no transfer with this reference: it was
// never sent, and it is safe to send with the same reference.
func (c *Client) Verify(ctx context.Context, ref string) (TransferStatus, error) {
	if !reference.MatchString(ref) {
		return TransferStatus{}, fmt.Errorf("paystack: reference %q is not valid", ref)
	}
	var data transferData
	err := c.call(ctx, http.MethodGet, "/transfer/verify/"+url.PathEscape(ref), nil, &data)
	if errors.Is(err, ErrRejected) && strings.Contains(strings.ToLower(err.Error()), "not found") {
		return TransferStatus{}, fmt.Errorf("%w: %w", ErrNotFound, err) // Paystack reports a missing transfer as a 400
	}
	if err != nil {
		return TransferStatus{}, err
	}
	return data.status(ref)
}

type transferData struct {
	Status       string      `json:"status"`
	TransferCode string      `json:"transfer_code"`
	Reference    string      `json:"reference"`
	Amount       json.Number `json:"amount"`
}

func (d transferData) status(wantRef string) (TransferStatus, error) {
	if d.Reference != "" && d.Reference != wantRef {
		return TransferStatus{}, fmt.Errorf("%w: answer is for reference %q, asked about %q", ErrUnknown, d.Reference, wantRef)
	}
	amt := money.Zero
	if d.Amount != "" {
		a, err := money.ParseMinor(d.Amount.String())
		if err != nil {
			return TransferStatus{}, fmt.Errorf("%w: amount %q", ErrUnknown, d.Amount)
		}
		amt = a
	}
	return TransferStatus{Status: d.Status, TransferCode: d.TransferCode, Reference: wantRef, Amount: amt}, nil
}

// ErrBadSignature means the webhook was not signed by this Paystack account.
var ErrBadSignature = errors.New("paystack: bad webhook signature")

// VerifyWebhook checks x-paystack-signature: the hex HMAC-SHA512 of the
// raw request body with the secret key. Check it before parsing the body,
// and treat a failure as a forgery.
func (c *Client) VerifyWebhook(body []byte, signature string) error {
	m := hmac.New(sha512.New, []byte(c.key))
	m.Write(body)
	want := hex.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(strings.ToLower(strings.TrimSpace(signature))), []byte(want)) {
		return ErrBadSignature
	}
	return nil
}

// ListBanks returns the Nigerian banks that can receive NGN transfers
// (GET /bank). It implements bankverify.BankLister.
func (c *Client) ListBanks(ctx context.Context) ([]bankverify.Bank, error) {
	var data []struct {
		Name   string `json:"name"`
		Code   string `json:"code"`
		Active bool   `json:"active"`
	}
	if err := c.call(ctx, http.MethodGet, "/bank?country=nigeria&currency=NGN&perPage=100", nil, &data); err != nil {
		return nil, err
	}
	out := make([]bankverify.Bank, 0, len(data))
	for _, b := range data {
		if b.Active && b.Code != "" {
			out = append(out, bankverify.Bank{Code: b.Code, Name: b.Name})
		}
	}
	return out, nil
}
