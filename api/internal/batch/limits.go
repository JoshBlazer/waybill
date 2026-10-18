package batch

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/JoshBlazer/waybill/api/internal/money"
)

// VaultLimits reads the vault's per-payment limit and what it may still pay
// out today. The chain enforces both; reading them first gives a plain
// reason instead of a reverted transaction.
type VaultLimits interface {
	Limits(ctx context.Context) (perTx, roomToday money.Amount, err error)
}

// ChainVault reads Vault.limits(token) and Vault.paidOnDay(token, day).
type ChainVault struct {
	Client ContractCaller
	Vault  common.Address
	Token  common.Address
	Now    func() time.Time
}

// ContractCaller calls a contract read-only.
type ContractCaller interface {
	CallContract(ctx context.Context, msg ethereum.CallMsg, block *big.Int) ([]byte, error)
}

var (
	limitsSel    = crypto.Keccak256([]byte("limits(address)"))[:4]
	paidOnDaySel = crypto.Keccak256([]byte("paidOnDay(address,uint256)"))[:4]
)

// Limits implements VaultLimits. The day is the UTC day, as the vault's
// block.timestamp / 1 days.
func (v ChainVault) Limits(ctx context.Context) (money.Amount, money.Amount, error) {
	call := func(data []byte) ([]byte, error) {
		to := v.Vault
		return v.Client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	}
	out, err := call(append(append([]byte{}, limitsSel...), common.LeftPadBytes(v.Token.Bytes(), 32)...))
	if err != nil || len(out) != 64 {
		return money.Amount{}, money.Amount{}, fmt.Errorf("vault limits: %d bytes, %w", len(out), err)
	}
	perTx, perDay := new(big.Int).SetBytes(out[:32]), new(big.Int).SetBytes(out[32:])
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	day := big.NewInt(now().Unix() / 86400)
	data := append(append(append([]byte{}, paidOnDaySel...), common.LeftPadBytes(v.Token.Bytes(), 32)...), common.LeftPadBytes(day.Bytes(), 32)...)
	out, err = call(data)
	if err != nil || len(out) != 32 {
		return money.Amount{}, money.Amount{}, fmt.Errorf("vault paid today: %w", err)
	}
	room := new(big.Int).Sub(perDay, new(big.Int).SetBytes(out))
	if room.Sign() < 0 {
		room.SetInt64(0)
	}
	a, err := money.FromBig(perTx)
	if err != nil {
		return money.Amount{}, money.Amount{}, err
	}
	b, err := money.FromBig(room)
	return a, b, err
}
