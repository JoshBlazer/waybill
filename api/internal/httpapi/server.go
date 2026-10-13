package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// Pinger checks that the database answers queries.
type Pinger interface {
	Ping(ctx context.Context) (int32, error)
}

// Server implements StrictServerInterface.
type Server struct {
	DB       Pinger        // health checks
	Pool     *pgxpool.Pool // transactions
	Invoices *invoice.Service
	Tokens   map[safety.Network]string // token contract per network, lower-case hex
	WebURL   string                    // base URL of the web app, for payUrl and trackUrl
	Broker   *Broker
	Version  string
	Networks []string
	Log      *slog.Logger
}

var _ StrictServerInterface = (*Server)(nil)

// healthDBTimeout keeps a hung database from hanging the health endpoint.
const healthDBTimeout = 2 * time.Second

// GetHealth reports service health. It returns 503 with the same body when
// the database cannot answer.
func (s *Server) GetHealth(ctx context.Context, _ GetHealthRequestObject) (GetHealthResponseObject, error) {
	h := Health{
		Status:   HealthStatusOk,
		Database: HealthDatabaseOk,
		Version:  s.Version,
		Networks: s.Networks,
		TestMode: true,
	}
	if h.Networks == nil {
		h.Networks = []string{}
	}
	pctx, cancel := context.WithTimeout(ctx, healthDBTimeout)
	defer cancel()
	if _, err := s.DB.Ping(pctx); err != nil {
		s.logger().WarnContext(ctx, "health: database unavailable", "err", err)
		h.Status = HealthStatusDegraded
		h.Database = HealthDatabaseUnavailable
		return GetHealth503JSONResponse(h), nil
	}
	return GetHealth200JSONResponse(h), nil
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// authenticatedOperations lists the operations that need a contractor key.
// TestAuthenticatedOperationsMatchSpec keeps it in step with the `security`
// entries in openapi/waybill.yaml.
var authenticatedOperations = map[string]bool{
	"CreateInvoice": true,
}

// IsAuthenticated reports whether an operation requires a contractor key.
func IsAuthenticated(operationID string) bool { return authenticatedOperations[operationID] }

// AuthenticatedOperations lists the operations that require a contractor key.
func AuthenticatedOperations() []string {
	out := make([]string, 0, len(authenticatedOperations))
	for op := range authenticatedOperations {
		out = append(out, op)
	}
	return out
}

// NewHandler returns the HTTP handler for every route in the contract.
func NewHandler(s *Server) http.Handler {
	strict := NewStrictHandlerWithOptions(s, []StrictMiddlewareFunc{s.authMiddleware}, StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProblem(w, http.StatusBadRequest, "bad-request", "The request body is not valid JSON for this endpoint.", err.Error())
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			s.logger().ErrorContext(r.Context(), "handler error", "err", err, "path", r.URL.Path)
			writeProblem(w, http.StatusInternalServerError, "internal", "Something went wrong on our side.", "")
		},
	})
	return HandlerWithOptions(strict, StdHTTPServerOptions{
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProblem(w, http.StatusBadRequest, "bad-request", "The request is missing or has an invalid parameter.", err.Error())
		},
	})
}

// authMiddleware authenticates operations that require a contractor key and
// puts the contractor id in the context.
func (s *Server) authMiddleware(f StrictHandlerFunc, operationID string) StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, req any) (any, error) {
		if !authenticatedOperations[operationID] {
			return f(ctx, w, r, req)
		}
		id, err := auth.Authenticate(ctx, s.Pool, r.Header.Get("Authorization"))
		if errors.Is(err, auth.ErrUnauthenticated) {
			writeProblem(w, http.StatusUnauthorized, "unauthenticated", "A valid contractor API key is required.", "")
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return f(auth.WithContractor(ctx, id), w, r, req)
	}
}

const problemBase = "https://waybill.dev/problems/"

func problem(status int, slug, title, detail string) Problem {
	p := Problem{Type: problemBase + slug, Title: title, Status: status}
	if detail != "" {
		p.Detail = &detail
	}
	return p
}

func writeProblem(w http.ResponseWriter, status int, slug, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem(status, slug, title, detail))
}
