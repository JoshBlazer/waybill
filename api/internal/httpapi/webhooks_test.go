package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/webhook"
)

const validEndpoint = `{"url":"https://example.com/hooks/waybill","events":["invoice.received","invoice.settled"],"description":"Accounting"}`

func TestWebhookEndpoints_CreateListDelete(t *testing.T) {
	e := setup(t)
	code, created := e.call(t, e.key, http.MethodPost, "/v1/webhook-endpoints", "w1", validEndpoint)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, created)
	}
	if !regexp.MustCompile(`^whsec_test_[0-9A-Za-z]{32}$`).MatchString(created["secret"].(string)) ||
		created["enabled"] != true || created["url"] != "https://example.com/hooks/waybill" {
		t.Fatalf("created %v", created)
	}

	code, list := e.call(t, e.key, http.MethodGet, "/v1/webhook-endpoints", "", "")
	eps := items(list)
	if code != http.StatusOK || len(eps) != 1 || eps[0]["id"] != created["id"] {
		t.Fatalf("list: %d %v", code, list)
	}
	if _, ok := eps[0]["secret"]; ok {
		t.Fatal("list returned the signing secret")
	}

	path := "/v1/webhook-endpoints/" + created["id"].(string)
	if code, body := e.call(t, e.key, http.MethodDelete, path, "", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, body)
	}
	if code, _ := e.call(t, e.key, http.MethodDelete, path, "", ""); code != http.StatusNoContent {
		t.Fatalf("second delete: %d, want 204", code)
	}
	_, list = e.call(t, e.key, http.MethodGet, "/v1/webhook-endpoints", "", "")
	if items(list)[0]["enabled"] != false {
		t.Fatalf("after delete: %v; want the endpoint listed as disabled", list)
	}
}

func TestWebhookEndpoints_Validation(t *testing.T) {
	e := setup(t)
	bad := map[string]string{
		"http":                `{"url":"http://example.com/h","events":["invoice.received"]}`,
		"loopback":            `{"url":"https://127.0.0.1/h","events":["invoice.received"]}`,
		"metadata service":    `{"url":"https://169.254.169.254/latest","events":["invoice.received"]}`,
		"localhost":           `{"url":"https://localhost/h","events":["invoice.received"]}`,
		"credentials":         `{"url":"https://u:p@example.com/h","events":["invoice.received"]}`,
		"no events":           `{"url":"https://example.com/h","events":[]}`,
		"unknown event":       `{"url":"https://example.com/h","events":["invoice.paid"]}`,
		"duplicate event":     `{"url":"https://example.com/h","events":["invoice.received","invoice.received"]}`,
		"long description":    fmt.Sprintf(`{"url":"https://example.com/h","events":["invoice.received"],"description":%q}`, strings.Repeat("d", 201)),
		"url over 2000 chars": fmt.Sprintf(`{"url":"https://example.com/%s","events":["invoice.received"]}`, strings.Repeat("p", 2000)),
	}
	for name, body := range bad {
		if code, b := e.call(t, e.key, http.MethodPost, "/v1/webhook-endpoints", "v-"+name, body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %v, want 422", name, code, b)
		}
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM webhook_endpoints`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d endpoints stored from invalid requests", n)
	}
}

func TestWebhookEndpoints_IdempotentCreateDoesNotReplaySecret(t *testing.T) {
	e := setup(t)
	_, first := e.call(t, e.key, http.MethodPost, "/v1/webhook-endpoints", "same", validEndpoint)
	code, again := e.call(t, e.key, http.MethodPost, "/v1/webhook-endpoints", "same", validEndpoint)
	if code != http.StatusConflict || !strings.Contains(again["detail"].(string), first["id"].(string)) {
		t.Fatalf("retry: %d %v; want 409 naming endpoint %v", code, again, first["id"])
	}
	var leaked int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_keys WHERE position($1::bytea in response_body) > 0`,
		[]byte(first["secret"].(string))).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("the signing secret was stored with the idempotency record")
	}
}

