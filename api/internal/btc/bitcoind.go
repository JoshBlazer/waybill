package btc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"

	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// Bitcoind reads a bitcoind node over JSON-RPC.
type Bitcoind struct {
	URL    *url.URL // credentials in the userinfo; never logged
	Client *http.Client
	id     atomic.Int64
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("bitcoind: %s (code %d)", e.Message, e.Code) }

// call makes one JSON-RPC call. Numbers are decoded as json.Number, so
// amounts are never rounded through a float.
func (b *Bitcoind) call(ctx context.Context, method string, out any, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": b.id.Add(1), "method": method, "params": params})
	if err != nil {
		return err
	}
	u := *b.URL
	u.User = nil
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	if b.URL.User != nil {
		pass, _ := b.URL.User.Password()
		req.SetBasicAuth(b.URL.User.Username(), pass)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := b.Client.Do(req)
	if err != nil {
		return fmt.Errorf("bitcoind at %s: %w", safety.RedactedHost(u.String()), err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil {
		return fmt.Errorf("bitcoind %s: HTTP %d: undecodable response", method, res.StatusCode)
	}
	if env.Error != nil {
		if env.Error.Code == -5 || env.Error.Code == -8 { // not found, or height out of range
			return fmt.Errorf("%w: %s", ErrNotFound, env.Error.Message)
		}
		return env.Error
	}
	if out == nil {
		return nil
	}
	dec = json.NewDecoder(bytes.NewReader(env.Result))
	dec.UseNumber()
	return dec.Decode(out)
}

// Height is the best block height.
func (b *Bitcoind) Height(ctx context.Context) (uint64, error) {
	var n uint64
	return n, b.call(ctx, "getblockcount", &n)
}

// BlockHash is the hash of the best chain's block at height.
func (b *Bitcoind) BlockHash(ctx context.Context, height uint64) (string, error) {
	var h string
	return h, b.call(ctx, "getblockhash", &h, height)
}

// BlockOutputs lists the outputs of every transaction in the block.
func (b *Bitcoind) BlockOutputs(ctx context.Context, hash string) ([]Output, error) {
	var blk struct {
		Tx []struct {
			TxID string `json:"txid"`
			Vout []struct {
				N            uint32      `json:"n"`
				Value        json.Number `json:"value"`
				ScriptPubKey struct {
					Address string `json:"address"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
		} `json:"tx"`
	}
	if err := b.call(ctx, "getblock", &blk, hash, 2); err != nil {
		return nil, err
	}
	var out []Output
	for _, tx := range blk.Tx {
		for _, v := range tx.Vout {
			if v.ScriptPubKey.Address == "" {
				continue // OP_RETURN and other outputs with no address
			}
			sats, err := btcToSats(v.Value.String())
			if err != nil {
				return nil, err
			}
			out = append(out, Output{TxID: tx.TxID, Vout: v.N, Address: v.ScriptPubKey.Address, Amount: sats})
		}
	}
	return out, nil
}

// Unspent sums the addresses' unspent outputs with scantxoutset, which
// needs no wallet and no index.
func (b *Bitcoind) Unspent(ctx context.Context, addresses []string) (money.Amount, error) {
	if len(addresses) == 0 {
		return money.Zero, nil
	}
	descs := make([]string, len(addresses))
	for i, a := range addresses {
		descs[i] = "addr(" + a + ")"
	}
	var res struct {
		Success     bool        `json:"success"`
		TotalAmount json.Number `json:"total_amount"`
	}
	if err := b.call(ctx, "scantxoutset", &res, "start", descs); err != nil {
		return money.Zero, err
	}
	if !res.Success {
		return money.Zero, fmt.Errorf("bitcoind: scantxoutset did not finish")
	}
	return btcToSats(res.TotalAmount.String())
}
