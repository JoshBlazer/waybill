package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/bankverify"
	"github.com/JoshBlazer/waybill/api/internal/checkout"
	"github.com/JoshBlazer/waybill/api/internal/config"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/paystack"
	"github.com/JoshBlazer/waybill/api/internal/rates"
	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// fakeRate is the local rate when WAYBILL_RATES=fake: ₦1,550.00 per USDC.
var fakeRate = money.FromInt64(155_000)

// demoAccountHolder is the name every account resolves to on a stack
// without a Paystack key: the demo contractor seed-dev installs.
const demoAccountHolder = "Adaeze Okafor"

// naira holds the stage 3 providers chosen by configuration.
type naira struct {
	quoter   *rates.Quoter
	banks    bankverify.BankLister
	resolver bankverify.Resolver
	paystack *paystack.Client // nil without a key
	charger  checkout.Charger // nil without a key: card pay-in is off
}

func (n naira) checkout(pool *pgxpool.Pool, tokens map[safety.Network]string, webURL string, log *slog.Logger) *checkout.Service {
	return &checkout.Service{Pool: pool, Charger: n.charger, Tokens: tokens, WebURL: webURL, Log: log}
}

func setupNaira(cfg config.Config, log *slog.Logger) (naira, error) {
	var provider rates.Provider
	switch cfg.Rates {
	case "exchangerate-api":
		provider = &rates.OpenERAPI{Client: &http.Client{Timeout: 10 * time.Second}, CacheFor: time.Hour}
	case "fake":
		provider = &rates.Fake{Rates: map[rates.Pair]money.Amount{invoice.NairaPair: fakeRate}}
	default:
		return naira{}, fmt.Errorf("config: WAYBILL_RATES must be exchangerate-api or fake, not %q", cfg.Rates)
	}
	n := naira{quoter: &rates.Quoter{
		Provider: provider, SpreadBps: cfg.RateSpreadBps, TTL: 48 * time.Hour, MaxAge: 26 * time.Hour,
	}}
	if cfg.PaystackSecretKey == "" {
		fake := &bankverify.Fake{AnyName: demoAccountHolder}
		n.banks, n.resolver = fake, fake
		log.Info("naira: no Paystack key; banks and account names are fake", "rates", cfg.Rates)
		return n, nil
	}
	c, err := paystack.New(cfg.PaystackSecretKey, "", 20*time.Second)
	if err != nil {
		return naira{}, err
	}
	n.banks, n.resolver, n.paystack, n.charger = c, c, c, c
	log.Info("naira: Paystack test mode", "client", c, "rates", cfg.Rates)
	return n, nil
}
