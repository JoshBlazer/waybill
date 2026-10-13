package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/invoice"
)

// sseHeartbeat keeps proxies and mobile networks from closing idle streams.
const sseHeartbeat = 15 * time.Second

// StreamTracking streams the tracking view as Server-Sent Events. Each event
// carries the complete current view, so a client that missed events only
// needs the latest one: reconnecting with Last-Event-ID simply receives the
// current state.
func (s *Server) StreamTracking(ctx context.Context, req StreamTrackingRequestObject) (StreamTrackingResponseObject, error) {
	inv, err := invoice.Load(ctx, s.Pool, s.Tokens, req.Code)
	if errors.Is(err, invoice.ErrNotFound) {
		return StreamTracking404ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(notFound())}, nil
	}
	if err != nil {
		return nil, err
	}
	return sseStream{ctx: ctx, s: s, code: req.Code, first: inv}, nil
}

// sseStream implements StreamTrackingResponseObject itself, because the
// generated 200 response copies a reader without flushing per event.
type sseStream struct {
	ctx   context.Context
	s     *Server
	code  string
	first invoice.Invoice
}

func (st sseStream) VisitStreamTrackingResponse(w http.ResponseWriter) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("sse: response writer cannot flush")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	w.WriteHeader(http.StatusOK)

	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return nil // client went away
	}
	updates, unsubscribe := st.s.Broker.Subscribe(st.first.ID)
	defer unsubscribe()

	lastSent := int64(-1)
	send := func(inv invoice.Invoice) error {
		if inv.LatestEventID == lastSent {
			return nil
		}
		data, err := json.Marshal(trackingJSON(inv))
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: tracking\ndata: %s\n\n", inv.LatestEventID, data); err != nil {
			return err
		}
		flusher.Flush()
		lastSent = inv.LatestEventID
		return nil
	}
	if err := send(st.first); err != nil {
		return nil
	}

	ping := time.NewTicker(sseHeartbeat)
	defer ping.Stop()
	for {
		select {
		case <-st.ctx.Done():
			return nil
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
		case <-updates:
			inv, err := invoice.Load(st.ctx, st.s.Pool, st.s.Tokens, st.code)
			if err != nil {
				if st.ctx.Err() != nil {
					return nil
				}
				st.s.logger().Warn("sse: reload failed", "err", err)
				continue // keep the stream; the next signal or reconnect catches up
			}
			if err := send(inv); err != nil {
				return nil
			}
		}
	}
}