func TestWebhookDeliveries_PagedNewestFirstAndPrivate(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, created := e.call(t, e.key, http.MethodPost, "/v1/webhook-endpoints", "w1", validEndpoint)
	epID := uuid.MustParse(created["id"].(string))

	// Five logged attempts for one event, written as the deliverer would.
	var contractor, invoiceID uuid.UUID
	_ = e.pool.QueryRow(ctx, `SELECT contractor_id FROM webhook_endpoints WHERE id = $1`, epID).Scan(&contractor)
	_, inv := e.create(t, "inv", validBody)
	invoiceID = uuid.MustParse(inv["id"].(string))
	q := store.New(e.pool)
	evID := webhook.NewEventID(time.Now())
	if err := q.InsertWebhookEvent(ctx, store.InsertWebhookEventParams{ID: evID, ContractorID: contractor, Type: "invoice.received", InvoiceID: invoiceID, InvoiceState: "received"}); err != nil {
		t.Fatal(err)
	}
	for a := int32(1); a <= 5; a++ {
		status := int32(500)
		if err := q.InsertWebhookDelivery(ctx, store.InsertWebhookDeliveryParams{EndpointID: epID, EventID: evID, Attempt: a, Outcome: "failed", ResponseStatus: &status}); err != nil {
			t.Fatal(err)
		}
	}

	path := "/v1/webhook-endpoints/" + epID.String() + "/deliveries"
	code, page1 := e.call(t, e.key, http.MethodGet, path+"?limit=2", "", "")
	got1 := items(page1)
	if code != http.StatusOK || len(got1) != 2 || got1[0]["attempt"].(float64) != 5 || page1["nextBefore"] == nil {
		t.Fatalf("page 1: %d %v", code, page1)
	}
	if got1[0]["eventId"] != evID || got1[0]["eventType"] != "invoice.received" || got1[0]["responseStatus"].(float64) != 500 {
		t.Fatalf("page 1 entry %v", got1[0])
	}
	_, page2 := e.call(t, e.key, http.MethodGet, fmt.Sprintf("%s?limit=2&before=%d", path, int64(page1["nextBefore"].(float64))), "", "")
	_, page3 := e.call(t, e.key, http.MethodGet, fmt.Sprintf("%s?limit=2&before=%d", path, int64(page2["nextBefore"].(float64))), "", "")
	if len(items(page2)) != 2 || items(page2)[0]["attempt"].(float64) != 3 || len(items(page3)) != 1 || page3["nextBefore"] != nil {
		t.Fatalf("page 2 %v, page 3 %v", page2, page3)
	}

	// Another contractor sees neither the endpoint nor its log.
	other := e.otherContractorKey(t)
	if code, _ := e.call(t, other, http.MethodGet, path, "", ""); code != http.StatusNotFound {
		t.Fatalf("another contractor reading the log: %d, want 404", code)
	}
	if code, _ := e.call(t, other, http.MethodDelete, "/v1/webhook-endpoints/"+epID.String(), "", ""); code != http.StatusNotFound {
		t.Fatalf("another contractor deleting the endpoint: %d, want 404", code)
	}
}

// Deleting an endpoint cancels its pending deliveries.
func TestWebhookEndpoints_DeleteCancelsPendingDeliveries(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, created := e.call(t, e.key, http.MethodPost, "/v1/webhook-endpoints", "w1", validEndpoint)
	if _, err := e.pool.Exec(ctx, `INSERT INTO outbox (topic, payload) VALUES ('webhook.deliver', jsonb_build_object('eventId', 'evt_X', 'endpointId', $1::text))`, created["id"]); err != nil {
		t.Fatal(err)
	}
	e.call(t, e.key, http.MethodDelete, "/v1/webhook-endpoints/"+created["id"].(string), "", "")
	var pending int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE done_at IS NULL AND dead_at IS NULL`).Scan(&pending)
	if pending != 0 {
		t.Fatalf("%d deliveries still pending to a deleted endpoint", pending)
	}
}
