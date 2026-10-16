package webhook_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/httpapi"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/outbox"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
	"github.com/JoshBlazer/waybill/api/internal/webhook"
)

var deployment = deployments.Deployment{
	Network: "evm:31337", ChainID: 31337,
	Token:          common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3"),
	Vault:          common.HexToAddress("0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512"),
	Factory:        common.HexToAddress("0x9fE46736679d2D9a65F0992F2272dE9f3c7fa6e0"),
	Implementation: common.HexToAddress("0x75537828f2ce51be7289709686A69CbFDbB714F1"),
}

// receiver is a contractor's webhook server that answers with the next
// status in its script (then 200) and keeps every request.
type receiver struct {
	mu       sync.Mutex
	statuses []int
	got      []received
	srv      *httptest.Server
}

type received struct {
	body      []byte
	signature string
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	r := &receiver{statuses: statuses}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.got = append(r.got, received{body: body, signature: req.Header.Get(webhook.SignatureHeader)})
		status := http.StatusOK
		if len(r.statuses) > 0 {
			status, r.statuses = r.statuses[0], r.statuses[1:]
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

type rig struct {
	pool       *pgxpool.Pool
	contractor uuid.UUID
	inv        invoice.Invoice
	worker     *outbox.Worker
}

func setup(t *testing.T, allowPrivate bool) rig {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	c, err := store.New(pool).UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: "Adaeze Okafor"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &invoice.Service{Deployments: map[safety.Network]deployments.Deployment{"evm:31337": deployment}, Now: time.Now}
	var inv invoice.Invoice
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		inv, err = svc.Create(ctx, tx, c.ID, invoice.Input{Description: "Work", Amount: "100", Asset: "USDC", Payout: "hold"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tokens := map[safety.Network]string{"evm:31337": evm.NormalizeAddress(deployment.Token)}
	d := &webhook.Deliverer{
		Pool: pool, Client: webhook.NewClient(allowPrivate, 2*time.Second), Log: slog.New(slog.DiscardHandler),
		Render: func(ctx context.Context, id uuid.UUID, state string) (any, error) {
			inv, err := invoice.LoadByID(ctx, pool, tokens, id)
			if err != nil {
				return nil, err
			}
			out := httpapi.InvoiceJSON("http://web.test", inv)
			out.State = httpapi.InvoiceState(state)
			return out, nil
		},
	}
	w := &outbox.Worker{
		Pool: pool, Owner: "test", Log: slog.New(slog.DiscardHandler), Lease: time.Minute, Batch: 10,
		Handlers: map[string]outbox.Handler{webhook.Topic: d.Handle},
		// The real schedule, without the waiting.
		Policies: map[string]outbox.Policy{webhook.Topic: {MaxAttempts: webhook.MaxAttempts, Backoff: func(int) time.Duration { return 0 }}},
	}
	return rig{pool: pool, contractor: c.ID, inv: inv, worker: w}
}

// endpoint registers an endpoint directly (the API's URL check is tested
// separately), returning its id and secret.
func (r rig) endpoint(t *testing.T, url string, events ...string) (uuid.UUID, string) {
	t.Helper()
	secret := webhook.NewSecret()
	ep, err := store.New(r.pool).CreateWebhookEndpoint(context.Background(), store.CreateWebhookEndpointParams{
		ContractorID: r.contractor, Url: url, Events: events, Secret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ep.ID, secret
}

func (r rig) apply(t *testing.T, ev statemachine.InvoiceEvent) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		_, err := invoice.Apply(context.Background(), tx, r.inv.ID, ev, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (r rig) run(t *testing.T, rounds int) {
	t.Helper()
	for range rounds {
		if _, err := r.worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

type delivery struct {
	attempt  int
	outcome  string
	status   *int32
	hasNext  bool
	errorMsg *string
}

func (r rig) deliveries(t *testing.T) []delivery {
	t.Helper()
	rows, err := r.pool.Query(context.Background(),
		`SELECT attempt, outcome, response_status, next_attempt_at IS NOT NULL, error FROM webhook_deliveries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []delivery
	for rows.Next() {
		var d delivery
		if err := rows.Scan(&d.attempt, &d.outcome, &d.status, &d.hasNext, &d.errorMsg); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func (r rig) jobState(t *testing.T) string {
	t.Helper()
	var s string
	if err := r.pool.QueryRow(context.Background(), `
		SELECT CASE WHEN done_at IS NOT NULL THEN 'done' WHEN dead_at IS NOT NULL THEN 'dead' ELSE 'pending' END
		FROM outbox WHERE topic = 'webhook.deliver'`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestWebhook_RetriesWithBackoff is invariant I13's delivery half: an
// endpoint that fails is retried, every attempt is logged with its outcome
// and next retry, every attempt carries a valid signature, and the event id
// is the same each time.
func TestWebhook_RetriesWithBackoff(t *testing.T) {
	r := setup(t, true)
	rcv := newReceiver(t, 500, 503) // fails twice, then 200
	_, secret := r.endpoint(t, rcv.srv.URL+"/hooks", "invoice.received")

	r.apply(t, statemachine.EvPaymentDetected)
	r.run(t, 5)

	got := r.deliveries(t)
	if len(got) != 3 {
		t.Fatalf("%d attempts logged, want 3: %+v", len(got), got)
	}
	for i, want := range []struct {
		outcome string
		status  int32
		next    bool
	}{{"failed", 500, true}, {"failed", 503, true}, {"succeeded", 200, false}} {
		d := got[i]
		if d.attempt != i+1 || d.outcome != want.outcome || d.status == nil || *d.status != want.status || d.hasNext != want.next {
			t.Fatalf("attempt %d logged as %+v (status %v); want %+v", i+1, d, d.status, want)
		}
	}
	if s := r.jobState(t); s != "done" {
		t.Fatalf("job is %s after success, want done", s)
	}

	if len(rcv.got) != 3 {
		t.Fatalf("receiver got %d requests, want 3", len(rcv.got))
	}
	var firstID string
	for i, req := range rcv.got {
		if err := webhook.Verify(secret, req.signature, req.body, time.Now()); err != nil {
			t.Fatalf("request %d: signature does not verify: %v", i+1, err)
		}
		var ev struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Data struct {
				Invoice struct {
					ID           string `json:"id"`
					TrackingCode string `json:"trackingCode"`
					State        string `json:"state"`
				} `json:"invoice"`
			} `json:"data"`
		}
		if err := json.Unmarshal(req.body, &ev); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstID = ev.ID
		}
		if ev.ID != firstID || ev.Type != "invoice.received" || ev.Data.Invoice.ID != r.inv.ID.String() ||
			ev.Data.Invoice.TrackingCode != r.inv.TrackingCode || ev.Data.Invoice.State != "received" {
			t.Fatalf("request %d body %s; want the same invoice.received event every time", i+1, req.body)
		}
	}
}

func TestWebhook_GivesUpAfterLastAttempt(t *testing.T) {
	r := setup(t, true)
	rcv := newReceiver(t, 500, 500, 500, 500)
	r.endpoint(t, rcv.srv.URL, "invoice.received")
	r.worker.Policies[webhook.Topic] = outbox.Policy{MaxAttempts: 3, Backoff: func(int) time.Duration { return 0 }}

	r.apply(t, statemachine.EvPaymentDetected)
	r.run(t, 6)

	if got := r.deliveries(t); len(got) != 3 || r.jobState(t) != "dead" {
		t.Fatalf("%d attempts, job %s; want 3 attempts and a dead job", len(got), r.jobState(t))
	}
}

// The event carries the state when it happened, not when it was sent.
func TestWebhook_EventCarriesStateAtCreation(t *testing.T) {
	r := setup(t, true)
	rcv := newReceiver(t)
	r.endpoint(t, rcv.srv.URL, "invoice.received", "invoice.confirmed")

	r.apply(t, statemachine.EvPaymentDetected) // received
	r.apply(t, statemachine.EvConfirmedExact)  // confirmed, before anything is sent
	r.run(t, 2)

	states := map[string]string{}
	for _, req := range rcv.got {
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Invoice struct {
					State string `json:"state"`
				} `json:"invoice"`
			} `json:"data"`
		}
		_ = json.Unmarshal(req.body, &ev)
		states[ev.Type] = ev.Data.Invoice.State
	}
	if states["invoice.received"] != "received" || states["invoice.confirmed"] != "confirmed" {
		t.Fatalf("event states %v; each must carry the state it was created with", states)
	}
}

func TestWebhook_OnlySubscribedEnabledEndpointsGetEvents(t *testing.T) {
	r := setup(t, true)
	wants := newReceiver(t)
	other := newReceiver(t)
	disabled := newReceiver(t)
	r.endpoint(t, wants.srv.URL, "invoice.received")
	r.endpoint(t, other.srv.URL, "invoice.settled")
	off, _ := r.endpoint(t, disabled.srv.URL, "invoice.received")
	if _, err := r.pool.Exec(context.Background(), `UPDATE webhook_endpoints SET disabled_at = now() WHERE id = $1`, off); err != nil {
		t.Fatal(err)
	}

	r.apply(t, statemachine.EvPaymentDetected)
	r.run(t, 2)

	if len(wants.got) != 1 || len(other.got) != 0 || len(disabled.got) != 0 {
		t.Fatalf("requests: subscribed %d, other type %d, disabled %d; want 1, 0, 0", len(wants.got), len(other.got), len(disabled.got))
	}
}

func TestWebhook_NoSubscribersWritesNothing(t *testing.T) {
	r := setup(t, true)
	r.apply(t, statemachine.EvPaymentDetected)
	var events, jobs int
	_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM webhook_events`).Scan(&events)
	_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox`).Scan(&jobs)
	if events != 0 || jobs != 0 {
		t.Fatalf("%d events, %d jobs without any endpoint; want none", events, jobs)
	}
}

func TestWebhook_EventIsPartOfTheStateChangeTransaction(t *testing.T) {
	r := setup(t, true)
	rcv := newReceiver(t)
	r.endpoint(t, rcv.srv.URL, "invoice.received")
	_ = pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		if _, err := invoice.Apply(context.Background(), tx, r.inv.ID, statemachine.EvPaymentDetected, nil); err != nil {
			t.Fatal(err)
		}
		return context.Canceled // roll back the state change
	})
	var events int
	_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM webhook_events`).Scan(&events)
	if events != 0 {
		t.Fatalf("%d events after the state change rolled back; want 0", events)
	}
}

// A URL that passed registration but resolves to a private address at
// delivery (DNS rebinding) is refused: nothing is sent, the attempt is
// logged as refused, and the job is not retried.
func TestWebhook_PrivateAddressAtDeliveryIsRefused(t *testing.T) {
	r := setup(t, false)
	rcv := newReceiver(t)
	r.endpoint(t, rcv.srv.URL, "invoice.received") // 127.0.0.1
	r.apply(t, statemachine.EvPaymentDetected)
	r.run(t, 3)

	got := r.deliveries(t)
	if len(rcv.got) != 0 || len(got) != 1 || got[0].outcome != "refused" || got[0].hasNext || r.jobState(t) != "dead" {
		t.Fatalf("sent %d, logged %+v, job %s; want nothing sent, one refused attempt, dead job", len(rcv.got), got, r.jobState(t))
	}
}
