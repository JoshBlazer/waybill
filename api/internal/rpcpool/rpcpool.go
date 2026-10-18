// Package rpcpool spreads one EVM network over several RPC endpoints and
// fails over between them (docs/design/stage5.md §3, ADR-045).
//
//   - Every call goes to the current endpoint. A transport failure (no
//     connection, a timeout, HTTP 5xx or 429) marks it down, and the call is
//     retried once on the next healthy endpoint.
//   - A JSON-RPC error (a revert, "nonce too low", not found) is the node's
//     answer, not a failure: it is returned at once and never retried, so a
//     caller never gets a different answer by asking twice.
//   - A health loop reads every endpoint's head. An endpoint more than
//     MaxLag blocks behind the best is lagging and is used only when nothing
//     better is up. The primary (the first endpoint) is used again once it
//     has been healthy for RecoverAfter.
//
// Every endpoint must pass the startup chain-ID check on its own (the
// test-money guard checks each URL), so failing over can never move to
// another chain.
package rpcpool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/JoshBlazer/waybill/api/internal/telemetry"
)

// Backend is one endpoint's client. *ethclient.Client satisfies it.
type Backend interface {
	BlockNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	CallContract(ctx context.Context, msg ethereum.CallMsg, block *big.Int) ([]byte, error)
	TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error)
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
}

// Endpoint is one RPC endpoint. Name is the URL's host only: a full URL can
// carry an API key.
type Endpoint struct {
	Name   string
	Client Backend
}

// State of an endpoint.
type State string

// The states an endpoint can be in.
const (
	Up      State = "up"
	Lagging State = "lagging"
	Down    State = "down"
)

type endpoint struct {
	Endpoint
	state        State
	healthySince time.Time
	head         uint64
	latency      time.Duration
	lastErr      string
	failures     uint64
}

// Options tune a pool. Zero values take the defaults.
type Options struct {
	CallTimeout  time.Duration // per attempt; default 10 s
	MaxLag       uint64        // blocks behind the best head before lagging; default 10
	RecoverAfter time.Duration // healthy time before the primary is used again; default 1 min
	Log          *slog.Logger
	Now          func() time.Time
	// OnSwitch, if set, is called when the current endpoint changes.
	OnSwitch func(from, to string)
}

// Pool is a failover client for one network.
type Pool struct {
	network string
	opts    Options
	mu      sync.Mutex
	eps     []*endpoint
	current int
}

// New returns a pool over eps; the first is the primary.
func New(network string, eps []Endpoint, opts Options) (*Pool, error) {
	if len(eps) == 0 {
		return nil, fmt.Errorf("rpcpool %s: no endpoints", network)
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = 10 * time.Second
	}
	if opts.MaxLag == 0 {
		opts.MaxLag = 10
	}
	if opts.RecoverAfter <= 0 {
		opts.RecoverAfter = time.Minute
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	p := &Pool{network: network, opts: opts}
	// Created here, not at package init: an observable must come from the
	// same meter as its callback, and the real meter provider is installed
	// only after start-up.
	m := telemetry.Meter()
	endpointState, err := m.Int64ObservableGauge("waybill.rpc.endpoint.state",
		metric.WithDescription("Endpoint health: 2 up, 1 lagging, 0 down."))
	if err != nil {
		return nil, err
	}
	if _, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, e := range p.Status() {
			v := map[State]int64{Up: 2, Lagging: 1, Down: 0}[e.State]
			o.ObserveInt64(endpointState, v, metric.WithAttributes(attribute.String("network", network),
				attribute.String("endpoint", e.Name), attribute.Bool("current", e.Current)))
		}
		return nil
	}, endpointState); err != nil {
		return nil, err
	}
	now := opts.Now()
	for _, e := range eps {
		p.eps = append(p.eps, &endpoint{Endpoint: e, state: Up, healthySince: now.Add(-opts.RecoverAfter)})
	}
	return p, nil
}

// Transient reports whether err means the endpoint failed, rather than the
// node answering. Only transient errors fail over.
func Transient(err error) bool {
	if err == nil || errors.Is(err, ethereum.NotFound) {
		return false
	}
	var answer rpc.Error
	if errors.As(err, &answer) {
		return false // a JSON-RPC error object: the node answered
	}
	var h rpc.HTTPError
	if errors.As(err, &h) {
		return h.StatusCode >= 500 || h.StatusCode == 429
	}
	return true // connection refused, reset, timeout, malformed response
}

