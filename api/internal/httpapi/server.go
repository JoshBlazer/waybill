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
	"github.com/JoshBlazer/waybill/api/internal/bankverify"
	"github.com/JoshBlazer/waybill/api/internal/batch"
	"github.com/JoshBlazer/waybill/api/internal/checkout"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/org"
	"github.com/JoshBlazer/waybill/api/internal/paystack"
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
	// WebhookAllowPrivate accepts http and private hosts for webhook
	// endpoints. Local development on Anvil only.
	WebhookAllowPrivate bool

	Banks    bankverify.BankLister // payout banks
	Resolver bankverify.Resolver   // bank account names (ADR-040)
	Paystack *paystack.Client      // webhook signatures; nil when no key is configured
	Checkout *checkout.Service     // card and bank pay-in; nil disables it
	Org      *org.Service          // payer organisations (stage 4)
	Batches  *batch.Service        // contractor wallets and batch runs (stage 4)
	Now      func() time.Time      // nil means time.Now
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
	"CreateInvoice":         true,
	"CreateApiKey":          true,
	"ListApiKeys":           true,
	"RevokeApiKey":          true,
	"CreateWebhookEndpoint": true,
	"ListWebhookEndpoints":  true,
	"DeleteWebhookEndpoint": true,
	"ListWebhookDeliveries": true,
	"ListBanks":             true,
	"GetBankAccount":        true,
	"SetBankAccount":        true,
	"GetInvoice":            true,
	"AcceptShortfall":       true,
	"ListPayoutWallets":     true,
	"SetPayoutWallet":       true,
}

// memberOperations lists the operations that need an organisation
// member's key (stage 4, ADR-043). Like authenticatedOperations, it is kept
// in step with the spec by TestAuthenticatedOperationsMatchSpec.
var memberOperations = map[string]bool{
	"GetOrg":       true,
	"UpdateOrg":    true,
	"ListMembers":  true,
	"AddMember":    true,
	"UpdateMember": true,
	"GetFunding":   true,
	"CreateBatch":  true,
	"ListBatches":  true,
	"GetBatch":     true,
	"ApproveBatch": true,
	"RejectBatch":  true,
	"SendBatch":    true,
	"CancelBatch":  true,
}

// IsAuthenticated reports whether an operation requires a key.
func IsAuthenticated(operationID string) bool {
	return authenticatedOperations[operationID] || memberOperations[operationID]
}

// AuthenticatedOperations lists the operations that require a key.
func AuthenticatedOperations() []string {
	out := make([]string, 0, len(authenticatedOperations)+len(memberOperations))
	for op := range authenticatedOperations {
		out = append(out, op)
	}
	for op := range memberOperations {
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
	return keepRawBody(HandlerWithOptions(strict, StdHTTPServerOptions{
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProblem(w, http.StatusBadRequest, "bad-request", "The request is missing or has an invalid parameter.", err.Error())
		},
	}))
}

// authMiddleware authenticates operations that require a key and puts its
// owner in the context. A contractor key cannot call a member operation,
// nor a member key a contractor operation.
func (s *Server) authMiddleware(f StrictHandlerFunc, operationID string) StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, req any) (any, error) {
		forContractor, forMember := authenticatedOperations[operationID], memberOperations[operationID]
		if !forContractor && !forMember {
			return f(ctx, w, r, req)
		}
		title := "A valid contractor API key is required."
		if forMember {
			title = "A valid organisation member API key is required."
		}
		p, err := auth.Authenticate(ctx, s.Pool, r.Header.Get("Authorization"))
		if err == nil && p.IsMember() != forMember {
			err = auth.ErrUnauthenticated
		}
		if errors.Is(err, auth.ErrUnauthenticated) {
			writeProblem(w, http.StatusUnauthorized, "unauthenticated", title, "")
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return f(auth.WithPrincipal(ctx, p), w, r, req)
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
