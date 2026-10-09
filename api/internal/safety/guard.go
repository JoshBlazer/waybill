// Package safety implements the test-money guard: every service refuses to
// start unless all configured chains are test networks and all payment
// provider keys are test-mode keys.
//
// The guard fails closed. A chain whose identity cannot be confirmed at
// startup is treated as unsafe.
package safety

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// Network identifies a chain as "<family>:<id>", for example "evm:84532" or
// "btc:regtest".
type Network string

// Allowed test networks. Nothing else may be configured.
const (
	EVMAnvil       Network = "evm:31337"
	EVMBaseSepolia Network = "evm:84532"
	EVMSepolia     Network = "evm:11155111"
	BTCRegtest     Network = "btc:regtest"
	BTCSignet      Network = "btc:signet"
)

var allowedNetworks = map[Network]bool{
	EVMAnvil:       true,
	EVMBaseSepolia: true,
	EVMSepolia:     true,
	BTCRegtest:     true,
	BTCSignet:      true,
}

// AllowedNetworks returns the allowlist, for documentation and error messages.
func AllowedNetworks() []Network {
	return []Network{EVMAnvil, EVMBaseSepolia, EVMSepolia, BTCRegtest, BTCSignet}
}

// PaystackTestKeyPrefix is the prefix every Paystack secret key must carry.
const PaystackTestKeyPrefix = "sk_test_"

// Chain is one configured chain and the RPC endpoint used to reach it.
type Chain struct {
	Network Network
	RPCURL  string
}

// Config is the subset of service configuration the guard inspects.
type Config struct {
	Chains            []Chain
	PaystackSecretKey string
}

// ChainIDProber asks an EVM RPC endpoint which chain it serves.
type ChainIDProber interface {
	ChainID(ctx context.Context, rpcURL string) (*big.Int, error)
}

// LiveCheckTimeout bounds each RPC probe so a hung endpoint cannot hang startup.
const LiveCheckTimeout = 10 * time.Second

// Check runs the static configuration check and then the live check. Any
// error means the service must not start.
func Check(ctx context.Context, cfg Config, prober ChainIDProber) error {
	if err := CheckConfig(cfg); err != nil {
		return err
	}
	return CheckLive(ctx, cfg, prober)
}

// CheckConfig verifies the configuration without touching the network. It
// reports every problem it finds, not only the first.
func CheckConfig(cfg Config) error {
	var errs []error
	seen := make(map[Network]bool)
	for _, c := range cfg.Chains {
		if !allowedNetworks[c.Network] {
			errs = append(errs, fmt.Errorf("network %q is not an allowed test network (allowed: %s)", c.Network, joinNetworks(AllowedNetworks())))
			continue
		}
		if seen[c.Network] {
			errs = append(errs, fmt.Errorf("network %q is configured more than once", c.Network))
		}
		seen[c.Network] = true
		if _, err := parseRPCURL(c.RPCURL); err != nil {
			errs = append(errs, fmt.Errorf("network %q: %w", c.Network, err))
		}
	}
	if k := cfg.PaystackSecretKey; k != "" && !strings.HasPrefix(k, PaystackTestKeyPrefix) {
		// Never echo the key, not even a prefix of it.
		errs = append(errs, fmt.Errorf("PAYSTACK_SECRET_KEY is not a test-mode key (must begin with %q)", PaystackTestKeyPrefix))
	}
	return errors.Join(errs...)
}

// CheckLive confirms that each configured chain's RPC really serves the
// configured network. An unreachable RPC is a failure: "couldn't find out" is
// not "safe". Networks without a live check fail closed.
func CheckLive(ctx context.Context, cfg Config, prober ChainIDProber) error {
	var errs []error
	for _, c := range cfg.Chains {
		host := RedactedHost(c.RPCURL)
		family, id, _ := strings.Cut(string(c.Network), ":")
		switch family {
		case "evm":
			want, ok := new(big.Int).SetString(id, 10)
			if !ok {
				errs = append(errs, fmt.Errorf("network %q: invalid EVM chain id", c.Network))
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, LiveCheckTimeout)
			got, err := prober.ChainID(pctx, c.RPCURL)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Errorf("network %q: could not confirm chain id from RPC at %s: %w", c.Network, host, redactErr(err, c.RPCURL)))
				continue
			}
			if got.Cmp(want) != 0 {
				errs = append(errs, fmt.Errorf("network %q: RPC at %s reports chain id %s, want %s", c.Network, host, got, want))
			}
		default:
			errs = append(errs, fmt.Errorf("network %q: no live check is implemented for %q networks yet; refusing to start", c.Network, family))
		}
	}
	return errors.Join(errs...)
}

// RedactedHost returns only the host of an RPC URL. RPC URLs often embed
// API keys in the path or query, so full URLs never appear in errors or logs.
func RedactedHost(rpcURL string) string {
	u, err := url.Parse(rpcURL)
	if err != nil || u.Host == "" {
		return "<invalid url>"
	}
	return u.Host
}

func parseRPCURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errors.New("RPC URL is not a valid absolute URL")
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
		return u, nil
	default:
		return nil, fmt.Errorf("RPC URL scheme %q is not supported", u.Scheme)
	}
}

// redactErr removes the full RPC URL from an underlying error message.
func redactErr(err error, rpcURL string) error {
	if rpcURL == "" || !strings.Contains(err.Error(), rpcURL) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), rpcURL, RedactedHost(rpcURL)))
}

func joinNetworks(ns []Network) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = string(n)
	}
	return strings.Join(s, ", ")
}
