package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) (int32, error) { return 1, f.err }

func getHealth(t *testing.T, s *Server) (int, Health) {
	t.Helper()
	rec := httptest.NewRecorder()
	NewHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	var h Health
	if err := json.NewDecoder(rec.Body).Decode(&h); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return rec.Code, h
}

func TestGetHealth_OK(t *testing.T) {
	code, h := getHealth(t, &Server{DB: fakeDB{}, Version: "test", Networks: []string{"evm:31337"}})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if h.Status != HealthStatusOk || h.Database != HealthDatabaseOk || !h.TestMode {
		t.Fatalf("body = %+v", h)
	}
	if len(h.Networks) != 1 || h.Networks[0] != "evm:31337" {
		t.Fatalf("networks = %v", h.Networks)
	}
}

func TestGetHealth_DatabaseDown(t *testing.T) {
	code, h := getHealth(t, &Server{DB: fakeDB{err: errors.New("down")}, Version: "test"})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if h.Status != HealthStatusDegraded || h.Database != HealthDatabaseUnavailable {
		t.Fatalf("body = %+v", h)
	}
	if h.Networks == nil {
		t.Fatal("networks must be [] not null, per the contract")
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler(&Server{DB: fakeDB{}}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
