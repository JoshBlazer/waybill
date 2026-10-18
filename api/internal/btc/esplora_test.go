package btc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/JoshBlazer/waybill/api/internal/btc"
	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// The Esplora client against a scripted server: tip height, hashes, the
// outputs of a block across pages, and confirmed unspent value only.
func TestEsplora_ReadsBlocksAndUnspent(t *testing.T) {
	const hash = "00000000000000000000000000000000000000000000000000000000000000aa"
	routes := map[string]string{
		"/api/blocks/tip/height":         "205\n",
		"/api/block-height/205":          hash,
		"/api/block/" + hash:             `{"tx_count": 26}`,
		"/api/block/" + hash + "/txs/0":  `[{"txid":"t1","vout":[{"scriptpubkey_address":"tb1qours","value":1500},{"value":0}]}]`,
		"/api/block/" + hash + "/txs/25": `[{"txid":"t2","vout":[{"scriptpubkey_address":"tb1qother","value":7}]}]`,
		"/api/address/tb1qours/utxo":     `[{"value":1500,"status":{"confirmed":true}},{"value":99,"status":{"confirmed":false}}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c, err := btc.Dial("esplora+" + srv.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if h, err := c.Height(ctx); err != nil || h != 205 {
		t.Fatalf("height %d, %v", h, err)
	}
	got, err := c.BlockHash(ctx, 205)
	if err != nil || got != hash {
		t.Fatalf("hash %q, %v", got, err)
	}
	outs, err := c.BlockOutputs(ctx, hash)
	if err != nil || len(outs) != 2 || outs[0].Address != "tb1qours" || outs[0].Amount.String() != "1500" || outs[1].TxID != "t2" {
		t.Fatalf("outputs %+v, %v; want both pages, no address-less output", outs, err)
	}
	if u, err := c.Unspent(ctx, []string{"tb1qours"}); err != nil || u.String() != "1500" {
		t.Fatalf("unspent %s, %v; want confirmed outputs only", u, err)
	}
	if _, err := c.BlockHash(ctx, 999); err == nil {
		t.Fatal("a missing block was not an error")
	}
}

// TestLive_EsploraSignet reads a public signet Esplora API (default
// mempool.space; set WAYBILL_LIVE_ESPLORA_URL for another): the tip, and
// block 0, which must be the signet genesis. It needs the network, so it
// runs only with WAYBILL_LIVE_ESPLORA=1.
func TestLive_EsploraSignet(t *testing.T) {
	if os.Getenv("WAYBILL_LIVE_ESPLORA") != "1" {
		t.Skip("set WAYBILL_LIVE_ESPLORA=1 to read the public signet API")
	}
	u := os.Getenv("WAYBILL_LIVE_ESPLORA_URL")
	if u == "" {
		u = "https://mempool.space/signet/api"
	}
	c, err := btc.Dial("esplora+" + u)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h, err := c.Height(ctx)
	if err != nil || h < 200_000 {
		t.Fatalf("signet tip %d, %v", h, err)
	}
	g, err := c.BlockHash(ctx, 0)
	if err != nil || g != safety.BTCGenesis[safety.BTCSignet] {
		t.Fatalf("block 0 = %s, %v; want signet genesis", g, err)
	}
	tip, err := c.BlockHash(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.BlockOutputs(ctx, tip); err != nil {
		t.Fatalf("outputs of the tip: %v", err)
	}
	t.Logf("signet tip %d (%s)", h, tip)
}
