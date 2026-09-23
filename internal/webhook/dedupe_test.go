package webhook

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock, safe for concurrent reads.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1767225600, 0).UTC()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestDeduperLifecycle(t *testing.T) {
	d := NewDeduper(DeduperConfig{Now: newFakeClock().Now})
	if got := d.Begin("evt_1"); got != StatusNew {
		t.Fatalf("first Begin = %v", got)
	}
	if got := d.Begin("evt_1"); got != StatusInFlight {
		t.Fatalf("Begin while in flight = %v", got)
	}
	d.Commit("evt_1")
	if got := d.Begin("evt_1"); got != StatusDuplicate {
		t.Fatalf("Begin after Commit = %v", got)
	}
	// Release after Commit must not forget a processed event.
	d.Release("evt_1")
	if got := d.Begin("evt_1"); got != StatusDuplicate {
		t.Fatalf("Begin after stray Release = %v", got)
	}
}

func TestDeduperReleaseAllowsRetry(t *testing.T) {
	d := NewDeduper(DeduperConfig{})
	d.Begin("evt_1")
	d.Release("evt_1")
	if got := d.Begin("evt_1"); got != StatusNew {
		t.Fatalf("retry after a failed attempt = %v, want new", got)
	}
}

// TestDeduperCoversRetryWindow replays Clearinghouse's schedule: the first
// delivery is processed but its 200 is lost, and retries keep arriving for
// three simulated days. Every retry inside the TTL must be a duplicate.
func TestDeduperCoversRetryWindow(t *testing.T) {
	clock := newFakeClock()
	d := NewDeduper(DeduperConfig{Now: clock.Now})
	if d.Begin("evt_dispute") != StatusNew {
		t.Fatal("first delivery not new")
	}
	d.Commit("evt_dispute")
	elapsed := time.Duration(0)
	for backoff := time.Minute; elapsed+backoff < 72*time.Hour; backoff *= 2 {
		clock.Advance(backoff)
		elapsed += backoff
		if got := d.Begin("evt_dispute"); got != StatusDuplicate {
			t.Fatalf("retry at %v = %v, want duplicate", elapsed, got)
		}
	}
	clock.Advance(DefaultDedupeTTL)
	if got := d.Begin("evt_dispute"); got != StatusNew {
		t.Fatalf("after TTL = %v, want new (forgotten)", got)
	}
}

func TestDeduperTTLExpiry(t *testing.T) {
	clock := newFakeClock()
	d := NewDeduper(DeduperConfig{TTL: time.Hour, Now: clock.Now})
	for i := range 10 {
		d.Begin(fmt.Sprint(i))
		d.Commit(fmt.Sprint(i))
		clock.Advance(10 * time.Minute)
	}
	// Id i was committed 100-10i minutes ago; ids 0-4 are at least an hour old.
	d.Begin("trigger-expiry")
	if got := d.Len(); got != 6 {
		t.Fatalf("Len = %d, want 6 (ids 5-9 plus the trigger)", got)
	}
	if d.Begin("4") != StatusNew || d.Begin("5") != StatusDuplicate {
		t.Fatal("expiry boundary wrong")
	}
	if d.Evictions() != 0 {
		t.Fatalf("TTL expiry counted as eviction: %d", d.Evictions())
	}
}

func TestDeduperCapacityEvictsOldest(t *testing.T) {
	clock := newFakeClock()
	d := NewDeduper(DeduperConfig{MaxEntries: 3, Now: clock.Now})
	for _, id := range []string{"a", "b", "c", "d"} {
		d.Begin(id)
		d.Commit(id)
		clock.Advance(time.Second)
	}
	if d.Len() != 3 || d.Evictions() != 1 {
		t.Fatalf("Len=%d Evictions=%d", d.Len(), d.Evictions())
	}
	if d.Begin("a") != StatusNew {
		t.Fatal("oldest id should have been evicted")
	}
	if d.Begin("d") != StatusDuplicate {
		t.Fatal("newest id should be kept")
	}
}