// order is the current endpoint, then the others in preference order.
func (p *Pool) order() []*endpoint {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []*endpoint{p.eps[p.current]}
	for _, want := range []State{Up, Lagging, Down} {
		for i, e := range p.eps {
			if i != p.current && e.state == want {
				out = append(out, e)
			}
		}
	}
	return out
}

func (p *Pool) markDown(e *endpoint, err error) {
	p.mu.Lock()
	e.state, e.lastErr = Down, err.Error()
	e.failures++
	p.mu.Unlock()
	p.opts.Log.Warn("rpc endpoint down", "network", p.network, "endpoint", e.Name, "err", err)
	p.pick()
}

// do runs f on the current endpoint and, after a transport failure, once
// more on the next one.
func (p *Pool) do(ctx context.Context, method string, f func(context.Context, Backend) error) (err error) {
	ctx, span := telemetry.Tracer().Start(ctx, "rpc "+method, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("rpc.method", method), attribute.String("network", p.network)))
	defer func() {
		if err != nil && Transient(err) {
			span.RecordError(err)
			span.SetStatus(codes.Error, "endpoint failed")
		}
		span.End()
	}()
	var last error
	for i, e := range p.order() {
		if i == 2 {
			break
		}
		span.SetAttributes(attribute.String("rpc.endpoint", e.Name))
		cctx, cancel := context.WithTimeout(ctx, p.opts.CallTimeout)
		err := f(cctx, e.Client)
		cancel()
		if err == nil || !Transient(err) {
			return err
		}
		if ctx.Err() != nil {
			return err // the caller gave up; not the endpoint's fault
		}
		p.markDown(e, err)
		last = err
	}
	return last
}

// pick chooses the current endpoint: the primary when it has been healthy
// long enough, else the first up endpoint, else the first lagging one,
// else stay.
func (p *Pool) pick() {
	p.mu.Lock()
	now := p.opts.Now()
	choice := -1
	if pr := p.eps[0]; pr.state == Up && now.Sub(pr.healthySince) >= p.opts.RecoverAfter {
		choice = 0
	}
	for _, want := range []State{Up, Lagging} {
		if choice >= 0 {
			break
		}
		if p.eps[p.current].state == want {
			choice = p.current // keep a working endpoint rather than flap
			break
		}
		for i, e := range p.eps {
			if e.state == want {
				choice = i
				break
			}
		}
	}
	from := p.eps[p.current].Name
	changed := choice >= 0 && choice != p.current
	if changed {
		p.current = choice
	}
	to := p.eps[p.current].Name
	p.mu.Unlock()
	if changed {
		failovers.Add(context.Background(), 1, metric.WithAttributes(attribute.String("network", p.network),
			attribute.String("from", from), attribute.String("to", to)))
		p.opts.Log.Warn("rpc failover", "network", p.network, "from", from, "to", to)
		if p.opts.OnSwitch != nil {
			p.opts.OnSwitch(from, to)
		}
	}
}

// Check reads every endpoint's head once and updates its state.
func (p *Pool) Check(ctx context.Context) {
	type result struct {
		head    uint64
		latency time.Duration
		err     error
	}
	results := make([]result, len(p.eps))
	var wg sync.WaitGroup
	for i, e := range p.eps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, p.opts.CallTimeout)
			defer cancel()
			start := time.Now()
			h, err := e.Client.BlockNumber(cctx)
			results[i] = result{h, time.Since(start), err}
		}()
	}
	wg.Wait()
	var best uint64
	for _, r := range results {
		if r.err == nil && r.head > best {
			best = r.head
		}
	}
	now := p.opts.Now()
	p.mu.Lock()
	for i, e := range p.eps {
		r := results[i]
		was := e.state
		switch {
		case r.err != nil:
			e.state, e.lastErr = Down, r.err.Error()
			e.failures++
		case best-r.head > p.opts.MaxLag:
			e.state, e.head, e.latency = Lagging, r.head, r.latency
		default:
			e.state, e.head, e.latency, e.lastErr = Up, r.head, r.latency, ""
		}
		if e.state == Up && was != Up {
			e.healthySince = now
		}
	}
	p.mu.Unlock()
	p.pick()
}

