package btc

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/money"
)

// Output is one transaction output paying an address.
type Output struct {
	TxID    string
	Vout    uint32
	Address string
	Amount  money.Amount // satoshis
}

// Chain is what the watcher and reconciliation read from a Bitcoin node or
// explorer. Heights and hashes refer to the best chain at the time of the
// call.
type Chain interface {
	Height(ctx context.Context) (uint64, error)
	BlockHash(ctx context.Context, height uint64) (string, error)
	// BlockOutputs lists every output in the block that pays an address.
	BlockOutputs(ctx context.Context, hash string) ([]Output, error)
	// Unspent is the total unspent value held by the addresses.
	Unspent(ctx context.Context, addresses []string) (money.Amount, error)
}

// ErrNotFound means the node has no such block (a reorg or a race), an
// answer rather than a failure.
var ErrNotFound = errors.New("btc: not found")

// Dial returns a Chain for an endpoint URL:
//   - http(s)://user:pass@host:port[/wallet/name] is bitcoind JSON-RPC;
//   - esplora+https://host/path is an Esplora REST API, such as
//     esplora+https://mempool.space/signet/api.
func Dial(raw string) (Chain, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errors.New("btc: endpoint is not a valid URL")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	if base, ok := strings.CutPrefix(u.Scheme, "esplora+"); ok {
		u.Scheme = base
		return &Esplora{Base: strings.TrimRight(u.String(), "/"), Client: client}, nil
	}
	return &Bitcoind{URL: u, Client: client}, nil
}

// btcToSats converts a decimal BTC amount as written in JSON ("0.0001",
// "1e-05") to satoshis, exactly.
func btcToSats(s string) (money.Amount, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return money.Zero, fmt.Errorf("btc: bad amount %q", s)
	}
	r.Mul(r, big.NewRat(100_000_000, 1))
	if !r.IsInt() {
		return money.Zero, fmt.Errorf("btc: amount %q has more than 8 decimals", s)
	}
	return money.FromBig(r.Num())
}
