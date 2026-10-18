package watcher_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/JoshBlazer/waybill/api/internal/rpcpool"
	"github.com/JoshBlazer/waybill/api/internal/testchain"
)

// proxy forwards TCP to a target until killed, like an RPC provider that
// goes away: the listener stops and every open connection is cut.
type proxy struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
	used  atomic.Int64
}

func startProxy(t *testing.T, target string) *proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln}
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				_ = in.Close()
				continue
			}
			p.used.Add(1)
			p.mu.Lock()
			p.conns = append(p.conns, in, out)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(out, in); _ = out.Close() }()
			go func() { _, _ = io.Copy(in, out); _ = in.Close() }()
		}
	}()
	t.Cleanup(p.kill)
	return p
}

func (p *proxy) url() string { return "http://" + p.ln.Addr().String() }

func (p *proxy) kill() {
	_ = p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// TestWatcher_SurvivesPrimaryRPCDying is the stage 5 failover exit check
// (ROADMAP): a payment is detected through the primary RPC, the primary is
// killed, and the watcher carries on through the fallback until the
// payment is final, counted exactly once.
func TestWatcher_SurvivesPrimaryRPCDying(t *testing.T) {
	r := setup(t)
	a := testchain.Start(t)
	u, err := url.Parse(a.URL)
	if err != nil {
		t.Fatal(err)
	}
	px := startProxy(t, u.Host)
	primary, err := ethclient.Dial(px.url())
	if err != nil {
		t.Fatal(err)
	}
	pool, err := rpcpool.New("evm:31337", []rpcpool.Endpoint{
		{Name: "primary", Client: primary},
		{Name: "fallback", Client: a.Client},
	}, rpcpool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r.w.Chain = pool
	r.w.Cfg.Confirmations, r.w.Cfg.Finality, r.w.Cfg.BatchBlocks = 2, 5, 500
	ctx := context.Background()

	a.Call(nil, "anvil_setCode", token, emitterCode)
	a.Mine(3)
	r.tick(t)
	data := append(common.LeftPadBytes(r.dep.Bytes(), 32), common.LeftPadBytes(big100USDC(), 32)...)
	a.Send(testchain.Dev0, token, data)
	a.Mine(1)
	r.tick(t)
	// Anvil mines the payment in its own block, so it may already be
	// confirmed; it must not be final yet.
	if st, ps := r.state(t); st == "settled" || ps["detected"]+ps["confirmed"] != 1 {
		t.Fatalf("before the kill: invoice %s, payments %v; want one payment seen, not final", st, ps)
	}
	if px.used.Load() == 0 {
		t.Fatal("the primary was never used")
	}

	px.kill()
	for range 8 {
		a.Mine(1)
		if err := r.w.Tick(ctx); err != nil {
			t.Fatalf("tick after the primary died: %v", err)
		}
	}
	st, ps := r.state(t)
	if st != "settled" || ps["final"] != 1 || len(ps) != 1 {
		t.Fatalf("after failover: invoice %s, payments %v; want settled with one final payment", st, ps)
	}
	if p := r.balance(t, "liability:contractor:%:USDC:available"); p != "-100000000" {
		t.Fatalf("available %s; the payment must count exactly once", p)
	}
	r.assertTrialBalanceZero(t)
	s := pool.Status()
	if s[0].State != rpcpool.Down || s[0].Current || !s[1].Current {
		t.Fatalf("pool status %+v; want the primary down and the fallback current", s)
	}
	// The primary stays down to calls made directly, so nothing reached it
	// after the kill by accident.
	if _, err := primary.BlockNumber(ctx); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("the killed primary still answers: %v", err)
	}
}
