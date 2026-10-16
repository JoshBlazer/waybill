package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// call sends method to path with key as the bearer token.
func (e env) call(t *testing.T, key, method, path, idemKey, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	return do(t, req)
}

func items(body map[string]any) []map[string]any {
	raw, _ := body["items"].([]any)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i] = r.(map[string]any)
	}
	return out
}

func TestAPIKeys_CreateUseListRevoke(t *testing.T) {
	e := setup(t)

	code, created := e.call(t, e.key, http.MethodPost, "/v1/api-keys", "k1", `{"name":"Accounting export"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, created)
	}
	newKey, _ := created["key"].(string)
	if !regexp.MustCompile(`^wb_test_[0-9a-z]{8}_[a-z2-7]{52}$`).MatchString(newKey) ||
		created["name"] != "Accounting export" || !strings.HasPrefix(newKey, created["prefix"].(string)+"_") {
		t.Fatalf("created key %v", created)
	}
	// The secret is stored only as a hash.
	var stored int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM api_keys WHERE secret_sha256 = sha256($1::bytea)`, []byte(newKey)).Scan(&stored)
	if stored != 1 {
		t.Fatal("the new key's hash is not stored")
	}
	var leaked int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_keys WHERE position($1::bytea in response_body) > 0`, []byte(newKey)).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("the full key was stored with the idempotency record")
	}

	// The new key works, and its use is recorded.
	if code, body := e.call(t, newKey, http.MethodGet, "/v1/api-keys", "", ""); code != http.StatusOK {
		t.Fatalf("list with the new key: %d %v", code, body)
	}
	code, list := e.call(t, e.key, http.MethodGet, "/v1/api-keys", "", "")
	keys := items(list)
	if code != http.StatusOK || len(keys) != 2 {
		t.Fatalf("list: %d %v", code, list)
	}
	for _, k := range keys {
		if _, ok := k["key"]; ok {
			t.Fatalf("list returned a secret: %v", k)
		}
		if k["id"] == created["id"] && k["lastUsedAt"] == nil {
			t.Fatalf("used key has no lastUsedAt: %v", k)
		}
	}

	// Revoke the new key: it stops working; revoking again is fine.
	path := "/v1/api-keys/" + created["id"].(string)
	if code, body := e.call(t, e.key, http.MethodDelete, path, "", ""); code != http.StatusNoContent {
		t.Fatalf("revoke: %d %v", code, body)
	}
	if code, _ := e.call(t, newKey, http.MethodGet, "/v1/api-keys", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("revoked key answered %d, want 401", code)
	}
	if code, _ := e.call(t, e.key, http.MethodDelete, path, "", ""); code != http.StatusNoContent {
		t.Fatalf("second revoke: %d, want 204", code)
	}
}

func TestAPIKeys_LastActiveKeyCannotBeRevoked(t *testing.T) {
	e := setup(t)
	_, list := e.call(t, e.key, http.MethodGet, "/v1/api-keys", "", "")
	only := items(list)[0]["id"].(string)
	code, body := e.call(t, e.key, http.MethodDelete, "/v1/api-keys/"+only, "", "")
	if code != http.StatusConflict || !strings.HasSuffix(body["type"].(string), "/last-key") {
		t.Fatalf("revoking the only key: %d %v; want 409 last-key", code, body)
	}
	if code, _ := e.call(t, e.key, http.MethodGet, "/v1/api-keys", "", ""); code != http.StatusOK {
		t.Fatal("the key stopped working")
	}
}

func TestAPIKeys_IdempotentCreate(t *testing.T) {
	e := setup(t)
	code, first := e.call(t, e.key, http.MethodPost, "/v1/api-keys", "same", `{"name":"CI"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, first)
	}
	code, again := e.call(t, e.key, http.MethodPost, "/v1/api-keys", "same", `{"name":"CI"}`)
	if code != http.StatusConflict || !strings.Contains(again["detail"].(string), first["id"].(string)) {
		t.Fatalf("retry: %d %v; want 409 naming key %v (the key is shown once)", code, again, first["id"])
	}
	if code, _ := e.call(t, e.key, http.MethodPost, "/v1/api-keys", "same", `{"name":"Other"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("same Idempotency-Key, different body: %d, want 422", code)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM api_keys`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d keys; the retries must not create more", n)
	}
}

func TestAPIKeys_Validation(t *testing.T) {
	e := setup(t)
	for _, body := range []string{`{"name":""}`, `{"name":"   "}`, fmt.Sprintf(`{"name":%q}`, strings.Repeat("x", 101))} {
		if code, b := e.call(t, e.key, http.MethodPost, "/v1/api-keys", "v-"+body, body); code != http.StatusUnprocessableEntity {
			t.Errorf("body %s: %d %v, want 422", body, code, b)
		}
	}
	if code, _ := e.call(t, e.key, http.MethodPost, "/v1/api-keys", "", `{"name":"x"}`); code != http.StatusBadRequest {
		t.Errorf("no Idempotency-Key: %d, want 400", code)
	}
	if code, _ := e.call(t, "", http.MethodGet, "/v1/api-keys", "", ""); code != http.StatusUnauthorized {
		t.Errorf("no key: %d, want 401", code)
	}
}

// Another contractor's key is invisible: listing does not show it and
// revoking it is a 404, never a 403 that would confirm it exists.
func TestAPIKeys_OtherContractorsKeysAreInvisible(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	q := store.New(e.pool)
	other, _ := q.UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: "Someone Else"})
	k, _ := auth.GenerateKey()
	row, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{ContractorID: other.ID, Prefix: k.Prefix, SecretSha256: k.Hash[:], Name: "theirs"})
	if err != nil {
		t.Fatal(err)
	}
	_, list := e.call(t, e.key, http.MethodGet, "/v1/api-keys", "", "")
	if len(items(list)) != 1 {
		t.Fatalf("list shows %d keys; another contractor's key leaked", len(items(list)))
	}
	if code, _ := e.call(t, e.key, http.MethodDelete, "/v1/api-keys/"+row.ID.String(), "", ""); code != http.StatusNotFound {
		t.Fatalf("revoking another contractor's key: %d, want 404", code)
	}
}

// otherContractorKey creates a second contractor with a key and returns it.
func (e env) otherContractorKey(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	q := store.New(e.pool)
	other, err := q.UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: "Someone Else"})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := auth.GenerateKey()
	if _, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{ContractorID: other.ID, Prefix: k.Prefix, SecretSha256: k.Hash[:], Name: "theirs"}); err != nil {
		t.Fatal(err)
	}
	return k.Full
}
