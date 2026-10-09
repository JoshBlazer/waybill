package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Pinger checks that the database answers queries.
type Pinger interface {
	Ping(ctx context.Context) (int32, error)
}

// Server implements StrictServerInterface.
type Server struct {
	DB       Pinger
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
		if s.Log != nil {
			s.Log.WarnContext(ctx, "health: database unavailable", "err", err)
		}
		h.Status = HealthStatusDegraded
		h.Database = HealthDatabaseUnavailable
		return GetHealth503JSONResponse(h), nil
	}
	return GetHealth200JSONResponse(h), nil
}

// NewHandler returns the HTTP handler for every route in the contract.
func NewHandler(s *Server) http.Handler {
	return HandlerWithOptions(NewStrictHandler(s, nil), StdHTTPServerOptions{})
}
