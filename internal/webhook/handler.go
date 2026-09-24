package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// DefaultMaxBodyBytes bounds a webhook body. Clearinghouse events are a few
// KB; anything near this size is not one of them.
const DefaultMaxBodyBytes = 256 << 10

// EventFunc processes one verified, first-seen event. Returning an error
// makes the handler answer 500 so Clearinghouse retries the delivery, or 503
// if the error wraps ErrUnavailable.
type EventFunc func(ctx context.Context, e *Event) error

// ErrUnavailable is for an EventFunc that cannot take events at all right
// now, for example because the service is shutting down. The handler
// answers 503, and Clearinghouse retries the delivery.
var ErrUnavailable = errors.New("webhook: receiver unavailable")

// Handler is an http.Handler for POST /v1/webhooks/clearinghouse.
//
// Responses, chosen so Clearinghouse's retry logic does the right thing:
//
//	200 processed, or a duplicate of an already-processed event (stop retrying)
//	400 signature rejected or body is not an event (retrying will not help;
//	    Clearinghouse still retries non-2xx, which covers a rotation race)
//	405 not a POST
//	409 the same event is being processed by a concurrent delivery (retry later)
//	413 body over MaxBodyBytes
//	500 the callback failed (retry later)
//	503 the callback returned ErrUnavailable (retry later)
type Handler struct {
	Verifier *Verifier
	Deduper  *Deduper
	OnEvent  EventFunc
	// MaxBodyBytes defaults to DefaultMaxBodyBytes when zero.
	MaxBodyBytes int64
	// Logger defaults to slog.Default when nil.
	Logger *slog.Logger
}

// NewHandler returns a Handler with default limits.
func NewHandler(v *Verifier, d *Deduper, onEvent EventFunc) *Handler {
	return &Handler{Verifier: v, Deduper: d, OnEvent: onEvent}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	logger := h.Logger
	if logger == nil {
		logger = slog.Default()
	}
	limit := h.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}

	// The signature covers the exact bytes on the wire, so read them raw
	// before any decoding.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, "body_too_large")
			return
		}
		writeJSON(w, http.StatusBadRequest, "unreadable_body")
		return
	}

	if err := h.Verifier.Verify(r.Header.Get(SignatureHeader), body); err != nil {
		logger.Warn("webhook signature rejected", "error", err)
		writeJSON(w, http.StatusBadRequest, string(Code(err)))
		return
	}

	event, err := ParseEvent(body)
	if err != nil {
		logger.Warn("webhook body rejected", "error", err)
		writeJSON(w, http.StatusBadRequest, "invalid_event")
		return
	}

	switch h.Deduper.Begin(event.ID) {
	case StatusDuplicate:
		writeJSON(w, http.StatusOK, "duplicate")
		return
	case StatusInFlight:
		writeJSON(w, http.StatusConflict, "in_flight")
		return
	}

	// Release the claim on every path that does not commit, including a
	// panic in OnEvent; otherwise the id would stay in flight until its TTL
	// and every retry would get 409.
	committed := false
	defer func() {
		if !committed {
			h.Deduper.Release(event.ID)
		}
	}()
	if err := h.OnEvent(r.Context(), event); err != nil {
		if errors.Is(err, ErrUnavailable) {
			logger.Warn("webhook event refused", "event", event.ID, "type", event.Type, "error", err)
			writeJSON(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		logger.Error("webhook event failed", "event", event.ID, "type", event.Type, "error", err)
		writeJSON(w, http.StatusInternalServerError, "processing_failed")
		return
	}
	h.Deduper.Commit(event.ID)
	committed = true
	writeJSON(w, http.StatusOK, "processed")
}

// writeJSON writes {"status": status}. Error responses carry the machine
// code (for example "no_matching_signature") so the sender can log it.
func writeJSON(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}
