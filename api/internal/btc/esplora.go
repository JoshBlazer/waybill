package btc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/JoshBlazer/waybill/api/internal/money"
)

// Esplora reads an Esplora REST API (mempool.space, Blockstream). It needs
// no key; a third party serves it (ADR-045).
type Esplora struct {
	Base   string // e.g. https://mempool.space/signet/api
	Client *http.Client
}

func (e *Esplora) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.Base+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := e.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("esplora: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case res.StatusCode == http.StatusNotFound, res.StatusCode == http.StatusBadRequest:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	case res.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("esplora %s: HTTP %d", path, res.StatusCode)
	}
	return body, nil
}

// Height is the tip height.
func (e *Esplora) Height(ctx context.Context) (uint64, error) {
	b, err := e.get(ctx, "/blocks/tip/height")
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// BlockHash is the hash of the best chain's block at height.
func (e *Esplora) BlockHash(ctx context.Context, height uint64) (string, error) {
	b, err := e.get(ctx, "/block-height/"+strconv.FormatUint(height, 10))
	if err != nil {
		return "", err
	}
	h := strings.TrimSpace(string(b))
	if len(h) != 64 {
		return "", fmt.Errorf("esplora: block hash %q", h)
	}
	return h, nil
}

type esploraTx struct {
	TxID string `json:"txid"`
	Vout []struct {
		Address string `json:"scriptpubkey_address"`
		Value   int64  `json:"value"` // satoshis
	} `json:"vout"`
}

// BlockOutputs pages through the block's transactions, 25 at a time.
func (e *Esplora) BlockOutputs(ctx context.Context, hash string) ([]Output, error) {
	b, err := e.get(ctx, "/block/"+hash)
	if err != nil {
		return nil, err
	}
	var blk struct {
		TxCount int `json:"tx_count"`
	}
	if err := json.Unmarshal(b, &blk); err != nil {
		return nil, err
	}
	var out []Output
	for start := 0; start < blk.TxCount; start += 25 {
		page, err := e.get(ctx, "/block/"+hash+"/txs/"+strconv.Itoa(start))
		if err != nil {
			return nil, err
		}
		var txs []esploraTx
		if err := json.Unmarshal(page, &txs); err != nil {
			return nil, err
		}
		for _, tx := range txs {
			for n, v := range tx.Vout {
				if v.Address == "" {
					continue
				}
				out = append(out, Output{TxID: tx.TxID, Vout: uint32(n), Address: v.Address, Amount: money.FromInt64(v.Value)})
			}
		}
	}
	return out, nil
}

// Unspent sums each address's confirmed unspent outputs.
func (e *Esplora) Unspent(ctx context.Context, addresses []string) (money.Amount, error) {
	total := money.Zero
	for _, a := range addresses {
		b, err := e.get(ctx, "/address/"+a+"/utxo")
		if err != nil {
			return money.Zero, err
		}
		var utxos []struct {
			Value  int64 `json:"value"`
			Status struct {
				Confirmed bool `json:"confirmed"`
			} `json:"status"`
		}
		if err := json.Unmarshal(b, &utxos); err != nil {
			return money.Zero, err
		}
		for _, u := range utxos {
			if u.Status.Confirmed {
				total = total.Add(money.FromInt64(u.Value))
			}
		}
	}
	return total, nil
}
