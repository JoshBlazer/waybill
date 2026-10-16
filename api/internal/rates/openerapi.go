package rates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/money"
)

// OpenERAPIURL is ExchangeRate-API's keyless endpoint for USD rates
// (ADR-039). It updates once a day; its terms require attribution, which
// Source carries to the page.
const OpenERAPIURL = "https://open.er-api.com/v6/latest/USD"

// OpenERAPI reads USD→NGN from ExchangeRate-API and applies it to USD
// stablecoins 1:1 (USDC and USDT). Responses are cached for CacheFor, so a
// burst of invoices makes one request.
type OpenERAPI struct {
	URL      string // OpenERAPIURL unless a test points it elsewhere
	Client   *http.Client
	CacheFor time.Duration

	mu      sync.Mutex
	cached  Observation
	fetched time.Time
}

// usdStable lists the stablecoins treated as exactly one US dollar. A depeg
// is a known gap (RISKS.md).
var usdStable = map[money.AssetCode]bool{money.USDC.Code: true, money.USDT.Code: true}

// Mid returns the NGN rate for a USD stablecoin.
func (o *OpenERAPI) Mid(ctx context.Context, p Pair) (Observation, error) {
	if !usdStable[p.From] || p.To != money.NGN.Code {
		return Observation{}, fmt.Errorf("%w: %s", ErrUnsupportedPair, p)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.fetched.IsZero() && time.Since(o.fetched) < o.CacheFor {
		obs := o.cached
		obs.Pair = p
		return obs, nil
	}
	obs, err := o.fetch(ctx)
	if err != nil {
		return Observation{}, err
	}
	o.cached, o.fetched = obs, time.Now()
	obs.Pair = p
	return obs, nil
}

func (o *OpenERAPI) fetch(ctx context.Context) (Observation, error) {
	url := o.URL
	if url == "" {
		url = OpenERAPIURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Observation{}, err
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return Observation{}, fmt.Errorf("exchangerate-api: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Observation{}, fmt.Errorf("exchangerate-api: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Result         string                 `json:"result"`
		BaseCode       string                 `json:"base_code"`
		TimeLastUpdate int64                  `json:"time_last_update_unix"`
		Rates          map[string]json.Number `json:"rates"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.UseNumber() // rates stay decimal text; never parsed as floating point
	if err := dec.Decode(&body); err != nil {
		return Observation{}, fmt.Errorf("exchangerate-api: decode: %w", err)
	}
	if body.Result != "success" || body.BaseCode != "USD" {
		return Observation{}, fmt.Errorf("exchangerate-api: result %q, base %q", body.Result, body.BaseCode)
	}
	ngn, ok := body.Rates["NGN"]
	if !ok {
		return Observation{}, fmt.Errorf("exchangerate-api: no NGN rate")
	}
	rate, err := ParseRate(ngn.String(), money.NGN)
	if err != nil {
		return Observation{}, err
	}
	if body.TimeLastUpdate <= 0 {
		return Observation{}, fmt.Errorf("exchangerate-api: no update time")
	}
	return Observation{Rate: rate, Source: "ExchangeRate-API", ObservedAt: time.Unix(body.TimeLastUpdate, 0).UTC()}, nil
}
