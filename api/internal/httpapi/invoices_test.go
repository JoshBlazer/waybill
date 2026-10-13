package httpapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/httpapi"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
)

// Real addresses from Deploy.s.sol on a fresh Anvil (see internal/evm tests).
var anvilDeployment = deployments.Deployment{
	Network:        "evm:31337",
	ChainID:        31337,
	Token:          common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3"),
	Vault:          common.HexToAddress("0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512"),
	Factory:        common.HexToAddress("0x9fE46736679d2D9a65F0992F2272dE9f3c7fa6e0"),
	Implementation: common.HexToAddress("0x75537828f2ce51be7289709686A69CbFDbB714F1"),
}

type env struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	key  string
}

func setup(t *testing.T) env {
	t.Helper()
	pool := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	q := store.New(pool)
	now := time.Now()
	c, err := q.UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: "Adaeze Okafor", VerifiedAt: &now})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := auth.GenerateKey()
	if err := q.InsertAPIKey(ctx, store.InsertAPIKeyParams{ContractorID: c.ID, Prefix: k.Prefix, SecretSha256: k.Hash[:]}); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.DiscardHandler)
	broker := httpapi.NewBroker(pool, log)
	go broker.Run(ctx)
	s := &httpapi.Server{
		DB:       store.New(pool),
		Pool:     pool,
		Invoices: &invoice.Service{Deployments: map[safety.Network]deployments.Deployment{"evm:31337": anvilDeployment}, Now: time.Now},
		Tokens:   map[safety.Network]string{"evm:31337": evm.NormalizeAddress(anvilDeployment.Token)},
		WebURL:   "http://web.test",
		Broker:   broker,
		Version:  "test",
		Log:      log,
	}
	srv := httptest.NewServer(httpapi.NewHandler(s))
	t.Cleanup(srv.Close)
	return env{pool: pool, srv: srv, key: k.Full}
}

const validBody = `{"description":"Design work, September","amount":"125.50","asset":"USDC","payout":"hold"}`

func (e env) create(t *testing.T, idemKey, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/invoices", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.key)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	return do(t, req)
}

