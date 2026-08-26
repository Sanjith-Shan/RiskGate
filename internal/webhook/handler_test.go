package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSecret = "whsec_handler_test"

var disputeEvent = `{"id":"evt_d1","object":"event","type":"charge.dispute.created","created":1767225600,"api_version":"2026-01-01","data":{"object":{"id":"dp_1","object":"dispute","payment_intent":"pi_1","amount":4999,"currency":"usd","reason":"fraudulent","status":"needs_response"}}}`

type recorder struct {
	mu     sync.Mutex
	events []*Event
	err    error
}

func (r *recorder) onEvent(_ context.Context, e *Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func newTestHandler(t *testing.T, onEvent EventFunc) (*Handler, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	v := NewVerifier(testSecret)
	v.Now = clock.Now
	h := NewHandler(v, NewDeduper(DeduperConfig{Now: clock.Now}), onEvent)
	h.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return h, clock
}

func post(h http.Handler, body, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/clearinghouse", strings.NewReader(body))
	if header != "" {
		req.Header.Set(SignatureHeader, header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func status(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct{ Status string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response %q: %v", rec.Body.String(), err)
	}
	return body.Status
}

func TestHandlerProcessesThenAcknowledgesDuplicate(t *testing.T) {
	var rec recorder
	h, clock := newTestHandler(t, rec.onEvent)
	header := Sign([]byte(disputeEvent), clock.Now(), []byte(testSecret))

	res := post(h, disputeEvent, header)
	if res.Code != http.StatusOK || status(t, res) != "processed" {
		t.Fatalf("first delivery: %d %s", res.Code, res.Body)
	}
	// A retry arrives an hour later, freshly signed as Clearinghouse does.
	clock.Advance(time.Hour)
	res = post(h, disputeEvent, Sign([]byte(disputeEvent), clock.Now(), []byte(testSecret)))
	if res.Code != http.StatusOK || status(t, res) != "duplicate" {
		t.Fatalf("retry: %d %s", res.Code, res.Body)
	}
	if rec.count() != 1 {
		t.Fatalf("callback ran %d times, want 1", rec.count())
	}
	d, err := rec.events[0].Dispute()
	if err != nil || d.Reason != "fraudulent" || d.PaymentIntent != "pi_1" {
		t.Fatalf("dispute %+v, %v", d, err)
	}
}

func TestHandlerRejectsBadSignatures(t *testing.T) {
	var rec recorder
	h, clock := newTestHandler(t, rec.onEvent)
	now := clock.Now()
	for _, tc := range []struct {
		name, body, header, want string
	}{
		{"missing header", disputeEvent, "", "malformed_header"},
		{"wrong secret", disputeEvent, Sign([]byte(disputeEvent), now, []byte("nope")), "no_matching_signature"},
		{"tampered", strings.Replace(disputeEvent, "4999", "1", 1), Sign([]byte(disputeEvent), now, []byte(testSecret)), "no_matching_signature"},
		{"stale", disputeEvent, Sign([]byte(disputeEvent), now.Add(-time.Hour), []byte(testSecret)), "timestamp_outside_tolerance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := post(h, tc.body, tc.header)
			if res.Code != http.StatusBadRequest || status(t, res) != tc.want {
				t.Fatalf("got %d %s, want 400 %s", res.Code, res.Body, tc.want)
			}
		})
	}
	if rec.count() != 0 {
		t.Fatal("callback ran for a rejected request")
	}
}

func TestHandlerRejectsInvalidEvent(t *testing.T) {
	var rec recorder
	h, clock := newTestHandler(t, rec.onEvent)
	for _, body := range []string{
		`not json`,
		`{"id":"evt_1","object":"charge","type":"x","data":{"object":{}}}`,
		`{"object":"event","type":"x","data":{"object":{}}}`,
		`{"id":"evt_1","object":"event","data":{"object":{}}}`,
		`{"id":"evt_1","object":"event","type":"x","data":{"object":null}}`,
	} {
		res := post(h, body, Sign([]byte(body), clock.Now(), []byte(testSecret)))
		if res.Code != http.StatusBadRequest || status(t, res) != "invalid_event" {
			t.Errorf("%s: got %d %s", body, res.Code, res.Body)
		}
	}
}

func TestHandlerCallbackFailureIsRetried(t *testing.T) {
	rec := recorder{err: errors.New("label store down")}
	h, clock := newTestHandler(t, rec.onEvent)
	sign := func() string { return Sign([]byte(disputeEvent), clock.Now(), []byte(testSecret)) }

	if res := post(h, disputeEvent, sign()); res.Code != http.StatusInternalServerError {
		t.Fatalf("failing callback: got %d", res.Code)
	}
	rec.mu.Lock()
	rec.err = nil
	rec.mu.Unlock()
	if res := post(h, disputeEvent, sign()); res.Code != http.StatusOK || status(t, res) != "processed" {
		t.Fatalf("retry after failure: %d %s", res.Code, res.Body)
	}
}

func TestHandlerPanicReleasesClaim(t *testing.T) {
	calls := 0
	h, clock := newTestHandler(t, func(context.Context, *Event) error {
		calls++
		if calls == 1 {
			panic("boom")
		}
		return nil
	})
	header := Sign([]byte(disputeEvent), clock.Now(), []byte(testSecret))
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic to propagate to net/http")
			}
		}()
		post(h, disputeEvent, header)
	}()
	if res := post(h, disputeEvent, header); res.Code != http.StatusOK || status(t, res) != "processed" {
		t.Fatalf("retry after panic: %d %s", res.Code, res.Body)
	}
}

func TestHandlerConcurrentDuplicateIsNotAcknowledged(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	h, clock := newTestHandler(t, func(context.Context, *Event) error {
		close(entered)
		<-release
		return nil
	})
	header := Sign([]byte(disputeEvent), clock.Now(), []byte(testSecret))

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- post(h, disputeEvent, header) }()
	<-entered
	if res := post(h, disputeEvent, header); res.Code != http.StatusConflict {
		t.Fatalf("concurrent duplicate: got %d, want 409", res.Code)
	}
	close(release)
	if res := <-done; res.Code != http.StatusOK {
		t.Fatalf("first delivery: %d", res.Code)
	}
}

func TestHandlerLimits(t *testing.T) {
	var rec recorder
	h, clock := newTestHandler(t, rec.onEvent)
	h.MaxBodyBytes = 64

	big := strings.Repeat("x", 65)
	if res := post(h, big, Sign([]byte(big), clock.Now(), []byte(testSecret))); res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: got %d", res.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/clearinghouse", nil)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET: %d Allow=%q", res.Code, res.Header().Get("Allow"))
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestHandlerUnreadableBody(t *testing.T) {
	var rec recorder
	h, _ := newTestHandler(t, rec.onEvent)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/clearinghouse", errReader{})
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || status(t, res) != "unreadable_body" {
		t.Fatalf("got %d %s", res.Code, res.Body)
	}
}

func TestHandlerOverHTTP(t *testing.T) {
	var rec recorder
	h, clock := newTestHandler(t, rec.onEvent)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(disputeEvent))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(SignatureHeader, Sign([]byte(disputeEvent), clock.Now(), []byte(testSecret)))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || rec.count() != 1 {
		t.Fatalf("status %d, events %d", resp.StatusCode, rec.count())
	}
}
