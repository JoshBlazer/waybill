package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/batch"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

func walletJSON(w store.ContractorWallet) PayoutWallet {
	return PayoutWallet{
		Network: w.Network, Address: common.HexToAddress(w.Address).Hex(),
		AddedAt: w.AddedAt.UTC(), PayoutsAllowedFrom: w.PayoutsAllowedFrom.UTC(),
	}
}

func batchJSON(r batch.Run, withLines bool) Batch {
	out := Batch{
		Id: r.ID, State: BatchState(r.State), Network: r.Network, Total: usdcAmount(r.Total),
		CreatedBy: r.CreatedBy, NeedsApproval: r.NeedsApproval(), Problem: r.Failure, TxHash: r.TxHash,
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
	if !withLines {
		return out
	}
	lines := make([]BatchLine, len(r.Lines))
	for i, l := range r.Lines {
		var wallet *string
		if l.WalletAddress != nil {
			w := common.HexToAddress(*l.WalletAddress).Hex()
			wallet = &w
		}
		lines[i] = BatchLine{
			LineNo: int(l.LineNo), ContractorId: l.ContractorID, ContractorName: l.ContractorName,
			Verified: l.ContractorVerifiedAt != nil, WalletAddress: wallet, Amount: usdcAmount(l.Amount),
			Reference: l.Reference, Problem: l.Problem, TrackingCode: l.TrackingCode,
		}
	}
	approvals := make([]Approval, len(r.Approvals))
	for i, a := range r.Approvals {
		approvals[i] = Approval{ApproverId: a.ApproverID, Decision: ApprovalDecision(a.Decision), DecidedAt: a.DecidedAt.UTC()}
	}
	out.Lines, out.Approvals = &lines, &approvals
	return out
}

// ListPayoutWallets returns the contractor's payout wallets.
func (s *Server) ListPayoutWallets(ctx context.Context, _ ListPayoutWalletsRequestObject) (ListPayoutWalletsResponseObject, error) {
	c, _ := auth.ContractorFrom(ctx)
	ws, err := s.Batches.Wallets(ctx, c)
	if err != nil {
		return nil, err
	}
	out := PayoutWalletList{Data: make([]PayoutWallet, len(ws))}
	for i, w := range ws {
		out.Data[i] = walletJSON(w)
	}
	return ListPayoutWallets200JSONResponse(out), nil
}

// SetPayoutWallet saves the contractor's payout wallet on a network.
func (s *Server) SetPayoutWallet(ctx context.Context, req SetPayoutWalletRequestObject) (SetPayoutWalletResponseObject, error) {
	c, _ := auth.ContractorFrom(ctx)
	w, err := s.Batches.SetWallet(ctx, c, req.Body.Network, req.Body.Address)
	if errors.Is(err, batch.ErrBadWallet) {
		return SetPayoutWallet422ApplicationProblemPlusJSONResponse(problem(422, "bad-wallet",
			"That wallet can't be used for payouts.", err.Error())), nil
	}
	if err != nil {
		return nil, err
	}
	return SetPayoutWallet200JSONResponse(walletJSON(w)), nil
}

// batchProblem maps service errors to a problem and its status, or returns
// ok=false for an unexpected error.
func batchProblem(err error) (Problem, bool) {
	switch {
	case errors.Is(err, batch.ErrForbidden):
		return forbidden(), true
	case errors.Is(err, batch.ErrSelfApproval):
		return problem(403, "self-approval", "You can't approve or reject a run you created. Ask another approver.", ""), true
	case errors.Is(err, batch.ErrNotFound):
		return problem(404, "not-found", "No such run in your organisation.", ""), true
	case errors.Is(err, batch.ErrWrongState):
		return problem(409, "wrong-state", "The run can't do that now.", err.Error()), true
	case errors.Is(err, batch.ErrNotReady):
		return problem(422, "not-ready", "Some lines can't be paid.", err.Error()), true
	case errors.Is(err, batch.ErrNoFunds):
		return problem(422, "insufficient-funds", "The organisation's available balance is less than the run's total.", "Fund the organisation and try again."), true
	case errors.Is(err, batch.ErrOverDailyLimit):
		return problem(422, "over-daily-limit", "The vault can't pay this much more today.", err.Error()), true
	case errors.Is(err, batch.ErrInvalid):
		return problem(422, "validation", "The run is not valid.", err.Error()), true
	case errors.Is(err, batch.ErrNoContract):
		return problem(422, "no-batch-contract", "Batch payouts are not available on that network.", ""), true
	}
	return Problem{}, false
}

// CreateBatch builds and checks a run.
func (s *Server) CreateBatch(ctx context.Context, req CreateBatchRequestObject) (CreateBatchResponseObject, error) {
	p, _ := auth.MemberFrom(ctx)
	key := req.Params.IdempotencyKey
	canonical, err := json.Marshal(req.Body)
	if err != nil {
		return nil, err
	}
	reqHash := sha256.Sum256(canonical)
	principal := "member:" + p.MemberID.String()
	network := ""
	if req.Body.Network != nil {
		network = *req.Body.Network
	}
	lines := make([]batch.LineInput, len(req.Body.Lines))
	for i, l := range req.Body.Lines {
		lines[i] = batch.LineInput{ContractorID: l.ContractorId, Amount: l.Amount, Reference: l.Reference}
	}
	var (
		created *Batch
		replay  *store.IdempotencyKey
	)
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		_, err := q.ClaimIdempotencyKey(ctx, store.ClaimIdempotencyKeyParams{Principal: principal, Key: key, RequestHash: reqHash[:]})
		if errors.Is(err, pgx.ErrNoRows) {
			rec, err := q.GetIdempotencyRecord(ctx, store.GetIdempotencyRecordParams{Principal: principal, Key: key})
			if err != nil {
				return err
			}
			replay = &rec
			return nil
		}
		if err != nil {
			return err
		}
		run, err := s.Batches.Create(ctx, tx, p, network, lines)
		if err != nil {
			return err
		}
		out := batchJSON(run, true)
		body, err := json.Marshal(out)
		if err != nil {
			return err
		}
		status := int32(http.StatusCreated)
		if err := q.CompleteIdempotencyKey(ctx, store.CompleteIdempotencyKeyParams{
			Principal: principal, Key: key, StatusCode: &status, ResponseBody: body,
		}); err != nil {
			return err
		}
		created = &out
		return nil
	})
	if pr, ok := batchProblem(err); ok {
		switch pr.Status {
		case 403:
			return CreateBatch403ApplicationProblemPlusJSONResponse(pr), nil
		default:
			pr.Status = 422
			return CreateBatch422ApplicationProblemPlusJSONResponse(pr), nil
		}
	}
	if err != nil {
		return nil, err
	}
	if replay != nil {
		if !bytes.Equal(replay.RequestHash, reqHash[:]) {
			return CreateBatch422ApplicationProblemPlusJSONResponse(problem(422, "idempotency-key-reused",
				"This Idempotency-Key was already used with a different request.", "")), nil
		}
		var out Batch
		if err := json.Unmarshal(replay.ResponseBody, &out); err != nil {
			return nil, err
		}
		return CreateBatch201JSONResponse(out), nil
	}
	return CreateBatch201JSONResponse(*created), nil
}