func do(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func get(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	return do(t, req)
}

func countInvoices(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM invoices`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateInvoice(t *testing.T) {
	e := setup(t)
	code, body := e.create(t, "k1", validBody)
	if code != http.StatusCreated {
		t.Fatalf("status %d, body %v", code, body)
	}
	if !regexp.MustCompile(`^WB(-[0-9A-HJKMNP-TV-Z]{4}){4}$`).MatchString(body["trackingCode"].(string)) {
		t.Errorf("trackingCode = %v", body["trackingCode"])
	}
	amount := body["amount"].(map[string]any)
	if amount["minor"] != "125500000" || amount["scale"].(float64) != 6 || amount["asset"] != "USDC" {
		t.Errorf("amount = %v", amount)
	}
	if body["state"] != "open" || !strings.HasPrefix(body["payUrl"].(string), "http://web.test/pay/WB-") {
		t.Errorf("state/payUrl = %v / %v", body["state"], body["payUrl"])
	}
	// The deposit address must be what the factory will deploy to.
	id := uuid.MustParse(body["id"].(string))
	want := anvilDeployment.PredictDepositAddress(evm.SaltForInvoice(id)).Hex()
	addrs := body["depositAddresses"].([]any)
	if len(addrs) != 1 || addrs[0].(map[string]any)["address"] != want {
		t.Fatalf("depositAddresses = %v, want address %s", addrs, want)
	}
}

func TestCreateInvoice_IdempotentRetry(t *testing.T) {
	e := setup(t)
	_, first := e.create(t, "same-key", validBody)
	code, second := e.create(t, "same-key", validBody)
	if code != http.StatusCreated || second["id"] != first["id"] || second["trackingCode"] != first["trackingCode"] {
		t.Fatalf("retry = %d %v; want the original %v", code, second["id"], first["id"])
	}
	if n := countInvoices(t, e.pool); n != 1 {
		t.Fatalf("%d invoices after retry, want 1", n)
	}
}

func TestCreateInvoice_KeyReusedWithDifferentBody(t *testing.T) {
	e := setup(t)
	e.create(t, "k", validBody)
	code, body := e.create(t, "k", strings.Replace(validBody, "125.50", "999.00", 1))
	if code != http.StatusUnprocessableEntity || !strings.HasSuffix(body["type"].(string), "idempotency-key-reused") {
		t.Fatalf("got %d %v", code, body)
	}
	if n := countInvoices(t, e.pool); n != 1 {
		t.Fatalf("%d invoices, want 1", n)
	}
}

func TestCreateInvoice_ConcurrentSameKeyCreatesOne(t *testing.T) {
	e := setup(t)
	const n = 12
	ids := make(chan any, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := e.create(t, "race", validBody)
			if code != http.StatusCreated {
				t.Errorf("status %d %v", code, body)
			}
			ids <- body["id"]
		}()
	}
	wg.Wait()
	close(ids)
	var first any
	for id := range ids {
		if first == nil {
			first = id
		}
		if id != first {
			t.Fatalf("concurrent retries returned different invoices: %v and %v", first, id)
		}
	}
	if c := countInvoices(t, e.pool); c != 1 {
		t.Fatalf("%d invoices, want 1", c)
	}
}

func TestCreateInvoice_FailedRequestDoesNotBurnKey(t *testing.T) {
	e := setup(t)
	if code, _ := e.create(t, "k", strings.Replace(validBody, `"125.50"`, `"0"`, 1)); code != http.StatusUnprocessableEntity {
		t.Fatalf("zero amount: %d", code)
	}
	// The key was rolled back with the failed request; a corrected request
	// with a new body under the same key is a fresh request.
	if code, body := e.create(t, "k", validBody); code != http.StatusCreated {
		t.Fatalf("after failed attempt: %d %v", code, body)
	}
}

func TestCreateInvoice_Rejections(t *testing.T) {
	e := setup(t)
	cases := []struct {
		name   string
		key    string
		auth   string
		body   string
		status int
		slug   string
	}{
		{"no key header", "", "ok", validBody, 400, "bad-request"},
		{"no auth", "k1", "", validBody, 401, "unauthenticated"},
		{"bad auth", "k2", "Bearer wb_test_nope", validBody, 401, "unauthenticated"},
		{"malformed json", "k3", "ok", `{"description":`, 400, "bad-request"},
		{"too precise", "k4", "ok", strings.Replace(validBody, "125.50", "1.0000001", 1), 422, "validation"},
		{"empty description", "k5", "ok", strings.Replace(validBody, "Design work, September", "   ", 1), 422, "validation"},
		{"long description", "k6", "ok", strings.Replace(validBody, "Design work, September", strings.Repeat("x", 501), 1), 422, "validation"},
		{"unknown asset", "k7", "ok", strings.Replace(validBody, `"USDC"`, `"DOGE"`, 1), 422, "validation"},
		{"naira not yet", "k8", "ok", strings.Replace(validBody, `"hold"`, `"naira"`, 1), 422, "payout-unavailable"},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/invoices", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		if tc.key != "" {
			req.Header.Set("Idempotency-Key", tc.key)
		}
		switch tc.auth {
		case "ok":
			req.Header.Set("Authorization", "Bearer "+e.key)
		case "":
		default:
			req.Header.Set("Authorization", tc.auth)
		}
		code, body := do(t, req)
		if code != tc.status {
			t.Errorf("%s: status %d, want %d (%v)", tc.name, code, tc.status, body)
			continue
		}
		if typ, _ := body["type"].(string); !strings.HasSuffix(typ, "/"+tc.slug) {
			t.Errorf("%s: problem type %q, want …/%s", tc.name, typ, tc.slug)
		}
	}
	if n := countInvoices(t, e.pool); n != 0 {
		t.Fatalf("%d invoices created by rejected requests", n)
	}
}

func TestPublicViews(t *testing.T) {
	e := setup(t)
	_, inv := e.create(t, "k", validBody)
	code := inv["trackingCode"].(string)

	status, pay := get(t, e.srv.URL+"/v1/pay/"+code)
	if status != 200 || pay["contractorName"] != "Adaeze Okafor" || pay["verified"] != true {
		t.Fatalf("pay view %d %v", status, pay)
	}

	status, tr := get(t, e.srv.URL+"/v1/track/"+code)
	if status != 200 {
		t.Fatalf("track %d %v", status, tr)
	}
	steps := tr["steps"].([]any)
	if len(steps) != 4 || steps[0].(map[string]any)["status"] != "current" || steps[1].(map[string]any)["status"] != "waiting" {
		t.Fatalf("steps = %v", steps)
	}

	for _, path := range []string{"/v1/pay/WB-0000-0000-0000-0000", "/v1/track/WB-0000-0000-0000-0000"} {
		if status, body := get(t, e.srv.URL+path); status != 404 || !strings.HasSuffix(body["type"].(string), "not-found") {
			t.Errorf("%s: %d %v", path, status, body)
		}
	}
	// A malformed code gets the same 404 as an unknown one: no hint about
	// what a valid code looks like.
	if status, _ := get(t, e.srv.URL+"/v1/track/not-a-code"); status != 404 {
		t.Errorf("malformed code: %d, want 404", status)
	}
}

func TestStreamTracking_DeliversCurrentStateThenUpdates(t *testing.T) {
	e := setup(t)
	_, inv := e.create(t, "k", validBody)
	code := inv["trackingCode"].(string)
	id := uuid.MustParse(inv["id"].(string))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+"/v1/track/"+code+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	events := sseEvents(resp.Body)

	first := <-events
	if first["invoiceState"] != "open" {
		t.Fatalf("first event = %v", first)
	}

	// Simulate the watcher: change state and record the event in one commit.
	_, err = e.pool.Exec(context.Background(), `
		WITH u AS (UPDATE invoices SET state = 'received', updated_at = now() WHERE id = $1)
		INSERT INTO invoice_events (invoice_id, type, from_state, to_state) VALUES ($1, 'payment_detected', 'open', 'received')`, id)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case next := <-events:
		if next["invoiceState"] != "received" {
			t.Fatalf("update = %v", next)
		}
		if s := next["steps"].([]any)[0].(map[string]any)["status"]; s != "done" {
			t.Fatalf("received step = %v", s)
		}
	case <-ctx.Done():
		t.Fatal("no update delivered within 20s")
	}
}

// sseEvents parses `data:` lines of `event: tracking` messages.
func sseEvents(r io.Reader) <-chan map[string]any {
	out := make(chan map[string]any, 8)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var m map[string]any
				if err := json.Unmarshal([]byte(data), &m); err == nil {
					out <- m
				}
			}
		}
	}()
	return out
}

func TestAuthenticatedOperationsMatchSpec(t *testing.T) {
	spec, err := readSpecSecurity("../../../openapi/waybill.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for op := range spec {
		if !httpapi.IsAuthenticated(op) {
			t.Errorf("operation %s has security in the spec but is not authenticated", op)
		}
	}
	for _, op := range httpapi.AuthenticatedOperations() {
		if !spec[op] {
			t.Errorf("operation %s is authenticated in code but not in the spec", op)
		}
	}
	if len(spec) == 0 {
		t.Fatal("no secured operations found; the parser is wrong")
	}
}
