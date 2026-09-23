package backtest

import (
	"runtime"
	"sync"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// Benchmarks for experiment 6 over a SYNTHETIC table the size of IEEE-CIS
// (590,540 rows). cmd/backtest bench runs the same measurements on the real
// feature table and labels the machine.

const ieeeRows = 590_540

var bench struct {
	once sync.Once
	t    *Table
	rows []schema.Row
	set  *rules.RuleSet
	one  *rules.RuleSet
}

func benchSetup(b *testing.B) {
	bench.once.Do(func() {
		env := testEnv()
		bench.t = Synthetic(env.Catalog, SynthOptions{Rows: ieeeRows, Seed: 6})
		bench.rows = bench.t.Rows()
		var err error
		if bench.set, err = rules.Load(RealisticRuleSet(), env, 1); err != nil {
			panic(err)
		}
		if bench.one, err = rules.Load(BenchOneRule, env, 1); err != nil {
			panic(err)
		}
	})
	b.ResetTimer()
}

func benchVector(b *testing.B, set func() *rules.RuleSet, threads int) {
	benchSetup(b)
	s := set()
	for b.Loop() {
		if _, err := EvaluateRuleSet(s, bench.t, threads); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(bench.t.N)*float64(b.N)/b.Elapsed().Seconds(), "rows/s")
}

func benchRows(b *testing.B, set func() *rules.RuleSet, threads int) {
	benchSetup(b)
	s := set()
	for b.Loop() {
		evaluateRowsParallel(s, bench.rows, threads)
	}
	b.ReportMetric(float64(len(bench.rows))*float64(b.N)/b.Elapsed().Seconds(), "rows/s")
}

func one() *rules.RuleSet   { return bench.one }
func fifty() *rules.RuleSet { return bench.set }

func BenchmarkOneRuleVectorized1(b *testing.B)      { benchVector(b, one, 1) }
func BenchmarkOneRuleVectorizedPar(b *testing.B)    { benchVector(b, one, runtime.GOMAXPROCS(0)) }
func BenchmarkOneRuleRowAtATime1(b *testing.B)      { benchRows(b, one, 1) }
func BenchmarkOneRuleRowAtATimePar(b *testing.B)    { benchRows(b, one, runtime.GOMAXPROCS(0)) }
func BenchmarkFiftyRulesVectorized1(b *testing.B)   { benchVector(b, fifty, 1) }
func BenchmarkFiftyRulesVectorizedPar(b *testing.B) { benchVector(b, fifty, runtime.GOMAXPROCS(0)) }
func BenchmarkFiftyRulesRowAtATime1(b *testing.B)   { benchRows(b, fifty, 1) }
func BenchmarkFiftyRulesRowAtATimePar(b *testing.B) { benchRows(b, fifty, runtime.GOMAXPROCS(0)) }

// BenchmarkBacktestRun is the latency behind the page: one proposed rule
// against a cached 50-rule baseline, report and summary included.
func BenchmarkBacktestRun(b *testing.B) {
	benchSetup(b)
	bt, err := NewBacktester(bench.t, bench.set, 0)
	if err != nil {
		b.Fatal(err)
	}
	rs, err := rules.Load(`block if :risk_score: >= 80 and :amount: > 100`, testEnv(), 1)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := bt.Run(rs.Rules[0], Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSweep(b *testing.B) {
	benchSetup(b)
	bt, err := NewBacktester(bench.t, bench.set, 0)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := bt.Sweep(Options{}, true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoad(b *testing.B) {
	benchSetup(b)
	path := b.TempDir() + "/t.rgt"
	if err := bench.t.Save(path); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := Load(path, bench.t.Catalog); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRunBench(t *testing.T) {
	env := testEnv()
	tbl := Synthetic(env.Catalog, SynthOptions{Rows: 2000, Seed: 1})
	set, err := rules.Load(RealisticRuleSet(), env, 1)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := RunBench(tbl, tbl.Rows(), env, set, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 8 {
		t.Fatalf("%d cases", len(cases))
	}
	for _, c := range cases {
		if c.Best <= 0 || c.Median < c.Best || c.RowsPerSecond() <= 0 {
			t.Errorf("%+v", c)
		}
	}
	// The row-at-a-time helper agrees with the vectorized decisions.
	d, err := EvaluateRuleSet(set, tbl, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range evaluateRowsParallel(set, tbl.Rows(), 3) {
		if d.Action[i] != a {
			t.Fatalf("row %d", i)
		}
	}
}
