package service

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// Idempotency.
//
// Clearinghouse sends Idempotency-Key: <payment_id>:<attempt> and retries a
// timed-out assess with the same key. Assessing twice would count the
// payment twice in every velocity window, so the second request must get the
// first one's answer without touching the feature state. The store keeps the
// exact response bytes per key.
//
// Concurrency: a retry can arrive while the original is still running (the
// caller timed out, the server did not). The first request claims the key;
// later ones wait for it to finish and then return its stored answer. If
// the first fails without an answer worth replaying (a 4xx or 5xx), the
// claim is abandoned and a waiter takes it over, so an error is never
// cached as if it were a decision.
//
// Memory is bounded by maxEntries (oldest evicted first) and entries expire
// after ttl. Forgetting a key can only cause a double count for a retry that
// arrives after it was forgotten; the defaults are far longer than
// Clearinghouse's retry window.
//
// A key reused with a different body gets errKeyReused, following Stripe's
// idempotency semantics: the key names one request, not one payment id.

const (
	DefaultIdempotencyTTL        = 24 * time.Hour
	DefaultIdempotencyMaxEntries = 1 << 20
)

var errKeyReused = errors.New("idempotency key reused with a different request body")

type idemEntry struct {
	key  string
	hash uint64 // of the request body
	at   time.Time
	body []byte // the stored response; valid once ready
	// done is closed when the entry becomes ready or is abandoned.
	done      chan struct{}
	ready     bool
	abandoned bool
}

type idemStore struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu    sync.Mutex
	byKey map[string]*list.Element // value *idemEntry
	order *list.List               // oldest first
}

func newIdemStore(ttl time.Duration, maxEntries int, now func() time.Time) *idemStore {
	if ttl <= 0 {
		ttl = DefaultIdempotencyTTL
	}
	if maxEntries <= 0 {
		maxEntries = DefaultIdempotencyMaxEntries
	}
	return &idemStore{ttl: ttl, maxEntries: maxEntries, now: now, byKey: map[string]*list.Element{}, order: list.New()}
}

// claim returns either a stored response (body != nil) or an entry the
// caller now owns and must finish or abandon. It waits, bounded by ctx, for
// a concurrent request with the same key.
func (s *idemStore) claim(ctx context.Context, key string, hash uint64) (body []byte, owned *idemEntry, err error) {
	for {
		s.mu.Lock()
		now := s.now()
		s.expire(now)
		if el, ok := s.byKey[key]; ok {
			e := el.Value.(*idemEntry)
			if e.ready {
				s.mu.Unlock()
				if e.hash != hash {
					return nil, nil, errKeyReused
				}
				return e.body, nil, nil
			}
			done := e.done
			s.mu.Unlock()
			select {
			case <-done:
				continue // ready or abandoned: look again
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		e := &idemEntry{key: key, hash: hash, at: now, done: make(chan struct{})}
		s.insert(e)
		s.mu.Unlock()
		return nil, e, nil
	}
}

// finish stores the owner's response. body is retained, so the caller must
// not reuse it.
func (s *idemStore) finish(e *idemEntry, body []byte) {
	s.mu.Lock()
	e.body, e.ready = body, true
	s.mu.Unlock()
	close(e.done)
}

// abandon releases a claim without storing anything.
func (s *idemStore) abandon(e *idemEntry) {
	s.mu.Lock()
	if el, ok := s.byKey[e.key]; ok && el.Value.(*idemEntry) == e {
		s.remove(el)
	}
	e.abandoned = true
	s.mu.Unlock()
	close(e.done)
}

func (s *idemStore) insert(e *idemEntry) {
	s.byKey[e.key] = s.order.PushBack(e)
	for s.order.Len() > s.maxEntries {
		s.remove(s.order.Front())
	}
}

func (s *idemStore) remove(el *list.Element) {
	delete(s.byKey, el.Value.(*idemEntry).key)
	s.order.Remove(el)
}

// expire drops ready entries older than the TTL from the front. An entry
// still in flight is never expired, however old, since its owner will
// finish it.
func (s *idemStore) expire(now time.Time) {
	for el := s.order.Front(); el != nil; {
		e := el.Value.(*idemEntry)
		if !e.ready || now.Sub(e.at) < s.ttl {
			return
		}
		next := el.Next()
		s.remove(el)
		el = next
	}
}

func (s *idemStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}

// idemRecord is one stored response in a snapshot.
type idemRecord struct {
	Key  string    `json:"key"`
	Hash uint64    `json:"hash"`
	At   time.Time `json:"at"`
	Body []byte    `json:"body"`
}

// snapshot returns the finished entries, oldest first. In-flight claims are
// left out: they have no answer yet, and a retry after a restart must be
// assessed.
func (s *idemStore) snapshot() []idemRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(s.now())
	out := make([]idemRecord, 0, s.order.Len())
	for el := s.order.Front(); el != nil; el = el.Next() {
		if e := el.Value.(*idemEntry); e.ready {
			out = append(out, idemRecord{Key: e.key, Hash: e.hash, At: e.at, Body: e.body})
		}
	}
	return out
}

// restore replaces the contents with a snapshot's entries.
func (s *idemStore) restore(recs []idemRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byKey = make(map[string]*list.Element, len(recs))
	s.order.Init()
	now := s.now()
	for _, r := range recs {
		if now.Sub(r.At) >= s.ttl {
			continue
		}
		done := make(chan struct{})
		close(done)
		s.insert(&idemEntry{key: r.Key, hash: r.Hash, at: r.At, body: r.Body, done: done, ready: true})
	}
}
