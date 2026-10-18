package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/btc"
	"github.com/JoshBlazer/waybill/api/internal/btcwatch"
	"github.com/JoshBlazer/waybill/api/internal/config"
	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// liveProber is the test-money guard's live check: chain id for EVM
// endpoints, block 0 for Bitcoin endpoints.
type liveProber struct{ safety.EVMProber }

func (liveProber) Genesis(ctx context.Context, rpcURL string) (string, error) {
	c, err := btc.Dial(rpcURL)
	if err != nil {
		return "", err
	}
	return c.BlockHash(ctx, 0)
}

// btcNetworks returns the configured Bitcoin networks with their address
// derivers and chain clients. Any problem refuses startup: an unusable xpub
// would hand out addresses nobody can spend from.
func btcNetworks(cfg config.Config) (map[safety.Network]btcNetwork, error) {
	out := map[safety.Network]btcNetwork{}
	for _, ch := range cfg.Chains {
		if !strings.HasPrefix(string(ch.Network), "btc:") {
			continue
		}
		raw, err := cfg.BTCXPub(ch.Network)
		if err != nil {
			return nil, err
		}
		d, err := btc.ParseXPub(ch.Network, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ch.Network, err)
		}
		c, err := btc.Dial(ch.RPCURL)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ch.Network, err)
		}
		out[ch.Network] = btcNetwork{deriver: d, chain: c}
	}
	return out, nil
}

type btcNetwork struct {
	deriver *btc.Deriver
	chain   btc.Chain
}

func btcDerivers(nets map[safety.Network]btcNetwork) map[safety.Network]*btc.Deriver {
	out := make(map[safety.Network]*btc.Deriver, len(nets))
	for n, b := range nets {
		out[n] = b.deriver
	}
	return out
}

// btcWatcher is how each Bitcoin network is watched: booked in a block,
// final at 3 blocks on regtest and 6 on signet. A first run starts at the
// current height: every address is fresh, so there is no history to read.
func btcWatcher(ctx context.Context, pool *pgxpool.Pool, n safety.Network, b btcNetwork, log *slog.Logger) (*btcwatch.Watcher, error) {
	cfg := btcwatch.Config{Network: n, Confirmations: 1, Finality: 6, PollInterval: 30 * time.Second}
	if n == safety.BTCRegtest {
		cfg.Finality, cfg.PollInterval = 3, 2*time.Second
	}
	head, err := b.chain.Height(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: height: %w", n, err)
	}
	cfg.StartHeight = head
	return &btcwatch.Watcher{Pool: pool, Chain: b.chain, Cfg: cfg, Log: log.With("network", n)}, nil
}