func TestDeduperCommitAfterEviction(t *testing.T) {
	d := NewDeduper(DeduperConfig{MaxEntries: 1})
	d.Begin("slow")
	d.Begin("fast") // evicts the in-flight claim on "slow"
	d.Commit("slow")
	if got := d.Begin("slow"); got != StatusDuplicate {
		t.Fatalf("committed id forgotten: %v", got)
	}
}

func TestDeduperSnapshotRestore(t *testing.T) {
	clock := newFakeClock()
	d := NewDeduper(DeduperConfig{TTL: time.Hour, Now: clock.Now})
	d.Begin("old")
	d.Commit("old")
	clock.Advance(50 * time.Minute)
	d.Begin("recent")
	d.Commit("recent")
	d.Begin("in-flight")

	snap := d.Snapshot()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []SeenEvent
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 {
		t.Fatalf("snapshot has %d entries, want 2 (in-flight excluded): %s", len(decoded), raw)
	}

	// Restart 20 minutes later: "old" is now 70 minutes old and expired.
	clock.Advance(20 * time.Minute)
	restored := NewDeduper(DeduperConfig{TTL: time.Hour, Now: clock.Now})
	restored.Restore(decoded)
	if restored.Len() != 1 {
		t.Fatalf("restored Len = %d, want 1", restored.Len())
	}
	if restored.Begin("recent") != StatusDuplicate {
		t.Fatal("recent id lost across restart")
	}
	if restored.Begin("old") != StatusNew {
		t.Fatal("expired id survived restart")
	}
	if restored.Begin("in-flight") != StatusNew {
		t.Fatal("unprocessed in-flight id must be processed after restart")
	}
}

func TestDeduperRestoreMergesInTimeOrder(t *testing.T) {
	clock := newFakeClock()
	base := clock.Now()
	d := NewDeduper(DeduperConfig{TTL: time.Hour, Now: clock.Now})
	clock.Advance(30 * time.Minute)
	d.Begin("live")
	d.Commit("live")
	d.Restore([]SeenEvent{
		{ID: "newer", Seen: base.Add(20 * time.Minute)},
		{ID: "older", Seen: base.Add(5 * time.Minute)},
	})
	if got := d.Snapshot(); len(got) != 3 || got[0].ID != "older" || got[2].ID != "live" {
		t.Fatalf("snapshot order %+v", got)
	}
	// 55 more minutes: "older" (80m) and "newer" (65m) expire, "live" (55m) stays.
	clock.Advance(55 * time.Minute)
	if got := d.Snapshot(); len(got) != 1 || got[0].ID != "live" {
		t.Fatalf("after expiry %+v", got)
	}
}

// TestDeduperConcurrentDeliveries fires many concurrent deliveries of the
// same ids and checks each id is processed exactly once. Run with -race.
func TestDeduperConcurrentDeliveries(t *testing.T) {
	d := NewDeduper(DeduperConfig{})
	const ids, deliveries = 50, 20
	var processed [ids]atomic.Int32
	var wg sync.WaitGroup
	for i := range ids {
		for range deliveries {
			wg.Go(func() {
				id := fmt.Sprintf("evt_%d", i)
				if d.Begin(id) == StatusNew {
					processed[i].Add(1)
					d.Commit(id)
				}
			})
		}
	}
	wg.Wait()
	for i := range processed {
		if n := processed[i].Load(); n != 1 {
			t.Fatalf("evt_%d processed %d times", i, n)
		}
	}
}

func TestDedupeStatusString(t *testing.T) {
	for s, want := range map[DedupeStatus]string{
		StatusNew: "new", StatusDuplicate: "duplicate", StatusInFlight: "in_flight", 99: "unknown",
	} {
		if s.String() != want {
			t.Errorf("%d.String() = %q, want %q", int(s), s.String(), want)
		}
	}
}
