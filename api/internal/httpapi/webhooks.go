package httpapi

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/webhook"
)

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func endpointJSON(e store.WebhookEndpoint) WebhookEndpoint {
	events := make([]WebhookEventType, len(e.Events))
	for i, ev := range e.Events {
		events[i] = WebhookEventType(ev)
	}
	return WebhookEndpoint{
		Id: e.ID, Url: e.Url, Events: events, Description: e.Description,
		Enabled: e.DisabledAt == nil, CreatedAt: e.CreatedAt.UTC(),
	}
}

// CreateWebhookEndpoint registers an endpoint. The signing secret is in this
// response only.
func (s *Server) CreateWebhookEndpoint(ctx context.Context, req CreateWebhookEndpointRequestObject) (CreateWebhookEndpointResponseObject, error) {
	contractor, ok := auth.ContractorFrom(ctx)
	if !ok {
		return nil, errors.New("createWebhookEndpoint: no authenticated contractor")
	}
	if req.Body == nil || !validIdempotencyKey(req.Params.IdempotencyKey) {
		return CreateWebhookEndpoint400ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(
			problem(400, "bad-request", "A request body and a 1 to 255 character Idempotency-Key are required.", ""))}, nil
	}
	invalid := func(detail string) (CreateWebhookEndpointResponseObject, error) {
		return CreateWebhookEndpoint422ApplicationProblemPlusJSONResponse(problem(422, "validation", "The webhook endpoint is not valid.", detail)), nil
	}
	b := req.Body
	if len(b.Url) > 2000 {
		return invalid("url must be at most 2000 characters")
	}
	if err := webhook.CheckURL(b.Url, s.WebhookAllowPrivate); err != nil {
		return invalid(err.Error())
	}
	if len(b.Events) == 0 {
		return invalid("events must name at least one event type")
	}
	seen := map[WebhookEventType]bool{}
	events := make([]string, 0, len(b.Events))
	for _, ev := range b.Events {
		if !ev.Valid() {
			return invalid("unknown event type " + string(ev))
		}
		if seen[ev] {
			return invalid("event type " + string(ev) + " is listed twice")
		}
		seen[ev] = true
		events = append(events, string(ev))
	}
	var desc *string
	if b.Description != nil {
		d := strings.TrimSpace(*b.Description)
		if len([]rune(d)) > 200 {
			return invalid("description must be at most 200 characters")
		}
		if d != "" {
			desc = &d
		}
	}

	var created CreatedWebhookEndpoint
	err := s.createOnce(ctx, contractor, req.Params.IdempotencyKey, b, func(tx pgx.Tx) (any, error) {
		secret := webhook.NewSecret()
		row, err := store.New(tx).CreateWebhookEndpoint(ctx, store.CreateWebhookEndpointParams{
			ContractorID: contractor, Url: b.Url, Events: events, Description: desc, Secret: secret,
		})
		if err != nil {
			return nil, err
		}
		public := endpointJSON(row)
		created = CreatedWebhookEndpoint{
			Id: public.Id, Url: public.Url, Events: public.Events, Description: public.Description,
			Enabled: public.Enabled, CreatedAt: public.CreatedAt, Secret: secret,
		}
		return public, nil
	})
	var replay errReplay
	switch {
	case errors.As(err, &replay):
		return CreateWebhookEndpoint409ApplicationProblemPlusJSONResponse(problem(409, "already-created",
			"This request already created an endpoint, and its secret is shown only once.",
			"Endpoint "+replay.id+" exists. Delete it if you did not save the secret, then create another.")), nil
	case errors.Is(err, errKeyReused):
		return CreateWebhookEndpoint422ApplicationProblemPlusJSONResponse(problem(422, "idempotency-key-reused",
			"This Idempotency-Key was already used with a different request.", "")), nil
	case err != nil:
		return nil, err
	}
	return CreateWebhookEndpoint201JSONResponse(created), nil
}

