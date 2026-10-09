// Package config loads service configuration from the environment. Secrets
// come only from the environment; nothing secret lives in the repository.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// Config is the configuration shared by every subcommand.
type Config struct {
	// DatabaseURL is a PostgreSQL connection string. It may contain a
	// password, so it is never logged.
	DatabaseURL string
	// HTTPAddr is the API listen address.
	HTTPAddr string
	// Chains are the configured networks and their RPC endpoints.
	Chains []safety.Chain
	// PaystackSecretKey must be a test-mode key when set.
	PaystackSecretKey string
	// Version is the build version reported by /v1/health.
	Version string
}

// Getenv matches os.Getenv so tests can supply their own environment.
type Getenv func(string) string

// Load reads configuration from the process environment.
func Load() (Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom reads configuration using getenv.
func LoadFrom(getenv Getenv) (Config, error) {
	chains, err := ParseChains(getenv("WAYBILL_CHAINS"))
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		DatabaseURL:       getenv("DATABASE_URL"),
		HTTPAddr:          withDefault(getenv("HTTP_ADDR"), ":8080"),
		Chains:            chains,
		PaystackSecretKey: getenv("PAYSTACK_SECRET_KEY"),
		Version:           withDefault(getenv("WAYBILL_VERSION"), "0.1.0-dev"),
	}
	return cfg, nil
}

// Safety returns the part of the configuration the test-money guard checks.
func (c Config) Safety() safety.Config {
	return safety.Config{Chains: c.Chains, PaystackSecretKey: c.PaystackSecretKey}
}

// Networks lists the configured network identifiers.
func (c Config) Networks() []string {
	out := make([]string, len(c.Chains))
	for i, ch := range c.Chains {
		out[i] = string(ch.Network)
	}
	return out
}

// RequireDatabase returns an error when DATABASE_URL is unset.
func (c Config) RequireDatabase() error {
	if c.DatabaseURL == "" {
		return errors.New("DATABASE_URL is not set")
	}
	return nil
}

// ParseChains parses WAYBILL_CHAINS: a comma-separated list of
// "<network>=<rpc url>", for example
// "evm:31337=http://anvil:8545,evm:84532=https://sepolia.base.org".
// Whether a network is allowed is decided by the safety package, not here.
func ParseChains(raw string) ([]safety.Chain, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var chains []safety.Chain
	for i, item := range strings.Split(raw, ",") {
		network, rpc, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || network == "" || rpc == "" {
			// Report the position, not the item: it may contain an RPC key.
			return nil, fmt.Errorf("WAYBILL_CHAINS entry %d is not of the form <network>=<rpc url>", i+1)
		}
		chains = append(chains, safety.Chain{Network: safety.Network(network), RPCURL: rpc})
	}
	return chains, nil
}

func withDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
