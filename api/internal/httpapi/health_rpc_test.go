package httpapi

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/JoshBlazer/waybill/api/internal/rpcpool"
)

// headOnly answers BlockNumber with a fixed head or error; nothing else is
// called by the health check.
type headOnly struct {
	head uint64
	err  error
}

func (h headOnly) BlockNumber(context.Context) (uint64, error) { return h.head, h.err }
func (headOnly) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return nil, errors.New("unused")
}
func (headOnly) FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
	return nil, errors.New("unused")
}
func (headOnly) CallContract(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error) {
	return nil, errors.New("unused")
}
func (headOnly) TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error) {
	return nil, errors.New("unused")
}
func (headOnly) SuggestGasTipCap(context.Context) (*big.Int, error) { return nil, errors.New("unused") }
func (headOnly) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return 0, errors.New("unused")
}
func (headOnly) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	return 0, errors.New("unused")
}
func (headOnly) SendTransaction(context.Context, *types.Transaction) error {
	return errors.New("unused")
}

func pool(t *testing.T, eps ...rpcpool.Endpoint) *rpcpool.Pool {
	t.Helper()
	p, err := rpcpool.New("evm:11155111", eps, rpcpool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p.Check(context.Background())
	return p
}

func TestGetHealth_ReportsRPCEndpointsByHostOnly(t *testing.T) {
	p := pool(t,
		rpcpool.Endpoint{Name: "a.example", Client: headOnly{err: errors.New("connection refused")}},
		rpcpool.Endpoint{Name: "b.example", Client: headOnly{head: 7_000_000}},
	)
	code, h := getHealth(t, &Server{DB: fakeDB{}, Version: "test", RPC: []*rpcpool.Pool{p}})
	if code != http.StatusOK || h.Status != HealthStatusOk || h.Rpc == nil || len(*h.Rpc) != 1 {
		t.Fatalf("%d %+v", code, h)
	}
	eps := (*h.Rpc)[0].Endpoints
	if (*h.Rpc)[0].Network != "evm:11155111" || len(eps) != 2 ||
		eps[0].Name != "a.example" || eps[0].State != Down || eps[0].Current ||
		eps[1].State != Up || !eps[1].Current || eps[1].Head != 7_000_000 {
		t.Fatalf("endpoints %+v", eps)
	}
}

func TestGetHealth_AllEndpointsDownIsDegraded(t *testing.T) {
	p := pool(t, rpcpool.Endpoint{Name: "a.example", Client: headOnly{err: errors.New("connection refused")}})
	code, h := getHealth(t, &Server{DB: fakeDB{}, Version: "test", RPC: []*rpcpool.Pool{p}})
	if code != http.StatusOK || h.Status != HealthStatusDegraded {
		t.Fatalf("%d %s; want 200 degraded: the API still serves, the network does not", code, h.Status)
	}
}