// Run checks health every interval until ctx is done.
func (p *Pool) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		p.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// EndpointStatus is one endpoint's health, for /v1/health and metrics.
type EndpointStatus struct {
	Name     string
	State    State
	Current  bool
	Head     uint64
	Latency  time.Duration
	Failures uint64
	LastErr  string
}

// Status reports every endpoint, in configured order.
func (p *Pool) Status() []EndpointStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]EndpointStatus, len(p.eps))
	for i, e := range p.eps {
		out[i] = EndpointStatus{Name: e.Name, State: e.state, Current: i == p.current, Head: e.head,
			Latency: e.latency, Failures: e.failures, LastErr: e.lastErr}
	}
	return out
}

// Network is the network this pool serves.
func (p *Pool) Network() string { return p.network }

// BlockNumber returns the head height, failing over as described above.
func (p *Pool) BlockNumber(ctx context.Context) (n uint64, err error) {
	err = p.do(ctx, "BlockNumber", func(c context.Context, b Backend) (e error) { n, e = b.BlockNumber(c); return })
	return
}

// HeaderByNumber returns a block header, failing over as described above.
func (p *Pool) HeaderByNumber(ctx context.Context, number *big.Int) (h *types.Header, err error) {
	err = p.do(ctx, "HeaderByNumber", func(c context.Context, b Backend) (e error) { h, e = b.HeaderByNumber(c, number); return })
	return
}

// FilterLogs returns matching logs, failing over as described above.
func (p *Pool) FilterLogs(ctx context.Context, q ethereum.FilterQuery) (logs []types.Log, err error) {
	err = p.do(ctx, "FilterLogs", func(c context.Context, b Backend) (e error) { logs, e = b.FilterLogs(c, q); return })
	return
}

// CallContract runs a read-only call, failing over as described above.
func (p *Pool) CallContract(ctx context.Context, msg ethereum.CallMsg, block *big.Int) (out []byte, err error) {
	err = p.do(ctx, "CallContract", func(c context.Context, b Backend) (e error) { out, e = b.CallContract(c, msg, block); return })
	return
}

// TransactionReceipt returns a receipt, or ethereum.NotFound (an answer, not a failure).
func (p *Pool) TransactionReceipt(ctx context.Context, hash common.Hash) (r *types.Receipt, err error) {
	err = p.do(ctx, "TransactionReceipt", func(c context.Context, b Backend) (e error) { r, e = b.TransactionReceipt(c, hash); return })
	return
}

// SuggestGasTipCap returns the suggested priority fee, failing over as described above.
func (p *Pool) SuggestGasTipCap(ctx context.Context) (v *big.Int, err error) {
	err = p.do(ctx, "SuggestGasTipCap", func(c context.Context, b Backend) (e error) { v, e = b.SuggestGasTipCap(c); return })
	return
}

// EstimateGas estimates gas; a revert is an answer and is never retried elsewhere.
func (p *Pool) EstimateGas(ctx context.Context, msg ethereum.CallMsg) (g uint64, err error) {
	err = p.do(ctx, "EstimateGas", func(c context.Context, b Backend) (e error) { g, e = b.EstimateGas(c, msg); return })
	return
}

// PendingNonceAt returns the pending nonce, failing over as described above.
func (p *Pool) PendingNonceAt(ctx context.Context, account common.Address) (n uint64, err error) {
	err = p.do(ctx, "PendingNonceAt", func(c context.Context, b Backend) (e error) { n, e = b.PendingNonceAt(c, account); return })
	return
}

// SendTransaction may reach the second endpoint after the first failed
// mid-request. That is safe: the same signed transaction has the same hash,
// and the transaction manager has recorded it before sending (I12), so a
// second node answering "already known" is handled as before.
func (p *Pool) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	return p.do(ctx, "SendTransaction", func(c context.Context, b Backend) error { return b.SendTransaction(c, tx) })
}

var _ Backend = (*Pool)(nil)

// Pool metrics (ADR-045). Endpoints are named by host only.
var (
	failovers, _ = telemetry.Meter().Int64Counter("waybill.rpc.failovers",
		metric.WithDescription("Times a network's current RPC endpoint changed."))
)