// ListWebhookEndpoints lists the contractor's endpoints without secrets.
func (s *Server) ListWebhookEndpoints(ctx context.Context, _ ListWebhookEndpointsRequestObject) (ListWebhookEndpointsResponseObject, error) {
	contractor, ok := auth.ContractorFrom(ctx)
	if !ok {
		return nil, errors.New("listWebhookEndpoints: no authenticated contractor")
	}
	rows, err := store.New(s.Pool).ListWebhookEndpoints(ctx, contractor)
	if err != nil {
		return nil, err
	}
	out := WebhookEndpointList{Items: make([]WebhookEndpoint, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, endpointJSON(r))
	}
	return ListWebhookEndpoints200JSONResponse(out), nil
}

// ownedEndpoint returns the endpoint if it belongs to the contractor. Another
// contractor's endpoint is reported as not found, not forbidden.
func (s *Server) ownedEndpoint(ctx context.Context, db store.DBTX, id ResourceId) (store.WebhookEndpoint, bool, error) {
	contractor, ok := auth.ContractorFrom(ctx)
	if !ok {
		return store.WebhookEndpoint{}, false, errors.New("webhooks: no authenticated contractor")
	}
	ep, err := store.New(db).GetWebhookEndpoint(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.WebhookEndpoint{}, false, nil
	}
	if err != nil {
		return store.WebhookEndpoint{}, false, err
	}
	return ep, ep.ContractorID == contractor, nil
}

// DeleteWebhookEndpoint disables an endpoint and cancels its pending
// deliveries. The delivery log is kept.
func (s *Server) DeleteWebhookEndpoint(ctx context.Context, req DeleteWebhookEndpointRequestObject) (DeleteWebhookEndpointResponseObject, error) {
	found := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		ep, ok, err := s.ownedEndpoint(ctx, tx, req.Id)
		if err != nil || !ok {
			return err
		}
		found = true
		q := store.New(tx)
		if _, err := q.DisableWebhookEndpoint(ctx, store.DisableWebhookEndpointParams{ID: ep.ID, ContractorID: ep.ContractorID}); err != nil {
			return err
		}
		return q.CancelWebhookJobs(ctx, ep.ID.String())
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return DeleteWebhookEndpoint404ApplicationProblemPlusJSONResponse(problem(404, "not-found", "There is no such webhook endpoint.", "")), nil
	}
	return DeleteWebhookEndpoint204Response{}, nil
}

// ListWebhookDeliveries pages through an endpoint's delivery log, newest
// first.
func (s *Server) ListWebhookDeliveries(ctx context.Context, req ListWebhookDeliveriesRequestObject) (ListWebhookDeliveriesResponseObject, error) {
	ep, ok, err := s.ownedEndpoint(ctx, s.Pool, req.Id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return ListWebhookDeliveries404ApplicationProblemPlusJSONResponse(problem(404, "not-found", "There is no such webhook endpoint.", "")), nil
	}
	limit := 50
	if req.Params.Limit != nil {
		limit = min(max(*req.Params.Limit, 1), 100)
	}
	before := int64(math.MaxInt64)
	if req.Params.Before != nil {
		before = *req.Params.Before
	}
	rows, err := store.New(s.Pool).ListWebhookDeliveries(ctx, store.ListWebhookDeliveriesParams{
		EndpointID: ep.ID, Before: before, Max: int32(limit + 1),
	})
	if err != nil {
		return nil, err
	}
	out := WebhookDeliveryList{Items: make([]WebhookDelivery, 0, min(len(rows), limit))}
	for i, r := range rows {
		if i == limit {
			next := out.Items[limit-1].Id
			out.NextBefore = &next
			break
		}
		d := WebhookDelivery{
			Id: r.ID, EventId: r.EventID, EventType: WebhookEventType(r.EventType), Attempt: int(r.Attempt),
			Outcome: WebhookDeliveryOutcome(r.Outcome), Error: r.Error,
			NextAttemptAt: utcPtr(r.NextAttemptAt), CreatedAt: r.CreatedAt.UTC(),
		}
		if r.ResponseStatus != nil {
			v := int(*r.ResponseStatus)
			d.ResponseStatus = &v
		}
		if r.LatencyMs != nil {
			v := int(*r.LatencyMs)
			d.LatencyMs = &v
		}
		out.Items = append(out.Items, d)
	}
	return ListWebhookDeliveries200JSONResponse(out), nil
}
