package rpcpool_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/JoshBlazer/waybill/api/internal/rpcpool"
)

// fake is a scripted endpoint. err, when set, is returned by every call.
type fake struct {
	mu    sync.Mutex
	head  uint64
	err   error
	calls int
}

func (f *fake) answer() (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.head, f.err
}

func (f *fake) set(head uint64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head, f.err = head, err
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fake) BlockNumber(context.Context) (uint64, error) { return f.answer() }
func (f *fake) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	h, err := f.answer()
	return &types.Header{Number: new(big.Int).SetUint64(h)}, err
}
func (f *fake) FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
	_, err := f.answer()
	return nil, err
}
func (f *fake) CallContract(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error) {
	_, err := f.answer()
	return nil, err
}
func (f *fake) TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error) {
	_, err := f.answer()
	return nil, err
}
func (f *fake) SuggestGasTipCap(context.Context) (*big.Int, error) {
	_, err := f.answer()
	return big.NewInt(1), err
}
func (f *fake) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error)  { return f.answer() }
func (f *fake) PendingNonceAt(context.Context, common.Address) (uint64, error) { return f.answer() }
func (f *fake) SendTransaction(context.Context, *types.Transaction) error {
	_, err := f.answer()
	return err
}

// jsonRPCError is what go-ethereum returns for a JSON-RPC error object.
type jsonRPCError struct{ code int }

func (e jsonRPCError) Error() string  { return fmt.Sprintf("rpc error %d", e.code) }
func (e jsonRPCError) ErrorCode() int { return e.code }

var _ rpc.Error = jsonRPCError{}

func TestTransient(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"refused":         {fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		"timeout":         {context.DeadlineExceeded, true},
		"502":             {rpc.HTTPError{StatusCode: 502}, true},
		"429":             {rpc.HTTPError{StatusCode: 429}, true},
		"401":             {rpc.HTTPError{StatusCode: 401}, false},
		"revert":          {jsonRPCError{3}, false},
		"nonce too low":   {jsonRPCError{-32000}, false},
		"receipt pending": {ethereum.NotFound, false},
		"nil":             {nil, false},
	} {
		if got := rpcpool.Transient(tc.err); got != tc.want {
			t.Errorf("%s: Transient = %v, want %v", name, got, tc.want)
		}
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newPool(t *testing.T, c *clock, eps ...*fake) *rpcpool.Pool {
	t.Helper()
	var list []rpcpool.Endpoint
	for i, f := range eps {
		list = append(list, rpcpool.Endpoint{Name: fmt.Sprintf("rpc%d", i), Client: f})
	}
	p, err := rpcpool.New("evm:31337", list, rpcpool.Options{MaxLag: 5, RecoverAfter: time.Minute, Now: c.now, CallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func current(p *rpcpool.Pool) string {
	for _, s := range p.Status() {
		if s.Current {
			return s.Name
		}
	}
	return ""
}

// A transport failure on the primary is retried once on the next endpoint,
// in the same call, and later calls stay there.
func TestPool_FailsOverOnTransportError(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	a, b := &fake{head: 100}, &fake{head: 100}
	p := newPool(t, c, a, b)
	a.set(0, fmt.Errorf("dial: %w", syscall.ECONNREFUSED))

	n, err := p.BlockNumber(context.Background())
	if err != nil || n != 100 {
		t.Fatalf("BlockNumber = %d, %v; want 100 from the second endpoint", n, err)
	}
	if current(p) != "rpc1" || p.Status()[0].State != rpcpool.Down {
		t.Fatalf("status %+v", p.Status())
	}
	before := a.count()
	if _, err := p.BlockNumber(context.Background()); err != nil || a.count() != before {
		t.Fatalf("the next call went to the down primary (%v)", err)
	}
}

// A node's answer is never retried elsewhere: asking twice must not get a
// different answer.
func TestPool_AnswersAreNotRetried(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	a, b := &fake{head: 100}, &fake{head: 100}
	p := newPool(t, c, a, b)
	for _, answer := range []error{jsonRPCError{3}, ethereum.NotFound, rpc.HTTPError{StatusCode: 400}} {
		a.set(100, answer)
		if _, err := p.EstimateGas(context.Background(), ethereum.CallMsg{}); !errors.Is(err, answer) && err.Error() != answer.Error() {
			t.Fatalf("got %v, want the answer %v", err, answer)
		}
		if b.count() != 0 || current(p) != "rpc0" {
			t.Fatalf("an answer (%v) failed over", answer)
		}
	}
}

// With every endpoint down, the call fails after two attempts and the
// error is the transport error, never a made-up answer.
func TestPool_AllDownFails(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	down := fmt.Errorf("dial: %w", syscall.ECONNREFUSED)
	a, b, d := &fake{err: down}, &fake{err: down}, &fake{err: down}
	p := newPool(t, c, a, b, d)
	if _, err := p.BlockNumber(context.Background()); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("err = %v", err)
	}
	if a.count()+b.count()+d.count() != 2 {
		t.Fatalf("%d attempts; want the current endpoint and one retry", a.count()+b.count()+d.count())
	}
}

// A caller's own cancellation does not mark an endpoint down.
func TestPool_CallerCancelIsNotAnEndpointFailure(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	a, b := &fake{head: 1}, &fake{head: 1}
	p := newPool(t, c, a, b)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.set(1, context.Canceled)
	_, _ = p.BlockNumber(ctx)
	if p.Status()[0].State != rpcpool.Up || b.count() != 0 {
		t.Fatalf("status %+v", p.Status())
	}
}

// The health loop demotes an endpoint that falls behind, and the primary is
// used again only after it has been healthy for RecoverAfter.
func TestPool_LaggingPrimaryIsDemotedAndRecovers(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	a, b := &fake{head: 100}, &fake{head: 100}
	p := newPool(t, c, a, b)
	ctx := context.Background()

	a.set(90, nil) // 10 behind, MaxLag 5
	p.Check(ctx)
	if p.Status()[0].State != rpcpool.Lagging || current(p) != "rpc1" {
		t.Fatalf("after lagging: %+v", p.Status())
	}
	a.set(100, nil)
	p.Check(ctx)
	if p.Status()[0].State != rpcpool.Up || current(p) != "rpc1" {
		t.Fatalf("just caught up: %+v; want it up but not yet current", p.Status())
	}
	c.add(time.Minute)
	p.Check(ctx)
	if current(p) != "rpc0" {
		t.Fatalf("after a healthy minute: %+v; want the primary back", p.Status())
	}
}

// When the current endpoint fails, the retry goes to an endpoint that is
// up before one that is lagging.
func TestPool_RetryPrefersUpOverLagging(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	a, b, d := &fake{head: 100}, &fake{head: 80}, &fake{head: 100}
	p := newPool(t, c, a, b, d)
	p.Check(context.Background())
	if p.Status()[1].State != rpcpool.Lagging {
		t.Fatalf("status %+v", p.Status())
	}
	a.set(0, fmt.Errorf("dial: %w", syscall.ECONNREFUSED))
	if n, err := p.BlockNumber(context.Background()); err != nil || n != 100 || b.count() != 1 || current(p) != "rpc2" {
		t.Fatalf("got %d, %v; lagging endpoint called %d times (only by the health check); current %s", n, err, b.count(), current(p))
	}
}
