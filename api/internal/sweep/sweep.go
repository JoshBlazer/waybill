// Package sweep moves final deposits from their per-invoice forwarder
// addresses into the vault (docs/ARCHITECTURE.md §3, ForwarderFactory).
//
// A final payment gets one sweep job (EnqueueDue). The job sends
// ForwarderFactory.deployAndSweep(salt, token) through the transaction
// manager, once no payment to the invoice is still unconfirmed. When the
// sweep is mined, Hook records what moved, read from the factory's Swept
// event, as one ledger entry: deposit custody down, vault custody up.
package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/outbox"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/txmgr"
)

// Topic is the outbox topic for one sweep; Purpose is the transaction
// manager purpose its transaction carries.
const (
	Topic   = "sweep.deposit"
	Purpose = "sweep"
)

// Job is the outbox payload for Topic.
type Job struct {
	Network   safety.Network `json:"network"`
	InvoiceID uuid.UUID      `json:"invoiceId"`
	PaymentID uuid.UUID      `json:"paymentId"`
}

var (
	deployAndSweep = crypto.Keccak256([]byte("deployAndSweep(bytes32,address)"))[:4]
	// sweptTopic is ForwarderFactory's Swept(bytes32 salt, address forwarder, address token, uint256 amount).
	sweptTopic = crypto.Keccak256Hash([]byte("Swept(bytes32,address,address,uint256)"))
)

// EnqueueDue adds one sweep job for each final payment on network that has
// none yet, and returns how many it added. Safe to run often and from
// several workers: the job's dedupe key is the payment.
func EnqueueDue(ctx context.Context, pool *pgxpool.Pool, network safety.Network) (int, error) {
	rows, err := store.New(pool).ListUnsweptFinalPayments(ctx, string(network))
	if err != nil {
		return 0, fmt.Errorf("sweep: list: %w", err)
	}
	n := 0
	for _, r := range rows {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			added, err := outbox.Enqueue(ctx, tx, Topic, Job{Network: network, InvoiceID: r.InvoiceID, PaymentID: r.ID},
				outbox.Options{DedupeKey: "sweep:" + r.ID.String()})
			if added {
				n++
			}
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// Sweeper handles sweep jobs and records mined sweeps.
type Sweeper struct {
	Pool        *pgxpool.Pool
	Managers    map[safety.Network]*txmgr.Manager
	Deployments map[safety.Network]deployments.Deployment
	Asset       money.Asset
	Log         *slog.Logger
}

// errOpenPayments: sweeping now could move money whose payment is not
// confirmed yet. The job retries with backoff.
var errOpenPayments = errors.New("sweep: invoice has unconfirmed payments; sweeping later")

// Handle submits the sweep transaction. It returns once the transaction is
// recorded and sent; Hook records the outcome when it is mined.
func (s *Sweeper) Handle(ctx context.Context, j outbox.Job) error {
	var job Job
	if err := json.Unmarshal(j.Payload, &job); err != nil {
		return fmt.Errorf("%w: payload: %w", outbox.ErrPermanent, err)
	}
	m, ok := s.Managers[job.Network]
	d, okd := s.Deployments[job.Network]
	if !ok || !okd {
		return fmt.Errorf("%w: no signer or deployment for %s", outbox.ErrPermanent, job.Network)
	}
	open, err := store.New(s.Pool).SumPaymentsByState(ctx, store.SumPaymentsByStateParams{
		InvoiceID: job.InvoiceID, States: []string{"detected", "confirmed"},
	})
	if err != nil {
		return err
	}
	if !open.IsZero() {
		return errOpenPayments
	}
	salt := evm.SaltForInvoice(job.InvoiceID)
	data := make([]byte, 0, 68)
	data = append(data, deployAndSweep...)
	data = append(data, salt[:]...)
	data = append(data, common.LeftPadBytes(d.Token.Bytes(), 32)...)
	_, err = m.Submit(ctx, txmgr.Request{Purpose: Purpose, RefID: "sweep:" + job.PaymentID.String(), To: d.Factory, Data: data})
	if errors.Is(err, txmgr.ErrEstimateFailed) {
		return fmt.Errorf("%w: %w", outbox.ErrPermanent, err)
	}
	return err
}

// Hook returns the transaction-manager hook for sweeps on network. It runs
// in the transaction that records the sweep as mined.
func (s *Sweeper) Hook(network safety.Network) txmgr.Hook {
	return func(ctx context.Context, tx pgx.Tx, row store.OutboundTx, r *types.Receipt) error {
		if r.Status != types.ReceiptStatusSuccessful {
			return nil // the manager logs the revert; nothing moved
		}
		d := s.Deployments[network]
		amount := new(big.Int)
		for _, l := range r.Logs {
			if l.Address == d.Factory && len(l.Topics) == 4 && l.Topics[0] == sweptTopic && len(l.Data) == 32 {
				amount.Add(amount, new(big.Int).SetBytes(l.Data))
			}
		}
		if amount.Sign() == 0 {
			return nil // an earlier sweep already moved this invoice's money
		}
		amt, err := money.FromBig(amount)
		if err != nil {
			return err
		}
		net := strings.ReplaceAll(string(network), ":", "_")
		asset := s.Asset.Code
		deposit, err := ledger.EnsureAccount(ctx, tx, ledger.Account{
			Code: fmt.Sprintf("custody:deposit:%s:%s", net, asset), Asset: asset, Kind: ledger.KindAsset, Rule: ledger.NonNegative,
		})
		if err != nil {
			return err
		}
		vault, err := ledger.EnsureAccount(ctx, tx, ledger.Account{
			Code: fmt.Sprintf("custody:vault:%s:%s", net, asset), Asset: asset, Kind: ledger.KindAsset, Rule: ledger.NonNegative,
		})
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.Entry{
			IdempotencyKey: "sweep:" + row.ID.String(),
			Kind:           "sweep.confirmed",
			RefType:        "outbound_tx",
			RefID:          row.ID.String(),
			Memo:           "swept in " + strings.ToLower(r.TxHash.Hex()),
			Postings: []ledger.Posting{
				{Account: vault.ID, Asset: asset, Amount: amt},
				{Account: deposit.ID, Asset: asset, Amount: amt.Neg()},
			},
		})
		if err != nil {
			return fmt.Errorf("sweep: ledger: %w", err)
		}
		s.Log.Info("sweep: recorded", "network", network, "amount", amt.String(), "tx", r.TxHash.Hex())
		return nil
	}
}
