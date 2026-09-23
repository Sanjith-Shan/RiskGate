// Command loadgen drives POST /v1/assess at fixed arrival rates and reports
// latency measured from each request's intended send time, so coordinated
// omission cannot hide a stall. See docs/LOADGEN.md.
//
// Usage:
//
//	loadgen -input requests.jsonl -rate 2000 -duration 30s
//	loadgen -input requests.jsonl -rates 1000:20000:1000 -deadline 50ms -json > sweep.json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/loadgen"
)

type options struct {
	url         string
	input       string
	rate        float64
	rates       string
	duration    time.Duration
	warmup      time.Duration
	cooldown    time.Duration
	timeout     time.Duration
	maxInFlight int
	uniqueIDs   bool
	deadline    time.Duration
	jsonOut     bool
}

// output is the -json document. Experiment scripts depend on these names.
type output struct {
	Machine loadgen.Machine  `json:"machine"`
	Config  outputConfig     `json:"config"`
	Runs    []loadgen.Report `json:"runs"`
	// MaxRateWithinDeadline is the highest target rate whose p99 latency
	// stayed inside the deadline with no errors or timeouts. Present only
	// with -deadline.
	MaxRateWithinDeadline *float64 `json:"max_rate_within_deadline_rps,omitempty"`
}

type outputConfig struct {
	URL         string  `json:"url"`
	Input       string  `json:"input"`
	DurationSec float64 `json:"duration_s"`
	WarmupSec   float64 `json:"warmup_s"`
	TimeoutMS   float64 `json:"timeout_ms"`
	MaxInFlight int     `json:"max_inflight"`
	UniqueIDs   bool    `json:"unique_ids"`
	DeadlineMS  float64 `json:"deadline_ms,omitempty"`
	StartedAt   string  `json:"started_at"`
}

func main() {
	var o options
	flag.StringVar(&o.url, "url", "http://127.0.0.1:8080/v1/assess", "assess endpoint")
	flag.StringVar(&o.input, "input", "", "JSONL file of assess requests (required)")
	flag.Float64Var(&o.rate, "rate", 1000, "arrival rate, requests per second")
	flag.StringVar(&o.rates, "rates", "", "sweep: comma-separated rates or start:end:step (overrides -rate)")
	flag.DurationVar(&o.duration, "duration", 30*time.Second, "measured duration per rate")
	flag.DurationVar(&o.warmup, "warmup", 5*time.Second, "unrecorded warmup per rate, at that rate")
	flag.DurationVar(&o.cooldown, "cooldown", 2*time.Second, "pause between sweep steps")
	flag.DurationVar(&o.timeout, "timeout", 5*time.Second, "per-request timeout, from actual send")
	flag.IntVar(&o.maxInFlight, "max-inflight", 10000, "cap on concurrent requests")
	flag.BoolVar(&o.uniqueIDs, "unique-ids", true, "rewrite payment_id to be unique per send")
	flag.DurationVar(&o.deadline, "deadline", 0, "latency budget; reports the highest rate with p99 inside it")
	flag.BoolVar(&o.jsonOut, "json", false, "write one JSON document to stdout")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, o, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options, stdout, stderr io.Writer) error {
	if o.input == "" {
		return errors.New("-input is required")
	}
	rates := []float64{o.rate}
	if o.rates != "" {
		var err error
		if rates, err = parseRates(o.rates); err != nil {
			return err
		}
	}
	f, err := os.Open(o.input)
	if err != nil {
		return err
	}
	reqs, err := loadgen.ReadRequests(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", o.input, err)
	}

	// Progress goes to stderr in JSON mode so stdout stays one document.
	progress := stdout
	if o.jsonOut {
		progress = stderr
	}
	out := output{
		Machine: loadgen.DetectMachine(),
		Config: outputConfig{
			URL: o.url, Input: o.input,
			DurationSec: o.duration.Seconds(), WarmupSec: o.warmup.Seconds(),
			TimeoutMS: ms(o.timeout), MaxInFlight: o.maxInFlight, UniqueIDs: o.uniqueIDs,
			DeadlineMS: ms(o.deadline), StartedAt: time.Now().UTC().Format(time.RFC3339),
		},
	}
	fmt.Fprintf(progress, "machine: %s\nloaded %d requests from %s\n", out.Machine, len(reqs), o.input)

	// One client for the whole sweep so warm connections carry over.
	client := loadgen.NewClient(o.maxInFlight)
	for i, rate := range rates {
		if i > 0 && o.cooldown > 0 {
			select {
			case <-time.After(o.cooldown):
			case <-ctx.Done():
			}
		}
		if ctx.Err() != nil {
			break
		}
		res, err := loadgen.Run(ctx, loadgen.Config{
			URL: o.url, Rate: rate, Duration: o.duration, Warmup: o.warmup, Timeout: o.timeout,
			MaxInFlight: o.maxInFlight, Requests: reqs, UniqueIDs: o.uniqueIDs, Client: client,
		})
		if res == nil {
			return err
		}
		report := res.Report()
		out.Runs = append(out.Runs, report)
		fmt.Fprintln(progress, report)
		if o.deadline > 0 {
			verdict := "inside"
			if !withinDeadline(report, o.deadline) {
				verdict = "OUTSIDE"
			}
			fmt.Fprintf(progress, "  p99 %s the %s deadline\n", verdict, o.deadline)
		}
		if err != nil {
			fmt.Fprintln(progress, "interrupted:", err)
			break
		}
	}

	if o.deadline > 0 {
		for _, r := range out.Runs {
			if withinDeadline(r, o.deadline) && (out.MaxRateWithinDeadline == nil || r.TargetRate > *out.MaxRateWithinDeadline) {
				out.MaxRateWithinDeadline = &r.TargetRate
			}
		}
		if out.MaxRateWithinDeadline != nil {
			fmt.Fprintf(progress, "highest rate with p99 inside %s: %.0f rps\n", o.deadline, *out.MaxRateWithinDeadline)
		} else {
			fmt.Fprintf(progress, "no rate kept p99 inside %s\n", o.deadline)
		}
	}
	if o.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	return nil
}

// withinDeadline requires a clean run as well as the p99: a rate that sheds
// load through errors or timeouts has not been served inside the deadline.
func withinDeadline(r loadgen.Report, deadline time.Duration) bool {
	return r.Errors == 0 && r.Timeouts == 0 && r.Non2xx == 0 && r.LatencyMS.P99 <= ms(deadline)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// parseRates accepts "1000,2000,5000" or "start:end:step" (inclusive).
func parseRates(s string) ([]float64, error) {
	if parts := strings.Split(s, ":"); len(parts) == 3 {
		var v [3]float64
		for i, p := range parts {
			f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
			if err != nil || f <= 0 {
				return nil, fmt.Errorf("-rates %q: %q is not a positive number", s, p)
			}
			v[i] = f
		}
		start, end, step := v[0], v[1], v[2]
		if end < start {
			return nil, fmt.Errorf("-rates %q: end is below start", s)
		}
		var rates []float64
		// Step by index to avoid float accumulation; allow a hair of slack
		// so 0.1-style steps still include the end point.
		for i := 0; start+float64(i)*step <= end*(1+1e-9); i++ {
			rates = append(rates, start+float64(i)*step)
		}
		return rates, nil
	}
	var rates []float64
	for p := range strings.SplitSeq(s, ",") {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("-rates %q: %q is not a positive number", s, p)
		}
		rates = append(rates, f)
	}
	return rates, nil
}
