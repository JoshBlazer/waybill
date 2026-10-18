package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/JoshBlazer/waybill/api/internal/btc"
	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// The BIP84 test vector account key, test-network version bytes.
const testTpub = "tpubDCxX2sYFS5bDkSe5GKKYHjBW7tgyN1R3UchpLJvdbf54ohxeGRtd8MbDUe1cguVHe4vnK68DsuD5MXjxi9EXx16rb9EnNsaF5KT99CinaJz"

func TestCreateInvoice_BTC(t *testing.T) {
	e := setup(t)
	// Without a Bitcoin network a BTC invoice is refused, not a 500.
	if code, body := e.create(t, "b0", `{"description":"Audit","amount":"0.015","asset":"BTC","payout":"hold"}`); code != 422 || body["type"] != "https://waybill.dev/problems/no-network" {
		t.Fatalf("no Bitcoin network: %d %v", code, body)
	}

	d, err := btc.ParseXPub(safety.BTCRegtest, testTpub)
	if err != nil {
		t.Fatal(err)
	}
	e.server.Invoices.BTC = map[safety.Network]*btc.Deriver{safety.BTCRegtest: d}
	code, inv := e.create(t, "b1", `{"description":"Audit","amount":"0.015","asset":"BTC","payout":"hold"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, inv)
	}
	addrs := inv["depositAddresses"].([]any)
	if len(addrs) != 1 {
		t.Fatalf("addresses %v; want only the Bitcoin one", addrs)
	}
	a := addrs[0].(map[string]any)
	want, _ := d.Address(0)
	if a["network"] != "btc:regtest" || a["address"] != want || a["token"] != nil || a["uri"] != "bitcoin:"+want+"?amount=0.015" {
		t.Fatalf("address %v", a)
	}
	if amt := inv["amount"].(map[string]any); amt["asset"] != "BTC" || amt["minor"] != "1500000" || amt["scale"] != float64(8) {
		t.Fatalf("amount %v", amt)
	}
	// The payment page shows the same address.
	code, link := get(t, e.srv.URL+"/v1/pay/"+inv["trackingCode"].(string))
	if code != http.StatusOK || !strings.Contains(link["depositAddresses"].([]any)[0].(map[string]any)["address"].(string), "bcrt1q") {
		t.Fatalf("payment link: %d %v", code, link)
	}
	// BTC is held as BTC.
	if code, body := e.create(t, "b2", `{"description":"Audit","amount":"0.015","asset":"BTC","payout":"naira"}`); code != 422 {
		t.Fatalf("BTC with naira payout: %d %v", code, body)
	}
}
