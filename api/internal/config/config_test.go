package config

import (
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
