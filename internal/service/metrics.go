package service

import (
	"bufio"
	"cmp"
	"fmt"
	"io"
	"slices"
	"strconv"
	"sync/atomic"
	"time"
)

// Metrics are written in the Prometheus text exposition format by hand. The
// set is small and fixed, so a client library would add a dependency and an
// abstraction for about a hundred lines of formatting, and the hot path
// stays a handful of atomic adds with no map lookups or label hashing.

// routes are the label values of riskgate_http_requests_total. Each has a
// fixed array of counters, one per status code the service returns, so
// counting a request is one atomic add.
const (
	routeAssess = iota
	routeWebhook
	routeRulesGet
	routeRulesPut
	routeRulesTest
	routeRulesSweep
	routeRulesShadow
	routeHealth
	routeMetrics
	routeInfo
	routePage
	numRoutes
)

var routeNames = [numRoutes]string{
	"/v1/assess", "/v1/webhooks/clearinghouse", "GET /v1/rules", "PUT /v1/rules",
	"/v1/rules/test", "/v1/rules/sweep", "/v1/rules/shadow", "/healthz", "/metrics",
	"/v1/info", "/",
}

// statusCodes are the codes counted individually; anything else is "other".
var statusCodes = [...]int{200, 400, 404, 405, 409, 413, 422, 500, 503}

const numCodes = len(statusCodes) + 1

func codeIndex(code int) int {
	for i, c := range statusCodes {
		if c == code {
			return i
		}
	}
	return len(statusCodes)
}

// latencyBuckets are the upper bounds, in seconds, of the assess latency
// histogram. They are dense below 5ms, where a healthy service lives, and
// reach past Clearinghouse's 50ms starting deadline.
var latencyBuckets = [...]float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 1}

type histogram struct {
	counts [len(latencyBuckets) + 1]atomic.Uint64 // last is +Inf
	sumNs  atomic.Int64
}

func (h *histogram) observe(d time.Duration) {
	s := d.Seconds()
	i := 0
	for i < len(latencyBuckets) && s > latencyBuckets[i] {
		i++
	}
	h.counts[i].Add(1)
	h.sumNs.Add(int64(d))
}

type metrics struct {
	requests  [numRoutes][numCodes]atomic.Uint64
	decisions [4]atomic.Uint64 // by rules.Action; index 0 unused
	latency   histogram

	deadlineExceeded atomic.Uint64
	idempotentHits   atomic.Uint64
	snapshotFailures atomic.Uint64
}

func (m *metrics) count(route, code int) { m.requests[route][codeIndex(code)].Add(1) }

// gauge is a value computed at scrape time.
type gauge struct {
	name, help, typ string
	labels          string // pre-rendered, e.g. `{kind="fraud"}`, or ""
	value           float64
}

// ruleCount is one rule's decision counter, for riskgate_rule_decisions_total.
type ruleCount struct {
	id, action string
	n          uint64
}

// write renders every metric. extra holds the gauges and counters owned by
// other components, gathered by the caller.
func (m *metrics) write(w io.Writer, rulesByID []ruleCount, extra []gauge) error {
	bw := bufio.NewWriter(w)
	p := func(format string, args ...any) { fmt.Fprintf(bw, format, args...) }

	p("# HELP riskgate_http_requests_total HTTP requests by route and status code.\n")
	p("# TYPE riskgate_http_requests_total counter\n")
	for r := range numRoutes {
		for c := range numCodes {
			n := m.requests[r][c].Load()
			if n == 0 {
				continue
			}
			code := "other"
			if c < len(statusCodes) {
				code = strconv.Itoa(statusCodes[c])
			}
			p("riskgate_http_requests_total{route=%q,code=%q} %d\n", routeNames[r], code, n)
		}
	}

	p("# HELP riskgate_decisions_total Assessments by decision.\n")
	p("# TYPE riskgate_decisions_total counter\n")
	for i, a := range []string{"allow", "block", "review"} {
		p("riskgate_decisions_total{decision=%q} %d\n", a, m.decisions[i+1].Load())
	}

	p("# HELP riskgate_rule_decisions_total Assessments decided by each live rule, by stable rule id.\n")
	p("# TYPE riskgate_rule_decisions_total counter\n")
	slices.SortFunc(rulesByID, func(a, b ruleCount) int { return cmp.Compare(a.id, b.id) })
	for _, rc := range rulesByID {
		p("riskgate_rule_decisions_total{rule_id=%q,action=%q} %d\n", rc.id, rc.action, rc.n)
	}

	p("# HELP riskgate_assess_latency_seconds Time from request received to response written, inside the handler.\n")
	p("# TYPE riskgate_assess_latency_seconds histogram\n")
	var cum uint64
	for i := range m.latency.counts {
		cum += m.latency.counts[i].Load()
		le := "+Inf"
		if i < len(latencyBuckets) {
			le = strconv.FormatFloat(latencyBuckets[i], 'g', -1, 64)
		}
		p("riskgate_assess_latency_seconds_bucket{le=%q} %d\n", le, cum)
	}
	p("riskgate_assess_latency_seconds_sum %g\n", float64(m.latency.sumNs.Load())/1e9)
	p("riskgate_assess_latency_seconds_count %d\n", cum)

	counter := func(name, help string, v uint64) {
		p("# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	counter("riskgate_assess_deadline_exceeded_total", "Assessments answered after the caller's RiskGate-Deadline-Ms had passed.", m.deadlineExceeded.Load())
	counter("riskgate_idempotent_replays_total", "Assess requests answered from the idempotency store without touching velocity state.", m.idempotentHits.Load())
	counter("riskgate_snapshot_failures_total", "Snapshots that failed to write.", m.snapshotFailures.Load())

	// The format requires a metric's samples to be contiguous.
	slices.SortStableFunc(extra, func(a, b gauge) int { return cmp.Compare(a.name, b.name) })
	seen := map[string]bool{}
	for _, g := range extra {
		if !seen[g.name] {
			seen[g.name] = true
			p("# HELP %s %s\n# TYPE %s %s\n", g.name, g.help, g.name, g.typ)
		}
		p("%s%s %s\n", g.name, g.labels, strconv.FormatFloat(g.value, 'g', -1, 64))
	}
	return bw.Flush()
}
