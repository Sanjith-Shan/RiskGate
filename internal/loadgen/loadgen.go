// Package loadgen is an open-loop HTTP load generator for RiskGate's
// POST /v1/assess.
//
// # Open loop, and coordinated omission
//
// A closed-loop generator (N workers, each sending its next request when
// the previous one returns) slows down whenever the server does. If the
// server stalls for one second, each worker records one slow request and
// then simply does not send the requests it would have sent during the
// stall. The histogram omits exactly the samples that would have shown the
// stall, and p99 looks fine. Gil Tene named this coordinated omission.
//
// This generator fixes a timetable up front: request i is due at
// start + i/rate, whatever the server is doing. Latency is measured from
// that intended send time, not from when the request actually left, so
// any delay the request spent waiting to be sent (a stalled server holding
// every connection, a full in-flight cap, a late timer) is charged to it,
// as a real client arriving at that moment would have experienced.
//
// # Keeping the generator off the critical path
//
// Each request runs on its own goroutine, started by a single scheduler
// goroutine that sleeps until the next due time and then dispatches every
// request that is due. A semaphore caps requests in flight (MaxInFlight)
// so a dead server cannot exhaust memory or file descriptors. If the cap is
// reached, the scheduler waits, the affected requests go out late, and
// their latency still counts from their intended time; SendLag reports how
// late sends were so a run limited by the generator is visible, not
// silently optimistic.
package loadgen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
)

// Histogram bounds, in microseconds: 1µs to 5 minutes at 3 significant
// digits (0.1% relative error).
const (
	histMinMicros = 1
	histMaxMicros = int64(5 * time.Minute / time.Microsecond)
	histSigFigs   = 3
)

// Config describes one fixed-rate run.
type Config struct {
	URL string
	// Rate is the arrival rate in requests per second.
	Rate float64
	// Duration is the measured part of the run.
	Duration time.Duration
	// Warmup is sent at Rate before Duration begins and is not recorded.
	Warmup time.Duration
	// Timeout bounds each request from when it is actually sent.
	Timeout time.Duration
	// MaxInFlight caps concurrent requests. Default 10000.
	MaxInFlight int
	// Requests are sent round-robin.
	Requests []Request
	// UniqueIDs rewrites payment_id on every send; see Request.Body.
	UniqueIDs bool
	// Client defaults to NewClient(MaxInFlight).
	Client *http.Client
}

// Result is the outcome of one run. Latency and SendLag are in microseconds.
type Result struct {
	TargetRate float64
	Duration   time.Duration
	// Sent counts requests dispatched in the measured window.
	Sent int64
	// AchievedRate is the measured window's send rate, from the first to the
	// last actual send.
	AchievedRate float64
	OK           int64 // 2xx
	Non2xx       int64
	Errors       int64 // transport errors other than timeouts
	Timeouts     int64
	// Latency holds 2xx, non-2xx and timed-out requests, measured from the
	// intended send time. Timeouts are recorded at the time they gave up,
	// a lower bound, so the tail is never hidden by dropping them.
	// Transport errors such as a refused connection are counted but not
	// recorded, because their fast failure would flatter the distribution.
	Latency *hdrhistogram.Histogram
	// SendLag is actual send time minus intended send time.
	SendLag *hdrhistogram.Histogram
}

// NewClient returns an HTTP client tuned for load generation. The key
// setting is MaxIdleConnsPerHost: net/http's default of 2 means that at
// thousands of requests per second almost every request opens a new
// connection and closes it, which measures the kernel's TCP handshake and
// TIME_WAIT handling rather than the server.
func NewClient(maxConns int) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        maxConns,
		MaxIdleConnsPerHost: maxConns,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
		// Assess bodies are small; HTTP/1.1 keep-alive connections keep
		// requests independent (no shared-stream head-of-line blocking).
		ForceAttemptHTTP2: false,
	}}
}

// recorder aggregates per-request outcomes. HdrHistogram is not safe for
// concurrent use, and a mutex held for one Record call is far cheaper than
// an HTTP round trip.
type recorder struct {
	mu        sync.Mutex
	res       *Result
	firstSend time.Time
	lastSend  time.Time
}

type outcome int

const (
	outcomeOK outcome = iota
	outcomeNon2xx
	outcomeTimeout
	outcomeError
)

func (r *recorder) record(intended, sent time.Time, elapsed time.Duration, o outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := r.res
	res.Sent++
	if r.firstSend.IsZero() || sent.Before(r.firstSend) {
		r.firstSend = sent
	}
	if sent.After(r.lastSend) {
		r.lastSend = sent
	}
	_ = res.SendLag.RecordValue(clampMicros(sent.Sub(intended)))
	switch o {
	case outcomeOK:
		res.OK++
	case outcomeNon2xx:
		res.Non2xx++
	case outcomeTimeout:
		res.Timeouts++
	case outcomeError:
		res.Errors++
		return
	}
	_ = res.Latency.RecordValue(clampMicros(elapsed))
}

func clampMicros(d time.Duration) int64 {
	return min(max(d.Microseconds(), histMinMicros), histMaxMicros)
}

