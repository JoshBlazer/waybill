package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// errReplay marks a create request whose Idempotency-Key and body were
// already used: the resource exists, but its secret was shown once and is
// not stored, so the original response cannot be replayed.
type errReplay struct{ id string }

func (e errReplay) Error() string { return "already created: " + e.id }

// errKeyReused: the Idempotency-Key was used with a different body.
var errKeyReused = errors.New("idempotency key reused with a different request")

// createOnce runs create inside a transaction that also claims the
// idempotency key (invariant I6). create returns the public part of what it
// made (with an "id" field), which is stored with the key; secrets are never
// stored there. A retry with the same key and body gets errReplay naming the
// resource.
func (s *Server) createOnce(ctx context.Context, contractor uuid.UUID, key string, body any, create func(tx pgx.Tx) (public any, err error)) error {
	canonical, err := json.Marshal(body)
	if err != nil {
		return err
	}
	reqHash := sha256.Sum256(canonical)
	principal := "contractor:" + contractor.String()
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		_, err := q.ClaimIdempotencyKey(ctx, store.ClaimIdempotencyKeyParams{Principal: principal, Key: key, RequestHash: reqHash[:]})
		if errors.Is(err, pgx.ErrNoRows) {
			rec, err := q.GetIdempotencyRecord(ctx, store.GetIdempotencyRecordParams{Principal: principal, Key: key})
			if err != nil {
				return err
			}
			if string(rec.RequestHash) != string(reqHash[:]) {
				return errKeyReused
			}
			var stored struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(rec.ResponseBody, &stored)
			return errReplay{id: stored.ID}
		}
		if err != nil {
			return err
		}
		public, err := create(tx)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(public)
		if err != nil {
			return err
		}
		status := int32(http.StatusCreated)
		return q.CompleteIdempotencyKey(ctx, store.CompleteIdempotencyKeyParams{
			Principal: principal, Key: key, StatusCode: &status, ResponseBody: raw,
		})
	})
}

func validIdempotencyKey(k string) bool { return len(k) >= 1 && len(k) <= 255 }

// CreateApiKey creates a key. The full key is in this response only.
func (s *Server) CreateApiKey(ctx context.Context, req CreateApiKeyRequestObject) (CreateApiKeyResponseObject, error) { //nolint:revive // name fixed by the generated StrictServerInterface
	contractor, ok := auth.ContractorFrom(ctx)
	if !ok {
		return nil, errors.New("createApiKey: no authenticated contractor")
	}
	if req.Body == nil || !validIdempotencyKey(req.Params.IdempotencyKey) {
		return CreateApiKey400ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(
			problem(400, "bad-request", "A request body and a 1 to 255 character Idempotency-Key are required.", ""))}, nil
	}
	name := strings.TrimSpace(req.Body.Name)
	if name == "" || len([]rune(name)) > 100 {
		return CreateApiKey422ApplicationProblemPlusJSONResponse(problem(422, "validation", "The key is not valid.", "name must be 1 to 100 characters")), nil
	}
	var created CreatedApiKey
	err := s.createOnce(ctx, contractor, req.Params.IdempotencyKey, req.Body, func(tx pgx.Tx) (any, error) {
		k, err := auth.GenerateKey()
		if err != nil {
			return nil, err
		}
		row, err := store.New(tx).CreateAPIKey(ctx, store.CreateAPIKeyParams{
			ContractorID: contractor, Prefix: k.Prefix, SecretSha256: k.Hash[:], Name: name,
		})
		if err != nil {
			return nil, err
		}
		public := ApiKey{Id: row.ID, Name: row.Name, Prefix: auth.KeyPrefix + row.Prefix, CreatedAt: row.CreatedAt.UTC()}
		created = CreatedApiKey{Id: public.Id, Name: public.Name, Prefix: public.Prefix, CreatedAt: public.CreatedAt, Key: k.Full}
		return public, nil
	})
	var replay errReplay
	switch {
	case errors.As(err, &replay):
		return CreateApiKey409ApplicationProblemPlusJSONResponse(problem(409, "already-created",
			"This request already created a key, and a key is shown only once.",
			"Key "+replay.id+" exists. Revoke it if you did not save it, then create another.")), nil
	case errors.Is(err, errKeyReused):
		return CreateApiKey422ApplicationProblemPlusJSONResponse(problem(422, "idempotency-key-reused",
			"This Idempotency-Key was already used with a different request.", "")), nil
	case err != nil:
		return nil, err
	}
	return CreateApiKey201JSONResponse(created), nil
}

// ListApiKeys lists the contractor's keys without their secrets.
func (s *Server) ListApiKeys(ctx context.Context, _ ListApiKeysRequestObject) (ListApiKeysResponseObject, error) { //nolint:revive // name fixed by the generated StrictServerInterface
	contractor, ok := auth.ContractorFrom(ctx)
	if !ok {
		return nil, errors.New("listApiKeys: no authenticated contractor")
	}
	rows, err := store.New(s.Pool).ListAPIKeys(ctx, contractor)
	if err != nil {
		return nil, err
	}
	out := ApiKeyList{Items: make([]ApiKey, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, ApiKey{
			Id: r.ID, Name: r.Name, Prefix: auth.KeyPrefix + r.Prefix, CreatedAt: r.CreatedAt.UTC(),
			LastUsedAt: utcPtr(r.LastUsedAt), RevokedAt: utcPtr(r.RevokedAt),
		})
	}
	return ListApiKeys200JSONResponse(out), nil
}

// RevokeApiKey revokes a key, refusing to revoke the last active one.
func (s *Server) RevokeApiKey(ctx context.Context, req RevokeApiKeyRequestObject) (RevokeApiKeyResponseObject, error) { //nolint:revive // name fixed by the generated StrictServerInterface
	contractor, ok := auth.ContractorFrom(ctx)
	if !ok {
		return nil, errors.New("revokeApiKey: no authenticated contractor")
	}
	var notFound, lastKey bool
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		keys, err := q.LockContractorAPIKeys(ctx, contractor)
		if err != nil {
			return err
		}
		active, found, alreadyRevoked := 0, false, false
		for _, k := range keys {
			if k.RevokedAt == nil {
				active++
			}
			if k.ID == req.Id {
				found, alreadyRevoked = true, k.RevokedAt != nil
			}
		}
		switch {
		case !found:
			notFound = true
			return nil
		case alreadyRevoked:
			return nil
		case active <= 1:
			lastKey = true
			return nil
		}
		return q.RevokeAPIKey(ctx, req.Id)
	})
	switch {
	case err != nil:
		return nil, err
	case notFound:
		return RevokeApiKey404ApplicationProblemPlusJSONResponse(problem(404, "not-found", "There is no such key.", "")), nil
	case lastKey:
		return RevokeApiKey409ApplicationProblemPlusJSONResponse(problem(409, "last-key",
			"This is your only active key.", "Create another key first, so you are not locked out.")), nil
	}
	return RevokeApiKey204Response{}, nil
}
