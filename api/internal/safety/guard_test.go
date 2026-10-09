package safety

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRPC serves eth_chainId with the given id. It speaks real JSON-RPC so
// the tests exercise EVMProber, not a stub of it.
func fakeRPC(t *testing.T, chainID int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Method != "eth_chainId" {
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, req.ID)
			return
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":"0x%x"}`, req.ID, chainID)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckConfig_AcceptsEveryAllowedNetwork(t *testing.T) {
	for _, n := range AllowedNetworks() {
		t.Run(string(n), func(t *testing.T) {
			cfg := Config{Chains: []Chain{{Network: n, RPCURL: "http://localhost:8545"}}}
			if err := CheckConfig(cfg); err != nil {
				t.Fatalf("CheckConfig(%s) = %v, want nil", n, err)
			}
		})
	}
}

func TestCheck_RejectsNonTestChains(t *testing.T) {
	cases := []Network{
		"evm:1",        // Ethereum mainnet
		"evm:8453",     // Base mainnet
		"evm:10",       // OP mainnet
		"evm:137",      // Polygon
		"evm:56",       // BNB Chain
		"evm:42161",    // Arbitrum One
		"evm:5",        // Goerli: retired, not on the allowlist
		"btc:main",     // Bitcoin mainnet
		"btc:mainnet",  // alternative spelling
		"btc:testnet3", // not on the allowlist
		"evm:031337",   // look-alike of Anvil
		"EVM:31337",    // case matters
		"evm:31337 ",   // trailing space
		"31337",        // no family
		"",             // empty
	}
	for _, n := range cases {
		t.Run(fmt.Sprintf("%q", n), func(t *testing.T) {
			cfg := Config{Chains: []Chain{{Network: n, RPCURL: "http://localhost:8545"}}}
			err := Check(context.Background(), cfg, EVMProber{})
			if err == nil {
				t.Fatalf("Check accepted %q, want rejection", n)
			}
			if !strings.Contains(err.Error(), "not an allowed test network") {
				t.Fatalf("error = %q, want allowlist rejection", err)
			}
		})
	}
}

func TestCheck_RejectsMixedConfigEvenWhenOneChainIsAllowed(t *testing.T) {
	cfg := Config{Chains: []Chain{
		{Network: EVMAnvil, RPCURL: "http://localhost:8545"},
		{Network: "evm:1", RPCURL: "http://localhost:8546"},
	}}
	if err := CheckConfig(cfg); err == nil {
		t.Fatal("CheckConfig accepted a config containing mainnet")
	}
}

func TestCheckConfig_RejectsDuplicateNetwork(t *testing.T) {
	cfg := Config{Chains: []Chain{
		{Network: EVMAnvil, RPCURL: "http://a:8545"},
		{Network: EVMAnvil, RPCURL: "http://b:8545"},
	}}
	if err := CheckConfig(cfg); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("CheckConfig = %v, want duplicate rejection", err)
	}
}

func TestCheckConfig_RejectsMalformedRPCURL(t *testing.T) {
	for _, raw := range []string{"", "localhost:8545", "ftp://host/x", "://nohost"} {
		t.Run(raw, func(t *testing.T) {
			cfg := Config{Chains: []Chain{{Network: EVMAnvil, RPCURL: raw}}}
			if err := CheckConfig(cfg); err == nil {
				t.Fatalf("CheckConfig accepted RPC URL %q", raw)
			}
		})
	}
}

func TestCheck_RejectsLivePaystackKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		ok   bool
	}{
		{"test key", "sk_test_abc123", true},
		{"unset", "", true},
		{"live key", "sk_live_abc123", false},
		{"public test key", "pk_test_abc123", false},
		{"uppercase prefix", "SK_TEST_abc123", false},
		{"prefix inside", "xsk_test_abc123", false},
		{"leading space", " sk_test_abc123", false},
		{"garbage", "hunter2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckConfig(Config{PaystackSecretKey: tc.key})
			if tc.ok && err != nil {
				t.Fatalf("CheckConfig(%q) = %v, want nil", tc.key, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("CheckConfig(%q) accepted a non-test key", tc.key)
				}
				if tc.key != "" && strings.Contains(err.Error(), tc.key) {
					t.Fatalf("error message leaks the key: %q", err)
				}
			}
		})
	}
}

