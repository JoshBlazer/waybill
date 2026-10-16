package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/JoshBlazer/waybill/api/internal/safety"
)

func env(m map[string]string) Getenv { return func(k string) string { return m[k] } }

func TestParseChains(t *testing.T) {
	got, err := ParseChains(" evm:31337=http://anvil:8545 , evm:84532=https://sepolia.base.org ")
	if err != nil {
		t.Fatal(err)
	}
	want := []safety.Chain{
		{Network: "evm:31337", RPCURL: "http://anvil:8545"},
		{Network: "evm:84532", RPCURL: "https://sepolia.base.org"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d chains, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("chain %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseChains_Empty(t *testing.T) {
	got, err := ParseChains("")
	if err != nil || got != nil {
		t.Fatalf("ParseChains(\"\") = %v, %v", got, err)
	}
}

func TestParseChains_MalformedDoesNotLeakEntry(t *testing.T) {
	_, err := ParseChains("evm:31337=http://ok:8545,https://rpc.example/SECRETKEY")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "SECRETKEY") {
		t.Fatalf("error leaks entry: %q", err)
	}
}

func TestLoadFrom_Defaults(t *testing.T) {
	cfg, err := LoadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.Version != "0.1.0-dev" {
		t.Fatalf("defaults = %+v", cfg)
	}
	if err := cfg.RequireDatabase(); err == nil {
		t.Fatal("RequireDatabase passed with DATABASE_URL unset")
	}
}

func TestLoadFrom_WebhookAllowPrivateOnlyOnAnvil(t *testing.T) {
	cfg, err := LoadFrom(env(map[string]string{
		"WAYBILL_CHAINS": "evm:31337=http://anvil:8545", "WAYBILL_WEBHOOK_ALLOW_PRIVATE": "1",
	}))
	if err != nil || !cfg.WebhookAllowPrivate {
		t.Fatalf("on Anvil: %+v, %v; want allowed", cfg.WebhookAllowPrivate, err)
	}
	_, err = LoadFrom(env(map[string]string{
		"WAYBILL_CHAINS": "evm:31337=http://anvil:8545,evm:84532=https://sepolia.base.org", "WAYBILL_WEBHOOK_ALLOW_PRIVATE": "1",
	}))
	if err == nil || !strings.Contains(err.Error(), "evm:84532") {
		t.Fatalf("with Base Sepolia configured: err = %v; want a refusal naming the network", err)
	}
	cfg, err = LoadFrom(env(map[string]string{"WAYBILL_CHAINS": "evm:84532=https://sepolia.base.org"}))
	if err != nil || cfg.WebhookAllowPrivate {
		t.Fatalf("unset: %+v, %v; want off", cfg.WebhookAllowPrivate, err)
	}
}

func TestLoadFrom_SignerKeyNeverPrints(t *testing.T) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	key := "0x" + hex.EncodeToString(raw)
	cfg, err := LoadFrom(env(map[string]string{"WAYBILL_SIGNER_KEY": key}))
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("config", "cfg", cfg, "key", cfg.SignerKey)
	out := fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg) + logs.String()
	if strings.Contains(out, key[2:]) {
		t.Fatal("the signing key appears in formatted config or logs")
	}
	if string(cfg.SignerKey) != key {
		t.Fatal("the key itself was not kept")
	}
	if _, err := LoadFrom(env(map[string]string{"WAYBILL_SIGNER_KEY": key, "WAYBILL_SIGNER_KEY_FILE": "/k"})); err == nil {
		t.Fatal("both key settings accepted; want an error")
	}
}

func TestLoadFrom_Rates(t *testing.T) {
	cfg, err := LoadFrom(env(map[string]string{}))
	if err != nil || cfg.Rates != "fake" || cfg.RateSpreadBps != 150 {
		t.Fatalf("defaults: %q %d %v; want fake, 150", cfg.Rates, cfg.RateSpreadBps, err)
	}
	cfg, err = LoadFrom(env(map[string]string{"WAYBILL_RATES": "exchangerate-api", "WAYBILL_RATE_SPREAD_BPS": "0"}))
	if err != nil || cfg.Rates != "exchangerate-api" || cfg.RateSpreadBps != 0 {
		t.Fatalf("set: %q %d %v", cfg.Rates, cfg.RateSpreadBps, err)
	}
	for _, bad := range []string{"-1", "10000", "1.5", "x"} {
		if _, err := LoadFrom(env(map[string]string{"WAYBILL_RATE_SPREAD_BPS": bad})); err == nil {
			t.Errorf("spread %q accepted", bad)
		}
	}
}
