package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshBlazer/waybill/api/internal/pdf"
)

// raw makes a request and returns the status, headers and body unparsed.
func raw(t *testing.T, base, key, path string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, body
}

func TestDocuments_InvoicePDF(t *testing.T) {
	e := setup(t)
	_, inv := e.create(t, "k1", validBody)
	id, code := inv["id"].(string), inv["trackingCode"].(string)
	path := "/v1/invoices/" + id + "/document.pdf"

	status, h, body := raw(t, e.srv.URL, e.key, path)
	if status != http.StatusOK || h.Get("Content-Type") != "application/pdf" || h.Get("Content-Disposition") != `attachment; filename="invoice-`+code+`.pdf"` {
		t.Fatalf("%d %s %q", status, h.Get("Content-Type"), h.Get("Content-Disposition"))
	}
	if err := pdf.Check(body); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(pdf.TextLines(body), "\n")
	for _, want := range []string{"Invoice", code, "Adaeze Okafor", "USDC 125.50", "http://web.test/pay/" + code, "Test money only. Nothing here is real."} {
		if !strings.Contains(text, want) {
			t.Errorf("invoice PDF lacks %q", want)
		}
	}

	if status, _, _ := raw(t, e.srv.URL, "", path); status != http.StatusUnauthorized {
		t.Fatalf("no key: %d, want 401", status)
	}
	if status, _, _ := raw(t, e.srv.URL, e.otherContractorKey(t), path); status != http.StatusNotFound {
		t.Fatalf("another contractor: %d, want 404", status)
	}
}

func TestDocuments_InvoiceReceiptOnlyOnceSettled(t *testing.T) {
	e := setup(t)
	_, inv := e.create(t, "k1", validBody)
	id := inv["id"].(string)
	for _, p := range []string{"/receipt", "/receipt.pdf"} {
		if status, _, _ := raw(t, e.srv.URL, e.key, "/v1/invoices/"+id+p); status != http.StatusConflict {
			t.Fatalf("%s of an open invoice: %d, want 409", p, status)
		}
	}
	e.underpay(t, id, "final")
	if code, _ := e.call(t, e.key, http.MethodPost, "/v1/invoices/"+id+"/accept-shortfall", "a1", ""); code != http.StatusOK {
		t.Fatalf("accept shortfall: %d", code)
	}
	status, _, body := raw(t, e.srv.URL, e.key, "/v1/invoices/"+id+"/receipt")
	var rec struct {
		Received struct{ Minor string }    `json:"received"`
		Payments []struct{ TxHash string } `json:"payments"`
	}
	if err := json.Unmarshal(body, &rec); status != http.StatusOK || err != nil || rec.Received.Minor != "60000000" || len(rec.Payments) != 1 {
		t.Fatalf("receipt: %d %s", status, body)
	}
	status, h, doc := raw(t, e.srv.URL, e.key, "/v1/invoices/"+id+"/receipt.pdf")
	if status != http.StatusOK || h.Get("Content-Type") != "application/pdf" {
		t.Fatalf("receipt PDF: %d %s", status, h.Get("Content-Type"))
	}
	if err := pdf.Check(doc); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := raw(t, e.srv.URL, e.otherContractorKey(t), "/v1/invoices/"+id+"/receipt"); status != http.StatusNotFound {
		t.Fatalf("another contractor's receipt: %d, want 404", status)
	}
}

func TestDocuments_ExportAcceptsEitherKeyAndChecksTheRange(t *testing.T) {
	e := setupOrg(t)
	today := time.Now().UTC().Format(time.DateOnly)
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format(time.DateOnly)
	path := "/v1/exports/ledger.csv?from=" + today + "&to=" + tomorrow
	for name, key := range map[string]string{"contractor": e.key, "member": e.adminKey} {
		status, h, body := raw(t, e.srv.URL, key, path)
		if status != http.StatusOK || h.Get("Content-Type") != "text/csv" || !strings.HasPrefix(string(body), "date,waybill_reference,description,") {
			t.Fatalf("%s key: %d %s %q", name, status, h.Get("Content-Type"), body)
		}
		if !strings.Contains(h.Get("Content-Disposition"), "waybill-"+today+"-to-"+tomorrow+".csv") {
			t.Fatalf("%s key: Content-Disposition %q", name, h.Get("Content-Disposition"))
		}
	}
	if status, _, _ := raw(t, e.srv.URL, "", path); status != http.StatusUnauthorized {
		t.Fatalf("no key: %d, want 401", status)
	}
	for _, q := range []string{"?from=" + tomorrow + "&to=" + today, "?from=2025-01-01&to=2026-01-03", "?from=" + today + "&to=" + today} {
		if status, _, _ := raw(t, e.srv.URL, e.key, "/v1/exports/ledger.csv"+q); status != http.StatusUnprocessableEntity {
			t.Fatalf("range %s: %d, want 422", q, status)
		}
	}
}

func TestDocuments_RunReceiptNeedsAMemberAndAPaidRun(t *testing.T) {
	b := setupBatches(t)
	b.fund(t, 100)
	code, run := b.create(t, b.adminKey, "r1", line(b.c1, "10", "x"))
	if code != http.StatusCreated {
		t.Fatalf("create run: %d %v", code, run)
	}
	path := "/v1/batches/" + run["id"].(string) + "/receipt"
	if status, _, _ := raw(t, b.srv.URL, b.adminKey, path); status != http.StatusConflict {
		t.Fatalf("receipt of an unpaid run: %d, want 409", status)
	}
	if status, _, _ := raw(t, b.srv.URL, b.key, path); status != http.StatusUnauthorized {
		t.Fatalf("contractor key: %d, want 401", status)
	}
	if status, _, _ := raw(t, b.srv.URL, b.adminKey, "/v1/batches/"+uuid.NewString()+"/receipt.pdf"); status != http.StatusNotFound {
		t.Fatalf("unknown run: %d, want 404", status)
	}
}
