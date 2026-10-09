package safety

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/ethclient"
)

// EVMProber asks a real EVM JSON-RPC endpoint for eth_chainId.
type EVMProber struct{}

// ChainID dials rpcURL and returns the chain id it reports.
func (EVMProber) ChainID(ctx context.Context, rpcURL string) (*big.Int, error) {
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.ChainID(ctx)
}
