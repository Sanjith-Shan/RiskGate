package backtest

import (
	"runtime"
	"sync"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// Experiment 6 measures how long a backtest's evaluation takes, vectorized
// against row-at-a-time, over the same cached features. The harness lives
// here rather than only in _test.go so that cmd/backtest can run it on the
// real table and print a labelled table of results.

// BenchCase is one measurement.
type BenchCase struct {
	Workload string        `json:"workload"` // "1 rule" or "50-rule set"
	Method   string        `json:"method"`
	Threads  int           `json:"threads"`
	Rows     int           `json:"rows"`
	Best     time.Duration `json:"best_ns"`
	Median   time.Duration `json:"median_ns"`
}

// RowsPerSecond is the throughput of the best run.
func (c BenchCase) RowsPerSecond() float64 { return float64(c.Rows) / c.Best.Seconds() }

// timeIt runs f reps times and returns the best and median durations.
func timeIt(reps int, f func()) (best, median time.Duration) {
	ds := make([]time.Duration, reps)
	for i := range ds {
		start := time.Now()
		f()
		ds[i] = time.Since(start)
	}
	for i := 1; i < len(ds); i++ { // insertion sort: reps is small
		for j := i; j > 0 && ds[j] < ds[j-1]; j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
	return ds[0], ds[len(ds)/2]
}

// BenchOneRule is the single rule experiment 6 times.
const BenchOneRule = `block if :purchaser_email_domain: in @risky_domains and :amount: > 300 or :card_txn_count_1h: >= 8`

// RunBench measures one rule and the rule set rs over t: vectorized on one
// thread and on GOMAXPROCS, and row-at-a-time over a pre-materialized row
// store (rows, which must be t.Rows()) on one thread and on GOMAXPROCS.
// Row-at-a-time gets the row store for free, which flatters it: decoding
// rows from the columns would cost more than evaluating them.
func RunBench(t *Table, rows []schema.Row, env rules.Env, rs *rules.RuleSet, reps int) ([]BenchCase, error) {
	one, err := rules.Load(BenchOneRule, env, 1)
	if err != nil {
		return nil, err
	}
	par := runtime.GOMAXPROCS(0)
	var out []BenchCase
	add := func(workload, method string, threads int, f func()) {
		f() // warm up: page in columns, build lowered dictionaries
		best, med := timeIt(reps, f)
		out = append(out, BenchCase{Workload: workload, Method: method, Threads: threads, Rows: t.N, Best: best, Median: med})
	}
	for _, w := range []struct {
		name string
		set  *rules.RuleSet
	}{{"1 rule", one}, {"50-rule set", rs}} {
		set := w.set
		for _, threads := range []int{1, par} {
			add(w.name, "vectorized", threads, func() {
				if _, err := EvaluateRuleSet(set, t, threads); err != nil {
					panic(err)
				}
			})
		}
		for _, threads := range []int{1, par} {
			add(w.name, "row-at-a-time", threads, func() { evaluateRowsParallel(set, rows, threads) })
		}
	}
	return out, nil
}

// evaluateRowsParallel is RuleSet.Evaluate on every row, split into
// contiguous ranges across goroutines.
func evaluateRowsParallel(rs *rules.RuleSet, rows []schema.Row, threads int) []rules.Action {
	out := make([]rules.Action, len(rows))
	var wg sync.WaitGroup
	per := (len(rows) + threads - 1) / threads
	for lo := 0; lo < len(rows); lo += per {
		hi := min(lo+per, len(rows))
		wg.Go(func() {
			for i := lo; i < hi; i++ {
				out[i] = rs.Evaluate(rows[i]).Action
			}
		})
	}
	wg.Wait()
	return out
}