func TestCheck_ReportsEveryProblem(t *testing.T) {
	cfg := Config{
		Chains:            []Chain{{Network: "evm:1", RPCURL: "http://x"}, {Network: "btc:main", RPCURL: "http://y"}},
		PaystackSecretKey: "sk_live_x",
	}
	err := CheckConfig(cfg)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{`"evm:1"`, `"btc:main"`, "PAYSTACK_SECRET_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestCheck_LiveChainIDMatches(t *testing.T) {
	srv := fakeRPC(t, 31337)
	cfg := Config{Chains: []Chain{{Network: EVMAnvil, RPCURL: srv.URL}}}
	if err := Check(context.Background(), cfg, EVMProber{}); err != nil {
		t.Fatalf("Check = %v, want nil", err)
	}
}

func TestCheck_LiveChainIDMismatch(t *testing.T) {
	// Config claims Base Sepolia, but the RPC is Ethereum mainnet.
	srv := fakeRPC(t, 1)
	cfg := Config{Chains: []Chain{{Network: EVMBaseSepolia, RPCURL: srv.URL}}}
	err := Check(context.Background(), cfg, EVMProber{})
	if err == nil {
		t.Fatal("Check accepted an RPC that serves mainnet")
	}
	if !strings.Contains(err.Error(), "reports chain id 1, want 84532") {
		t.Fatalf("error = %q", err)
	}
}

func TestCheck_RPCUnreachableFailsClosed(t *testing.T) {
	srv := fakeRPC(t, 31337)
	url := srv.URL
	srv.Close() // nothing listens any more
	cfg := Config{Chains: []Chain{{Network: EVMAnvil, RPCURL: url}}}
	if err := Check(context.Background(), cfg, EVMProber{}); err == nil {
		t.Fatal("Check passed with an unreachable RPC; must fail closed")
	}
}

func TestCheck_RPCErrorFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	cfg := Config{Chains: []Chain{{Network: EVMAnvil, RPCURL: srv.URL}}}
	if err := Check(context.Background(), cfg, EVMProber{}); err == nil {
		t.Fatal("Check passed with an erroring RPC; must fail closed")
	}
}

func TestCheck_BitcoinFailsClosedUntilLiveCheckExists(t *testing.T) {
	cfg := Config{Chains: []Chain{{Network: BTCRegtest, RPCURL: "http://localhost:18443"}}}
	if err := CheckConfig(cfg); err != nil {
		t.Fatalf("CheckConfig = %v; regtest is on the allowlist", err)
	}
	if err := Check(context.Background(), cfg, EVMProber{}); err == nil {
		t.Fatal("Check passed a Bitcoin network with no live check; must fail closed")
	}
}

func TestCheck_ErrorsNeverContainFullRPCURL(t *testing.T) {
	const secretPath = "/v2/SECRET-API-KEY"
	srv := fakeRPC(t, 1)
	rpcURL := srv.URL + secretPath
	cfg := Config{Chains: []Chain{{Network: EVMAnvil, RPCURL: rpcURL}}}
	err := Check(context.Background(), cfg, EVMProber{})
	if err == nil {
		t.Fatal("want mismatch error")
	}
	if strings.Contains(err.Error(), "SECRET-API-KEY") {
		t.Fatalf("error leaks RPC credentials: %q", err)
	}

	srv.Close()
	err = Check(context.Background(), cfg, EVMProber{})
	if err == nil {
		t.Fatal("want unreachable error")
	}
	if strings.Contains(err.Error(), "SECRET-API-KEY") {
		t.Fatalf("error leaks RPC credentials: %q", err)
	}
}

func TestCheck_EmptyConfigIsSafe(t *testing.T) {
	if err := Check(context.Background(), Config{}, EVMProber{}); err != nil {
		t.Fatalf("Check(empty) = %v; a service with no chains touches no funds", err)
	}
}

// stubProber lets a test assert the prober is never reached.
type stubProber struct{ called bool }

func (s *stubProber) ChainID(context.Context, string) (*big.Int, error) {
	s.called = true
	return big.NewInt(1), nil
}

func TestCheck_StaticFailureSkipsNetwork(t *testing.T) {
	p := &stubProber{}
	cfg := Config{Chains: []Chain{{Network: "evm:1", RPCURL: "http://localhost:8545"}}}
	if err := Check(context.Background(), cfg, p); err == nil {
		t.Fatal("want error")
	}
	if p.called {
		t.Fatal("prober was called for a chain that failed the static check")
	}
}
