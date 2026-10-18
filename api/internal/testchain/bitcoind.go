package testchain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// BitcoindImage is Bitcoin Core 28.1, pinned by digest (ADR-045).
const BitcoindImage = "bitcoin/bitcoin:28.1@sha256:f332e1117453886b704e8f985dc21594917e79422860effe7842f2aa45a3ac6e"

// Bitcoind is a running regtest node with two descriptor wallets:
// "custody" stands in for the offline key holder (Waybill reads only its
// xpub), and "payer" holds mined coins to pay with.
type Bitcoind struct {
	URL string // http://user:pass@host:port, for btc.Dial
	t   *testing.T
}

// StartBitcoind runs a fresh regtest node for the test. It skips under
// -short.
func StartBitcoind(t *testing.T) *Bitcoind {
	t.Helper()
	if testing.Short() {
		t.Skip("starts bitcoind in Docker; skipped with -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: BitcoindImage,
			Cmd: []string{"bitcoind", "-regtest", "-server", "-rpcuser=waybill", "-rpcpassword=regtest-only",
				"-rpcbind=0.0.0.0", "-rpcallowip=0.0.0.0/0", "-fallbackfee=0.0002", "-txindex=0", "-printtoconsole"},
			ExposedPorts: []string{"18443/tcp"},
			WaitingFor:   wait.ForLog("init message: Done loading").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start bitcoind: %v", err)
	}
	ep, err := ctr.PortEndpoint(ctx, "18443/tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	b := &Bitcoind{URL: "http://waybill:regtest-only@" + ep, t: t}
	b.Call(nil, "", "createwallet", "custody")
	b.Call(nil, "", "createwallet", "payer")
	var addr string
	b.Call(&addr, "payer", "getnewaddress")
	b.Call(nil, "", "generatetoaddress", 101, addr) // coinbase matures after 100
	return b
}

// Call makes a JSON-RPC call (on a wallet when wallet is set) and fails the
// test on error.
func (b *Bitcoind) Call(out any, wallet, method string, params ...any) {
	b.t.Helper()
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": 1, "method": method, "params": params})
	u := strings.Replace(b.URL, "waybill:regtest-only@", "", 1)
	if wallet != "" {
		u += "/wallet/" + wallet
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, u, bytes.NewReader(body))
	req.SetBasicAuth("waybill", "regtest-only")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatalf("bitcoind %s: %v", method, err)
	}
	defer func() { _ = res.Body.Close() }()
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		b.t.Fatalf("bitcoind %s: HTTP %d: %v", method, res.StatusCode, err)
	}
	if env.Error != nil {
		b.t.Fatalf("bitcoind %s: %s", method, env.Error.Message)
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			b.t.Fatalf("bitcoind %s: %v", method, err)
		}
	}
}

// CustodyXPub returns the custody wallet's BIP84 account tpub: the key in
// its wpkh receive descriptor.
func (b *Bitcoind) CustodyXPub() (xpub, descriptor string) {
	b.t.Helper()
	var res struct {
		Descriptors []struct {
			Desc     string `json:"desc"`
			Internal bool   `json:"internal"`
		} `json:"descriptors"`
	}
	b.Call(&res, "custody", "listdescriptors")
	for _, d := range res.Descriptors {
		if strings.HasPrefix(d.Desc, "wpkh(") && !d.Internal {
			// wpkh([fingerprint/84h/1h/0h]tpub…/0/*)#checksum
			i, j := strings.Index(d.Desc, "]"), strings.Index(d.Desc, "/0/*")
			return d.Desc[i+1 : j], d.Desc
		}
	}
	b.t.Fatal("custody wallet has no wpkh receive descriptor")
	return "", ""
}

// Pay sends amount (in BTC, as a decimal string) to addr from the payer
// wallet and mines one block, returning the txid.
func (b *Bitcoind) Pay(addr, amount string) string {
	b.t.Helper()
	var txid string
	b.Call(&txid, "payer", "sendtoaddress", addr, json.Number(amount))
	b.Mine(1)
	return txid
}

// Mine mines n blocks to the payer.
func (b *Bitcoind) Mine(n int) {
	b.t.Helper()
	var addr string
	b.Call(&addr, "payer", "getnewaddress")
	b.Call(nil, "", "generatetoaddress", n, addr)
}

func (b *Bitcoind) String() string {
	return fmt.Sprintf("bitcoind(%s)", strings.SplitN(b.URL, "@", 2)[1])
}