// ListBatches lists the organisation's runs.
func (s *Server) ListBatches(ctx context.Context, _ ListBatchesRequestObject) (ListBatchesResponseObject, error) {
	p, _ := auth.MemberFrom(ctx)
	bs, err := s.Batches.List(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	o, err := store.New(s.Pool).GetOrg(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	out := BatchList{Data: make([]Batch, len(bs))}
	for i, b := range bs {
		out.Data[i] = batchJSON(batch.Run{Batch: b, Threshold: o.ApprovalThreshold}, false)
	}
	return ListBatches200JSONResponse(out), nil
}

// GetBatch returns one run.
func (s *Server) GetBatch(ctx context.Context, req GetBatchRequestObject) (GetBatchResponseObject, error) {
	p, _ := auth.MemberFrom(ctx)
	r, err := s.Batches.Get(ctx, p.OrgID, req.Id)
	if errors.Is(err, batch.ErrNotFound) {
		pr, _ := batchProblem(err)
		return GetBatch404ApplicationProblemPlusJSONResponse(pr), nil
	}
	if err != nil {
		return nil, err
	}
	return GetBatch200JSONResponse(batchJSON(r, true)), nil
}

// actionResult turns the outcome of a run action into the status and body
// every action endpoint shares.
func actionResult(r batch.Run, err error) (int, any, error) {
	if pr, ok := batchProblem(err); ok {
		return pr.Status, pr, nil
	}
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, batchJSON(r, true), nil
}

// ApproveBatch approves a run.
func (s *Server) ApproveBatch(ctx context.Context, req ApproveBatchRequestObject) (ApproveBatchResponseObject, error) {
	p, _ := auth.MemberFrom(ctx)
	status, body, err := actionResult(s.Batches.Approve(ctx, p, req.Id))
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		return ApproveBatch200JSONResponse(body.(Batch)), nil
	case 403:
		return ApproveBatch403ApplicationProblemPlusJSONResponse(body.(Problem)), nil
	case 404:
		return ApproveBatch404ApplicationProblemPlusJSONResponse(body.(Problem)), nil
	default:
		pr := body.(Problem)
		pr.Status = 409
		return ApproveBatch409ApplicationProblemPlusJSONResponse(pr), nil
	}
}

// RejectBatch rejects a run.
func (s *Server) RejectBatch(ctx context.Context, req RejectBatchRequestObject) (RejectBatchResponseObject, error) {
	p, _ := auth.MemberFrom(ctx)
	status, body, err := actionResult(s.Batches.Reject(ctx, p, req.Id))
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		return RejectBatch200JSONResponse(body.(Batch)), nil
	case 403:
		return RejectBatch403ApplicationProblemPlusJSONResponse(body.(Problem)), nil
	case 404:
		return RejectBatch404ApplicationProblemPlusJSONResponse(body.(Problem)), nil
	default:
		pr := body.(Problem)
		pr.Status = 409
		return RejectBatch409ApplicationProblemPlusJSONResponse(pr), nil
	}
}

