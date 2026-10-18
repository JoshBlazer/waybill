// Package config loads service configuration from the environment. Secrets
// come only from the environment; nothing secret lives in the repository.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
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
	// DeploymentsDir holds <chainId>.json contract records.
	DeploymentsDir string
	// WebURL is the public base URL of the web app (payment and tracking links).
	WebURL string
	// DevContractorKey is the API key seed-dev installs for the local demo
	// contractor. Local stacks only.
	DevContractorKey string
	// DevAdminKey and DevApproverKey are the API keys seed-dev installs for
	// the demo organisation's two members (stage 4). Optional.
	DevAdminKey    string
	DevApproverKey string
	// WebhookAllowPrivate lets webhook endpoints use http and private hosts
	// (WAYBILL_WEBHOOK_ALLOW_PRIVATE=1). Refused unless every chain is Anvil.
	WebhookAllowPrivate bool
	// SignerKey (WAYBILL_SIGNER_KEY, hex) or SignerKeyFile
	// (WAYBILL_SIGNER_KEY_FILE) is the key the worker signs sweeps and
	// payouts with. Unset means the worker sends nothing. Test networks only,
	// like everything else.
	SignerKey     Secret
	SignerKeyFile string
	// Rates is the naira rate source (ADR-039): "exchangerate-api" or
	// "fake" (WAYBILL_RATES, default fake). RateSpreadBps is Waybill's
	// margin (WAYBILL_RATE_SPREAD_BPS, default 150).
	Rates         string
	RateSpreadBps int64
	// BTCXPubs (WAYBILL_BTC_XPUB_REGTEST, WAYBILL_BTC_XPUB_SIGNET) are the
	// extended public keys Bitcoin addresses are derived from; or, from a
	// file, BTCXPubFiles (the same names with _FILE). Never a private key
	// (I15, ADR-045).
	BTCXPubs     map[safety.Network]string
	BTCXPubFiles map[safety.Network]string
}

// btcXPubEnv names each Bitcoin network's xpub variable.
var btcXPubEnv = map[safety.Network]string{
	safety.BTCRegtest: "WAYBILL_BTC_XPUB_REGTEST",
	safety.BTCSignet:  "WAYBILL_BTC_XPUB_SIGNET",
}

// BTCXPub returns the extended public key configured for n, reading the
// file variant if that is the one set.
func (c Config) BTCXPub(n safety.Network) (string, error) {
	name := btcXPubEnv[n]
	v, f := c.BTCXPubs[n], c.BTCXPubFiles[n]
	switch {
	case v != "" && f != "":
		return "", fmt.Errorf("config: set %s or %s_FILE, not both", name, name)
	case f != "":
		raw, err := os.ReadFile(f) //nolint:gosec // a path the operator configured
		if err != nil {
			return "", fmt.Errorf("config: %s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(raw)), nil
	case v != "":
		return v, nil
	}
	return "", fmt.Errorf("config: %s is configured but %s is not set", n, name)
}

// Secret is a string that never prints: fmt and slog show "[redacted]".
type Secret string

// String hides the value.
func (Secret) String() string { return "[redacted]" }

// GoString hides the value under %#v too.
func (Secret) GoString() string { return "[redacted]" }

// LogValue hides the value in structured logs.
func (Secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// MarshalJSON hides the value when a struct holding it is encoded, as slog's
// JSON handler does with a logged Config: LogValue covers only top-level
// attributes, not nested fields.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// MarshalText hides the value from text encoders.
func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

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
		DeploymentsDir:    withDefault(getenv("WAYBILL_DEPLOYMENTS_DIR"), "../contracts/deployments"),
		WebURL:            strings.TrimRight(withDefault(getenv("WAYBILL_WEB_URL"), "http://localhost:3000"), "/"),
		DevContractorKey:  getenv("WAYBILL_DEV_CONTRACTOR_KEY"),
		DevAdminKey:       getenv("WAYBILL_DEV_ADMIN_KEY"),
		DevApproverKey:    getenv("WAYBILL_DEV_APPROVER_KEY"),
		SignerKey:         Secret(getenv("WAYBILL_SIGNER_KEY")),
		Rates:             withDefault(getenv("WAYBILL_RATES"), "fake"),
		RateSpreadBps:     150,
		SignerKeyFile:     getenv("WAYBILL_SIGNER_KEY_FILE"),
		BTCXPubs:          map[safety.Network]string{},
		BTCXPubFiles:      map[safety.Network]string{},
	}
	for n, name := range btcXPubEnv {
		if v := getenv(name); v != "" {
			cfg.BTCXPubs[n] = v
		}
		if v := getenv(name + "_FILE"); v != "" {
			cfg.BTCXPubFiles[n] = v
		}
	}
	if v := getenv("WAYBILL_RATE_SPREAD_BPS"); v != "" {
		bps, err := strconv.ParseInt(v, 10, 64)
		if err != nil || bps < 0 || bps > 9999 {
			return Config{}, fmt.Errorf("config: WAYBILL_RATE_SPREAD_BPS must be 0 to 9999, not %q", v)
		}
		cfg.RateSpreadBps = bps
	}
	if cfg.SignerKey != "" && cfg.SignerKeyFile != "" {
		return Config{}, errors.New("config: set WAYBILL_SIGNER_KEY or WAYBILL_SIGNER_KEY_FILE, not both")
	}
	if getenv("WAYBILL_WEBHOOK_ALLOW_PRIVATE") == "1" {
		for _, c := range chains {
			if c.Network != safety.EVMAnvil {
				return Config{}, fmt.Errorf("config: WAYBILL_WEBHOOK_ALLOW_PRIVATE is for local Anvil stacks only; %s is configured", c.Network)
			}
		}
		cfg.WebhookAllowPrivate = true
	}
	return cfg, nil
}

// Safety returns the part of the configuration the test-money guard checks.
func (c Config) Safety() safety.Config {
	return safety.Config{Chains: c.Chains, PaystackSecretKey: c.PaystackSecretKey}
}

// NetworkIDs lists the configured networks.
func (c Config) NetworkIDs() []safety.Network {
	out := make([]safety.Network, len(c.Chains))
	for i, ch := range c.Chains {
		out[i] = ch.Network
	}
	return out
}

// RPCURL returns the primary RPC endpoint configured for n.
func (c Config) RPCURL(n safety.Network) string {
	for _, ch := range c.Chains {
		if ch.Network == n {
			return ch.RPCURL
		}
	}
	return ""
}

// RPCURLs returns every RPC endpoint configured for n, primary first.
func (c Config) RPCURLs(n safety.Network) []string {
	for _, ch := range c.Chains {
		if ch.Network == n {
			return ch.URLs()
		}
	}
	return nil
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
// "<network>=<rpc url>[|<fallback url>...]", for example
// "evm:31337=http://anvil:8545,evm:84532=https://sepolia.base.org|https://base-sepolia-rpc.publicnode.com".
// The first URL of each network is the primary (ADR-045).
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
		urls := strings.Split(rpc, "|")
		for j, u := range urls {
			urls[j] = strings.TrimSpace(u)
			if urls[j] == "" {
				return nil, fmt.Errorf("WAYBILL_CHAINS entry %d has an empty endpoint", i+1)
			}
		}
		ch := safety.Chain{Network: safety.Network(network), RPCURL: urls[0]}
		if len(urls) > 1 {
			ch.Fallbacks = urls[1:]
		}
		chains = append(chains, ch)
	}
	return chains, nil
}

func withDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