// Run sends requests at cfg.Rate for cfg.Warmup + cfg.Duration and waits
// for the last one to finish. Cancelling ctx stops scheduling and returns
// the partial result with ctx's error.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	switch {
	case cfg.Rate <= 0:
		return nil, errors.New("loadgen: rate must be positive")
	case cfg.Duration <= 0:
		return nil, errors.New("loadgen: duration must be positive")
	case len(cfg.Requests) == 0:
		return nil, errors.New("loadgen: no requests")
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 10000
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	client := cfg.Client
	if client == nil {
		client = NewClient(cfg.MaxInFlight)
	}

	rec := &recorder{res: &Result{
		TargetRate: cfg.Rate,
		Duration:   cfg.Duration,
		Latency:    hdrhistogram.New(histMinMicros, histMaxMicros, histSigFigs),
		SendLag:    hdrhistogram.New(histMinMicros, histMaxMicros, histSigFigs),
	}}

	warmupN := int64(cfg.Warmup.Seconds() * cfg.Rate)
	totalN := warmupN + int64(cfg.Duration.Seconds()*cfg.Rate)
	// Computing each due time from i, rather than adding an interval to the
	// previous one, keeps rounding error from accumulating over a long run.
	dueAt := func(start time.Time, i int64) time.Time {
		return start.Add(time.Duration(float64(i) * float64(time.Second) / cfg.Rate))
	}

	sem := make(chan struct{}, cfg.MaxInFlight)
	var wg sync.WaitGroup
	timer := time.NewTimer(0)
	defer timer.Stop()
	start := time.Now()

	var runErr error
schedule:
	for i := int64(0); i < totalN; i++ {
		intended := dueAt(start, i)
		// Sleep until the next due time. When the timer fires late, the
		// loop dispatches every overdue request back to back, so the
		// average rate holds even with coarse OS timer resolution.
		if wait := time.Until(intended); wait > 0 {
			timer.Reset(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				runErr = ctx.Err()
				break schedule
			}
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			runErr = ctx.Err()
			break schedule
		}
		measured := i >= warmupN
		wg.Go(func() {
			defer func() { <-sem }()
			body := cfg.Requests[i%int64(len(cfg.Requests))].Body(i, cfg.UniqueIDs)
			sent := time.Now()
			o := send(ctx, client, cfg.URL, body, cfg.Timeout)
			if measured {
				rec.record(intended, sent, time.Since(intended), o)
			}
		})
	}
	wg.Wait()

	res := rec.res
	if span := rec.lastSend.Sub(rec.firstSend); res.Sent > 1 && span > 0 {
		res.AchievedRate = float64(res.Sent-1) / span.Seconds()
	}
	return res, runErr
}

func send(ctx context.Context, client *http.Client, url string, body []byte, timeout time.Duration) outcome {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return outcomeError
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return outcomeTimeout
		}
		return outcomeError
	}
	// Drain the body so the connection returns to the idle pool.
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return outcomeTimeout
	case err != nil:
		return outcomeError
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return outcomeNon2xx
	}
	return outcomeOK
}

// Summary is a histogram reduced to the percentiles the experiments report,
// in milliseconds.
type Summary struct {
	Count int64   `json:"count"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P99   float64 `json:"p99"`
	P999  float64 `json:"p99_9"`
	Max   float64 `json:"max"`
}

// Summarize reduces a microsecond histogram to a Summary.
func Summarize(h *hdrhistogram.Histogram) Summary {
	ms := func(us int64) float64 { return float64(us) / 1000 }
	return Summary{
		Count: h.TotalCount(),
		Mean:  h.Mean() / 1000,
		P50:   ms(h.ValueAtQuantile(50)),
		P90:   ms(h.ValueAtQuantile(90)),
		P99:   ms(h.ValueAtQuantile(99)),
		P999:  ms(h.ValueAtQuantile(99.9)),
		Max:   ms(h.Max()),
	}
}

// Report is the JSON form of a Result.
type Report struct {
	TargetRate   float64 `json:"target_rps"`
	AchievedRate float64 `json:"achieved_rps"`
	DurationSec  float64 `json:"duration_s"`
	Sent         int64   `json:"sent"`
	OK           int64   `json:"ok"`
	Non2xx       int64   `json:"non_2xx"`
	Errors       int64   `json:"errors"`
	Timeouts     int64   `json:"timeouts"`
	// LatencyMS is measured from the intended send time.
	LatencyMS Summary `json:"latency_ms"`
	SendLagMS Summary `json:"send_lag_ms"`
}

// Report reduces r for output.
func (r *Result) Report() Report {
	return Report{
		TargetRate:   r.TargetRate,
		AchievedRate: r.AchievedRate,
		DurationSec:  r.Duration.Seconds(),
		Sent:         r.Sent,
		OK:           r.OK,
		Non2xx:       r.Non2xx,
		Errors:       r.Errors,
		Timeouts:     r.Timeouts,
		LatencyMS:    Summarize(r.Latency),
		SendLagMS:    Summarize(r.SendLag),
	}
}

// String formats a Report for a terminal.
func (r Report) String() string {
	l, s := r.LatencyMS, r.SendLagMS
	return fmt.Sprintf(
		"target %.0f rps for %.0fs: sent %d, achieved %.1f rps; ok %d, non-2xx %d, errors %d, timeouts %d\n"+
			"  latency ms (from intended send): p50 %.3f  p90 %.3f  p99 %.3f  p99.9 %.3f  max %.3f\n"+
			"  send lag ms:                     p50 %.3f  p99 %.3f  max %.3f",
		r.TargetRate, r.DurationSec, r.Sent, r.AchievedRate, r.OK, r.Non2xx, r.Errors, r.Timeouts,
		l.P50, l.P90, l.P99, l.P999, l.Max,
		s.P50, s.P99, s.Max)
}