// SendBatch sends a run.
func (s *Server) SendBatch(ctx context.Context, req SendBatchRequestObject) (SendBatchResponseObject, error) {
	p, _ := auth.MemberFrom(ctx)
	status, body, err := actionResult(s.Batches.Send(ctx, p, req.Id))
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		return SendBatch200JSONResponse(body.(Batch)), nil
	case 403:
		return SendBatch403ApplicationProblemPlusJSONResponse(body.(Problem)), nil
	case 404:
		return SendBatch404ApplicationProblemPlusJSONResponse(body.(Problem)), nil
	case 409:
		return SendBatch409ApplicationProblemPlusJSONResponse(body.(Problem)), nil
	default:
		return SendBatch422ApplicationProblemPlusJSONResponse(body.(Problem)), nil
	}
}

// CancelBatch withdraws a run.
func (s *Server) CancelBatch(ctx context.Context, req CancelBatchRequestObject) (CancelBatchResponseObject, error) {
	p, _ := auth.MemberFrom(ctx)
	status, body, err := actionResult(s.Batches.Cancel(ctx, p, req.Id))
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		return CancelBatch200JSONResponse(body.(Batch)), nil
	case 403:
		return CancelBatch403ApplicationProblemPlusJSONResponse(body.(Problem)), nil
	case 404:
		return CancelBatch404ApplicationProblemPlusJSONResponse(body.(Problem)), nil
	default:
		pr := body.(Problem)
		pr.Status = 409
		return CancelBatch409ApplicationProblemPlusJSONResponse(pr), nil
	}
}
