package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/org"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// orgEnv adds an organisation with an admin (admin, maker) and an approver
// to the test server, and returns their keys.
type orgEnv struct {
	env
	orgID                uuid.UUID
	admin, approver      uuid.UUID
	adminKey, approverKy string
}

func setupOrg(t *testing.T) orgEnv {
	t.Helper()
	e := setup(t)
	ctx := context.Background()
	q := store.New(e.pool)
	o, err := q.UpsertOrg(ctx, store.UpsertOrgParams{ID: uuid.New(), Name: "Lagoon Labs", ApprovalThreshold: money.FromInt64(500_000_000)})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, roles ...string) (uuid.UUID, string) {
		m, err := q.UpsertMember(ctx, store.UpsertMemberParams{ID: uuid.New(), OrgID: o.ID, Name: name, Roles: roles})
		if err != nil {
			t.Fatal(err)
		}
		k, _ := auth.GenerateKey()
		if err := q.InsertMemberAPIKey(ctx, store.InsertMemberAPIKeyParams{MemberID: m.ID, Prefix: k.Prefix, SecretSha256: k.Hash[:], Name: "test"}); err != nil {
			t.Fatal(err)
		}
		return m.ID, k.Full
	}
	admin, adminKey := mk("Tolu Bakare", "admin", "maker")
	approver, approverKey := mk("Chidi Eze", "approver")
	e.server.Org = &org.Service{Pool: e.pool, Deployments: map[safety.Network]deployments.Deployment{"evm:31337": anvilDeployment}}
	return orgEnv{env: e, orgID: o.ID, admin: admin, approver: approver, adminKey: adminKey, approverKy: approverKey}
}

func (e orgEnv) req(t *testing.T, key, method, path, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r, _ = http.NewRequest(method, e.srv.URL+path, nil)
	} else {
		r, _ = http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	return do(t, r)
}

func TestOrg_KeysAreScopedToTheirKind(t *testing.T) {
	e := setupOrg(t)
	if code, body := e.req(t, e.adminKey, http.MethodGet, "/v1/org", ""); code != http.StatusOK || body["name"] != "Lagoon Labs" {
		t.Fatalf("member key on /v1/org: %d %v", code, body)
	}
	if code, _ := e.req(t, e.key, http.MethodGet, "/v1/org", ""); code != http.StatusUnauthorized {
		t.Fatalf("contractor key on a member operation: %d, want 401", code)
	}
	if code, _ := e.req(t, e.adminKey, http.MethodGet, "/v1/me/bank-account", ""); code != http.StatusUnauthorized {
		t.Fatalf("member key on a contractor operation: %d, want 401", code)
	}
	if code, _ := e.req(t, "", http.MethodGet, "/v1/org/members", ""); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d, want 401", code)
	}
}

func TestOrg_OnlyAdminsManage(t *testing.T) {
	e := setupOrg(t)
	if code, _ := e.req(t, e.approverKy, http.MethodPatch, "/v1/org", `{"approvalThreshold":"1"}`); code != http.StatusForbidden {
		t.Fatalf("approver changing the threshold: %d, want 403", code)
	}
	if code, _ := e.req(t, e.approverKy, http.MethodPost, "/v1/org/members", `{"name":"Mallory","roles":["admin"]}`); code != http.StatusForbidden {
		t.Fatalf("approver adding a member: %d, want 403", code)
	}
	code, body := e.req(t, e.adminKey, http.MethodPatch, "/v1/org", `{"approvalThreshold":"250.5"}`)
	th, _ := body["approvalThreshold"].(map[string]any)
	if code != http.StatusOK || th["minor"] != "250500000" {
		t.Fatalf("admin changing the threshold: %d %v", code, body)
	}
}

func TestOrg_AddedMemberKeyWorksAndRemovalStopsIt(t *testing.T) {
	e := setupOrg(t)
	code, body := e.req(t, e.adminKey, http.MethodPost, "/v1/org/members", `{"name":"Ngozi Obi","roles":["maker","maker"]}`)
	if code != http.StatusCreated {
		t.Fatalf("add member: %d %v", code, body)
	}
	key, _ := body["apiKey"].(string)
	m, _ := body["member"].(map[string]any)
	if roles, _ := m["roles"].([]any); len(roles) != 1 || roles[0] != "maker" {
		t.Fatalf("roles %v, want [maker]", m["roles"])
	}
	if code, _ := e.req(t, key, http.MethodGet, "/v1/org", ""); code != http.StatusOK {
		t.Fatalf("new member's key: %d, want 200", code)
	}
	id, _ := m["id"].(string)
	if code, body := e.req(t, e.adminKey, http.MethodPatch, "/v1/org/members/"+id, `{"removed":true}`); code != http.StatusOK || body["removedAt"] == nil {
		t.Fatalf("remove: %d %v", code, body)
	}
	if code, _ := e.req(t, key, http.MethodGet, "/v1/org", ""); code != http.StatusUnauthorized {
		t.Fatalf("removed member's key: %d, want 401", code)
	}
}

func TestOrg_LastAdminStays(t *testing.T) {
	e := setupOrg(t)
	path := "/v1/org/members/" + e.admin.String()
	if code, _ := e.req(t, e.adminKey, http.MethodPatch, path, `{"roles":["maker"]}`); code != http.StatusConflict {
		t.Fatalf("demoting the last admin: %d, want 409", code)
	}
	if code, _ := e.req(t, e.adminKey, http.MethodPatch, path, `{"removed":true}`); code != http.StatusConflict {
		t.Fatalf("removing the last admin: %d, want 409", code)
	}
	// With a second admin, the first may step down.
	if code, _ := e.req(t, e.adminKey, http.MethodPatch, "/v1/org/members/"+e.approver.String(), `{"roles":["admin","approver"]}`); code != http.StatusOK {
		t.Fatalf("promoting a second admin: %d", code)
	}
	if code, _ := e.req(t, e.adminKey, http.MethodPatch, path, `{"roles":["maker"]}`); code != http.StatusOK {
		t.Fatalf("stepping down with another admin present: %d, want 200", code)
	}
	if code, _ := e.req(t, e.adminKey, http.MethodPatch, path, `{"roles":["admin"]}`); code != http.StatusForbidden {
		t.Fatalf("a former admin re-promoting themselves: %d, want 403", code)
	}
}

func TestOrg_FundingAddressIsStableAndBalancesStartAtZero(t *testing.T) {
	e := setupOrg(t)
	_, a := e.req(t, e.approverKy, http.MethodGet, "/v1/org/funding", "")
	_, b := e.req(t, e.adminKey, http.MethodGet, "/v1/org/funding", "")
	addrs, _ := a["depositAddresses"].([]any)
	if len(addrs) != 1 {
		t.Fatalf("funding: %v", a)
	}
	first, _ := addrs[0].(map[string]any)
	again, _ := b["depositAddresses"].([]any)[0].(map[string]any)
	if first["address"] != again["address"] || first["network"] != "evm:31337" {
		t.Fatalf("funding address changed: %v then %v", first, again)
	}
	if av, _ := a["available"].(map[string]any); av["minor"] != "0" || av["asset"] != "USDC" {
		t.Fatalf("available %v, want 0 USDC", a["available"])
	}
	// The address is the organisation's CREATE2 forwarder, distinct from
	// any invoice's.
	var owner uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT org_id FROM deposit_addresses WHERE address = lower($1)`, first["address"]).Scan(&owner); err != nil || owner != e.orgID {
		t.Fatalf("address owner %v, %v; want the organisation", owner, err)
	}
}
