package loadgen

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer is a stand-in for /v1/assess. Handlers take a read lock, so
// stall (which takes the write lock) freezes every request, new and in
// flight, the way a stop-the-world pause or a lock convoy would.
type fakeServer struct {
	*httptest.Server
	gate     sync.RWMutex
	requests atomic.Int64
	ids      sync.Map
	dupIDs   atomic.Int64
	status   int
}

func newFakeServer(t *testing.T, status int) *fakeServer {
	t.Helper()
	fs := &fakeServer{status: status}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.gate.RLock()
		defer fs.gate.RUnlock()
		fs.requests.Add(1)
		var body struct {
			PaymentID string `json:"payment_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			if _, loaded := fs.ids.LoadOrStore(body.PaymentID, true); loaded {
				fs.dupIDs.Add(1)
			}
		}
		w.WriteHeader(fs.status)
		io.WriteString(w, `{"decision":"allow"}`)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fakeServer) stall(d time.Duration) {
	fs.gate.Lock()
	time.Sleep(d)
	fs.gate.Unlock()
}

func testRequests(t *testing.T) []Request {
	t.Helper()
	reqs, err := ReadRequests(strings.NewReader(
		`{"payment_id":"pay_a","created":1767225600,"amount":1999,"currency":"usd","risk_fields":{"card_network":"visa"}}` + "\n" +
			`{"payment_id":"pay_b","created":1767225601,"amount":250,"currency":"usd","risk_fields":{}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return reqs
}

func TestRateAccuracy(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	fs := newFakeServer(t, http.StatusOK)
	const rate = 2000
	res, err := Run(context.Background(), Config{
		URL: fs.URL, Rate: rate, Duration: 2 * time.Second, Warmup: 250 * time.Millisecond,
		Requests: testRequests(t), UniqueIDs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 2*rate {
		t.Fatalf("sent %d in the measured window, want exactly %d", res.Sent, 2*rate)
	}
	if res.OK != res.Sent || res.Errors+res.Timeouts+res.Non2xx != 0 {
		t.Fatalf("ok %d of %d, errors %d, timeouts %d, non-2xx %d", res.OK, res.Sent, res.Errors, res.Timeouts, res.Non2xx)
	}
	// Locally this is within 0.1%; the slack is for shared CI runners
	// under the race detector.
	if res.AchievedRate < 0.9*rate || res.AchievedRate > 1.1*rate {
		t.Fatalf("achieved %.1f rps, target %d", res.AchievedRate, rate)
	}
	if got := fs.requests.Load(); got != int64(2*rate+rate/4) {
		t.Fatalf("server saw %d requests, want %d including warmup", got, 2*rate+rate/4)
	}
	if n := fs.dupIDs.Load(); n != 0 {
		t.Fatalf("%d duplicate payment_ids with UniqueIDs", n)
	}
	t.Logf("%s", res.Report())
}

// TestCoordinatedOmission stalls the server for one second in the middle of
// a run. An open-loop generator keeps sending on schedule, so every request
// due during the stall, about rate*1s of them, must be recorded with a
// latency up to the full second. A closed-loop generator with the same
// concurrency would record one slow request per worker and hide the rest.
func TestCoordinatedOmission(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	fs := newFakeServer(t, http.StatusOK)
	const rate = 1000
	stall := time.Second
	go func() {
		time.Sleep(time.Second)
		fs.stall(stall)
	}()
	res, err := Run(context.Background(), Config{
		URL: fs.URL, Rate: rate, Duration: 3 * time.Second, Requests: testRequests(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK != 3*rate {
		t.Fatalf("ok %d, want %d", res.OK, 3*rate)
	}
	// Requests due during the stall wait for its remainder: latencies are
	// spread evenly from ~1s down to ~0, so about 0.9*rate exceed 100ms.
	// The upper bound is loose because after the stall the server must also
	// accept the burst of new connections opened during it, which adds a
	// real (and correctly recorded) recovery tail on a loaded CI machine.
	slow := res.Latency.TotalCount() - countBelow(res, 100*time.Millisecond)
	if lo, hi := int64(0.75*rate), int64(1.6*rate); slow < lo || slow > hi {
		t.Fatalf("%d requests over 100ms, want about %d (in [%d, %d])", slow, int64(0.9*rate), lo, hi)
	}
	r := res.Report()
	if r.LatencyMS.Max < 900 {
		t.Fatalf("max latency %.1fms, want about the 1000ms stall", r.LatencyMS.Max)
	}
	// A third of all requests hit the stall, so p90 must reflect it.
	if r.LatencyMS.P90 < 300 {
		t.Fatalf("p90 %.1fms hides the stall", r.LatencyMS.P90)
	}
	t.Logf("%s\n  requests over 100ms: %d", r, slow)
}

func countBelow(res *Result, d time.Duration) int64 {
	var n int64
	for _, b := range res.Latency.Distribution() {
		if b.To <= d.Microseconds() {
			n += b.Count
		}
	}
	return n
}

func TestOutcomesAreClassified(t *testing.T) {
	fs := newFakeServer(t, http.StatusServiceUnavailable)
	res, err := Run(context.Background(), Config{
		URL: fs.URL, Rate: 200, Duration: 100 * time.Millisecond, Requests: testRequests(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Non2xx != 20 || res.OK != 0 || res.Latency.TotalCount() != 20 {
		t.Fatalf("non-2xx %d ok %d recorded %d", res.Non2xx, res.OK, res.Latency.TotalCount())
	}

	// Refused connections are errors and are kept out of the histogram.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	res, err = Run(context.Background(), Config{
		URL: dead.URL, Rate: 200, Duration: 100 * time.Millisecond, Requests: testRequests(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != 20 || res.Latency.TotalCount() != 0 {
		t.Fatalf("errors %d recorded %d", res.Errors, res.Latency.TotalCount())
	}
}

func TestTimeoutsAreRecorded(t *testing.T) {
	fs := newFakeServer(t, http.StatusOK)
	go fs.stall(500 * time.Millisecond)
	time.Sleep(10 * time.Millisecond) // let the stall take the lock
	res, err := Run(context.Background(), Config{
		URL: fs.URL, Rate: 100, Duration: 100 * time.Millisecond, Timeout: 50 * time.Millisecond,
		Requests: testRequests(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Timeouts != 10 || res.Latency.TotalCount() != 10 {
		t.Fatalf("timeouts %d recorded %d", res.Timeouts, res.Latency.TotalCount())
	}
	if lowest := res.Latency.Min(); lowest < 50_000 {
		t.Fatalf("timed-out request recorded at %dµs, below the 50ms timeout", lowest)
	}
}

func TestInFlightCapDelaysButStillCharges(t *testing.T) {
	fs := newFakeServer(t, http.StatusOK)
	go fs.stall(300 * time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	res, err := Run(context.Background(), Config{
		URL: fs.URL, Rate: 200, Duration: 200 * time.Millisecond, MaxInFlight: 5,
		Requests: testRequests(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Sends past the cap wait for the stall to end, which SendLag exposes;
	// latency from the intended time still includes that wait.
	if lag := res.Report().SendLagMS.Max; lag < 50 {
		t.Fatalf("send lag max %.1fms, expected the cap to delay sends", lag)
	}
	if res.OK != 40 {
		t.Fatalf("ok %d", res.OK)
	}
}

func TestRunCancel(t *testing.T) {
	fs := newFakeServer(t, http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, err := Run(ctx, Config{URL: fs.URL, Rate: 100, Duration: time.Minute, Requests: testRequests(t)})
	if err == nil || res == nil || res.Sent == 0 || res.Sent > 20 {
		t.Fatalf("err %v, res %+v", err, res)
	}
}

func TestRunValidates(t *testing.T) {
	reqs := testRequests(t)
	for _, cfg := range []Config{
		{Rate: 0, Duration: time.Second, Requests: reqs},
		{Rate: 1, Duration: 0, Requests: reqs},
		{Rate: 1, Duration: time.Second},
	} {
		if _, err := Run(context.Background(), cfg); err == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
}
