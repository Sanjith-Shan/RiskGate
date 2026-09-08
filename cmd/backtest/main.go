// Command backtest runs RiskGate's backtester and its experiments from the
// command line.
//
//	backtest difftest   -rules 5000 -table synthetic   experiment 3: two evaluators, one answer
//	backtest bench      -table features.rgt           experiment 6: vectorized vs row-at-a-time
//	backtest run        -table features.rgt -rule 'block if ...' [-current rules.txt]
//	backtest sweep      -table features.rgt [-current rules.txt]
//	backtest labeldelay -table features.rgt -rule 'block if ...'   experiment 7 (SIMULATED)
//	backtest synth      -out synthetic.rgt -rows 590540
//	backtest rulestats  -table features.rgt -rules baseline.rules -split valid
//	backtest flag       -table features.rgt -rules baseline.rules -out predictions.csv
//
// -table takes a table file written by backtest.Table.Save, or "synthetic"
// for a generated table (-rows, -seed, -missing, -edge). Numbers from a
// synthetic table are about the code, never about payments. Named lists in
// rules are rules.SampleLists.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "backtest:", err)
		os.Exit(1)
	}
}

var commands = map[string]func(args []string, out io.Writer) error{
	"difftest":   difftest,
	"bench":      bench,
	"run":        runRule,
	"sweep":      sweep,
	"labeldelay": labelDelay,
	"synth":      synth,
	"flag":       flagRows,
	"rulestats":  rulestats,
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 || commands[args[0]] == nil {
		return errors.New("usage: backtest difftest|bench|run|sweep|labeldelay|synth|flag|rulestats [flags] (see -h on each)")
	}
	return commands[args[0]](args[1:], out)
}

// tableFlags are shared by every subcommand that reads a table.
type tableFlags struct {
	table   string
	rows    int
	seed    uint64
	missing float64
	edge    float64
}

func (f *tableFlags) register(fs *flag.FlagSet, rows int) {
	fs.StringVar(&f.table, "table", "synthetic", `table file from Table.Save, or "synthetic"`)
	fs.IntVar(&f.rows, "rows", rows, "rows in a synthetic table")
	fs.Uint64Var(&f.seed, "seed", 1, "seed for a synthetic table")
	fs.Float64Var(&f.missing, "missing", 0, "extra missing-value probability in a synthetic table")
	fs.Float64Var(&f.edge, "edge", 0, "adversarial-value probability in a synthetic table")
	fs.Func("lists", "JSON file of named lists, such as rules/lists.json (default: built-in sample lists)", loadListsFlag)
}

func (f *tableFlags) load(out io.Writer) (*backtest.Table, error) {
	cat := schema.Default()
	start := time.Now()
	if f.table == "synthetic" {
		t := backtest.Synthetic(cat, backtest.SynthOptions{Rows: f.rows, Seed: f.seed, Missing: f.missing, Edge: f.edge})
		fmt.Fprintf(out, "table: SYNTHETIC, %s rows (seed %d, extra missing %.2f, edge %.2f), built in %s\n",
			commas(t.N), f.seed, f.missing, f.edge, time.Since(start).Round(time.Millisecond))
		return t, nil
	}
	t, err := backtest.Load(f.table, cat)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "table: %s, %s rows, loaded in %s\n", f.table, commas(t.N), time.Since(start).Round(time.Millisecond))
	return t, nil
}

// ruleLists are the named lists rules may use: rules.SampleLists unless a
// command was given -lists (the service's lists file, rules/lists.json).
var ruleLists = rules.SampleLists()

func env() rules.Env { return rules.Env{Catalog: schema.Default(), Lists: ruleLists} }

// loadListsFlag reads a JSON lists file into ruleLists.
func loadListsFlag(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	l, err := rules.ParseLists(b)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	ruleLists = l
	return nil
}

func parse(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		var b strings.Builder
		fs.SetOutput(&b)
		fs.PrintDefaults()
		return fmt.Errorf("%s: %w\n%s", fs.Name(), err, b.String())
	}
	return nil
}

func difftest(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("difftest", flag.ContinueOnError)
	var tf tableFlags
	tf.register(fs, 20000)
	n := fs.Int("rules", 5000, "rules to generate")
	seed := fs.Uint64("rule-seed", 1, "rule generator seed")
	minDepth := fs.Int("min-depth", 0, "smallest nesting depth")
	maxDepth := fs.Int("max-depth", 6, "largest nesting depth")
	ground := fs.Bool("ground", true, "draw half the rules' constants from the table's own values")
	if err := parse(fs, args); err != nil {
		return err
	}
	t, err := tf.load(out)
	if err != nil {
		return err
	}
	start := time.Now()
	half := *n / 2
	if !*ground {
		half = *n
	}
	e := env()
	res, err := backtest.DiffTest(t, e, backtest.DiffOptions{Rules: half, Seed: *seed, MinDepth: *minDepth, MaxDepth: *maxDepth})
	if err != nil {
		return err
	}
	if *n-half > 0 {
		g, err := backtest.DiffTest(t, e, backtest.DiffOptions{Rules: *n - half, Seed: *seed + 1, MinDepth: *minDepth, MaxDepth: *maxDepth, Ground: true})
		if err != nil {
			return err
		}
		res.Rules += g.Rules
		res.Checked += g.Checked
		res.Matched += g.Matched
		res.Trivial += g.Trivial
		res.Grounded += g.Grounded
		res.Disagreements += g.Disagreements
		res.BadRules += g.BadRules
		res.Examples = append(res.Examples, g.Examples...)
	}
	fmt.Fprintf(out, "rules generated:   %s (depth %d-%d, %s with constants drawn from the table)\n", commas(res.Rules), *minDepth, *maxDepth, commas(res.Grounded))
	fmt.Fprintf(out, "rows:              %s\n", commas(res.Rows))
	fmt.Fprintf(out, "rule-row checks:   %s (%s matches; %s rules match no row or every row)\n", commas(int(res.Checked)), commas(int(res.Matched)), commas(res.Trivial))
	fmt.Fprintf(out, "disagreements:     %s (in %s rules)\n", commas(int(res.Disagreements)), commas(res.BadRules))
	fmt.Fprintf(out, "time:              %s\n", time.Since(start).Round(time.Millisecond))
	for _, d := range res.Examples {
		fmt.Fprintf(out, "  row %d (id %d): closure %v, vectorized %v: %s\n", d.Row, d.ID, d.Closure, d.Vector, d.Rule)
	}
	if res.Disagreements > 0 {
		return fmt.Errorf("%d disagreements", res.Disagreements)
	}
	return nil
}

func bench(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	var tf tableFlags
	tf.register(fs, 590_540)
	reps := fs.Int("reps", 7, "timed repetitions per case (best and median are reported)")
	asJSON := fs.Bool("json", false, "print JSON")
	proposedSrc := fs.String("proposed", `block if :risk_score: >= 80 and :amount: > 100`, "the proposed rule timed against the cached 50-rule baseline")
	if err := parse(fs, args); err != nil {
		return err
	}
	t, err := tf.load(out)
	if err != nil {
		return err
	}
	e := env()
	set, err := rules.Load(backtest.RealisticRuleSet(), e, 1)
	if err != nil {
		return err
	}
	rows := t.Rows()
	cases, err := backtest.RunBench(t, rows, e, set, *reps)
	if err != nil {
		return err
	}
	// A timing is only worth reporting if the two evaluators it compares
	// give the same answer on this table.
	checked, err := backtest.CheckRuleSet(set, t, rows)
	if err != nil {
		return err
	}
	// The page's latency: one proposed rule against a cached baseline.
	bt, err := backtest.NewBacktester(t, set, 0)
	if err != nil {
		return err
	}
	proposed, err := rules.Load(*proposedSrc, e, 1)
	if err != nil {
		return err
	}
	var runErr error
	runBest, runMed := timeIt(*reps, func() { _, runErr = bt.Run(proposed.Rules[0], backtest.Options{}) })
	if runErr != nil {
		return runErr
	}
	// The same with a window the Backtester has not resolved before (the
	// first run after the period changes): a new AsOf every time.
	_, hi := t.TimeSpan()
	asOf := hi + 1
	coldBest, coldMed := timeIt(*reps, func() {
		asOf++
		_, runErr = bt.Run(proposed.Rules[0], backtest.Options{To: hi + 1, AsOf: asOf})
	})
	if runErr != nil {
		return runErr
	}
	if *asJSON {
		return json.NewEncoder(out).Encode(map[string]any{"machine": machine(), "cases": cases,
			"backtest_run_best_ns": runBest, "backtest_run_median_ns": runMed,
			"backtest_run_new_window_best_ns": coldBest, "backtest_run_new_window_median_ns": coldMed,
			"decisions_checked": checked})
	}
	fmt.Fprintf(out, "machine: %s\n", machine())
	fmt.Fprintf(out, "Timings are indicative only if the machine was busy; best and median of %d runs.\n\n", *reps)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "workload\tmethod\tthreads\tbest\tmedian\trows/s\tvs row-at-a-time\t")
	base := map[[2]any]time.Duration{}
	for _, c := range cases {
		if c.Method == "row-at-a-time" {
			base[[2]any{c.Workload, c.Threads}] = c.Best
		}
	}
	for _, c := range cases {
		speedup := float64(base[[2]any{c.Workload, c.Threads}]) / float64(c.Best)
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%.1fx\t\n", c.Workload, c.Method, c.Threads,
			ms(c.Best), ms(c.Median), commas(int(c.RowsPerSecond())), speedup)
	}
	tw.Flush()
	fmt.Fprintf(out, "\nBacktester.Run, one proposed rule against the cached 50-rule baseline: best %s, median %s\n", ms(runBest), ms(runMed))
	fmt.Fprintf(out, "The same, first run over a new period (window not yet resolved): best %s, median %s\n", ms(coldBest), ms(coldMed))
	fmt.Fprintf(out, "Vectorized and row-at-a-time decisions agree on all %s rows.\n", commas(checked))
	return nil
}

func timeIt(reps int, f func()) (best, median time.Duration) {
	f()
	ds := make([]time.Duration, max(reps, 1))
	for i := range ds {
		start := time.Now()
		f()
		ds[i] = time.Since(start)
	}
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && ds[j] < ds[j-1]; j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
	return ds[0], ds[len(ds)/2]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f ms", float64(d)/1e6) }

// machine labels benchmark output: Go version, GOMAXPROCS, CPU count and,
// where the OS says, the CPU model.
func machine() string {
	cpu := "unknown CPU"
	switch runtime.GOOS {
	case "darwin":
		if b, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			cpu = strings.TrimSpace(string(b))
		}
	case "linux":
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
					cpu = strings.TrimSpace(v)
					break
				}
			}
		}
	}
	return fmt.Sprintf("%s, %d CPUs, GOMAXPROCS=%d, %s %s/%s", cpu, runtime.NumCPU(), runtime.GOMAXPROCS(0), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// windowFlags describe the backtest window in days from the epoch.
type windowFlags struct {
	fromDay, toDay, asOfDay float64
	maturityDays            float64
	current                 string
	asJSON                  bool
}

func (w *windowFlags) register(fs *flag.FlagSet) {
	fs.Float64Var(&w.fromDay, "from-day", 0, "window start, days after the epoch (TransactionDT/86400)")
	fs.Float64Var(&w.toDay, "to-day", 0, "window end (exclusive), days after the epoch; 0 = end of table")
	fs.Float64Var(&w.asOfDay, "as-of-day", 0, "when the backtest is run, days after the epoch; 0 = window end")
	fs.Float64Var(&w.maturityDays, "maturity-days", backtest.DefaultMaturity.Hours()/24, "exclude payments this recent (negative: none)")
	fs.StringVar(&w.current, "current", "", "file with the rule set in force (default: none)")
	fs.BoolVar(&w.asJSON, "json", false, "print the report as JSON")
}

func (w *windowFlags) options() backtest.Options {
	opt := backtest.Options{
		From: int64(w.fromDay * 86400), To: int64(w.toDay * 86400), AsOf: int64(w.asOfDay * 86400),
		Maturity: time.Duration(w.maturityDays * float64(24*time.Hour)),
	}
	if w.maturityDays == 0 {
		opt.Maturity = -1 // an explicit 0 means no window, not the default
	}
	return opt
}

func (w *windowFlags) backtester(t *backtest.Table) (*backtest.Backtester, error) {
	var current *rules.RuleSet
	if w.current != "" {
		src, err := os.ReadFile(w.current)
		if err != nil {
			return nil, err
		}
		if current, err = loadRules(string(src)); err != nil {
			return nil, err
		}
	}
	return backtest.NewBacktester(t, current, 0)
}

func loadRules(src string) (*rules.RuleSet, error) {
	rs, err := rules.Load(src, env(), 1)
	if err != nil {
		var ds rules.Diagnostics
		if errors.As(err, &ds) {
			return nil, errors.New(ds.Render())
		}
		return nil, err
	}
	return rs, nil
}

func proposedRule(src string) (*rules.CompiledRule, error) {
	if strings.TrimSpace(src) == "" {
		return nil, errors.New("-rule is required")
	}
	rs, err := loadRules(src)
	if err != nil {
		return nil, err
	}
	if len(rs.Rules) != 1 {
		return nil, fmt.Errorf("-rule must hold exactly one rule, found %d", len(rs.Rules))
	}
	return rs.Rules[0], nil
}

func runRule(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var tf tableFlags
	tf.register(fs, 100_000)
	var wf windowFlags
	wf.register(fs)
	src := fs.String("rule", "", "the proposed rule")
	if err := parse(fs, args); err != nil {
		return err
	}
	proposed, err := proposedRule(*src)
	if err != nil {
		return err
	}
	t, err := tf.load(out)
	if err != nil {
		return err
	}
	b, err := wf.backtester(t)
	if err != nil {
		return err
	}
	start := time.Now()
	r, err := b.Run(proposed, wf.options())
	if err != nil {
		return err
	}
	if wf.asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	fmt.Fprintf(out, "\n%s\n\n", r.Summary())
	for _, w := range r.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	for _, o := range r.Overlaps {
		fmt.Fprintf(out, "overlap: %5.1f%% of matches (%s) with %s; it decides %s of them\n", 100*o.Share, commas(o.Both), o.Rule, commas(o.DecidedBy))
	}
	fmt.Fprintf(out, "(backtest took %s)\n", time.Since(start).Round(time.Microsecond))
	return nil
}

func sweep(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	var tf tableFlags
	tf.register(fs, 100_000)
	var wf windowFlags
	wf.register(fs)
	given := fs.Bool("given-current", false, "leave out payments an allow rule in force protects")
	step := fs.Int("step", 5, "print every step-th threshold")
	if err := parse(fs, args); err != nil {
		return err
	}
	t, err := tf.load(out)
	if err != nil {
		return err
	}
	b, err := wf.backtester(t)
	if err != nil {
		return err
	}
	res, err := b.Sweep(wf.options(), *given)
	if err != nil {
		return err
	}
	if wf.asJSON {
		return json.NewEncoder(out).Encode(res)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "risk_score >=\tmatched\tprecision\tfraud $ recall\tfraud # recall\tlegit blocked\tlegit $ blocked\t")
	for _, p := range res.Points {
		if p.Threshold%max(*step, 1) != 0 {
			continue
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t\n", p.Threshold, commas(p.Matched.Count), pct(p.Precision),
			pct(p.FraudDollarRecall), pct(p.FraudCountRecall), commas(p.Legit.Count), dollars(p.Legit.Dollars))
	}
	return tw.Flush()
}

func labelDelay(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("labeldelay", flag.ContinueOnError)
	var tf tableFlags
	tf.register(fs, 200_000)
	var wf windowFlags
	wf.register(fs)
	src := fs.String("rule", "", "the rule to backtest")
	windowDays := fs.Float64("window-days", 30, "length of the backtest window")
	model := backtest.DefaultDelay
	fs.Float64Var(&model.MedianDays, "median-days", model.MedianDays, "ASSUMED median dispute delay")
	fs.Float64Var(&model.Sigma, "sigma", model.Sigma, "ASSUMED lognormal sigma")
	fs.Uint64Var(&model.Seed, "delay-seed", model.Seed, "seed for the simulated delays")
	if err := parse(fs, args); err != nil {
		return err
	}
	proposed, err := proposedRule(*src)
	if err != nil {
		return err
	}
	t, err := tf.load(out)
	if err != nil {
		return err
	}
	t.LabelTime = backtest.SimulateLabelTimes(t, model)
	b, err := wf.backtester(t)
	if err != nil {
		return err
	}
	asOf := int64(wf.asOfDay * 86400)
	if asOf == 0 {
		_, hi := t.TimeSpan()
		asOf = hi + 1
	}
	maturity := time.Duration(wf.maturityDays * float64(24*time.Hour))
	res, err := b.CompareLabelDelay(proposed, model, asOf, time.Duration(*windowDays*float64(24*time.Hour)), maturity)
	if err != nil {
		return err
	}
	if wf.asJSON {
		return json.NewEncoder(out).Encode(res)
	}
	fmt.Fprintf(out, "\n%s\n\n", res.Summary())
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "SIMULATED\twindow\tlabels\tchanged\tfraud caught\tfraud $ caught\tprecision\t")
	for _, v := range []struct {
		name string
		v    backtest.DelayView
	}{{"naive", res.Naive}, {"naive, truth", res.NaiveTruth}, {"matured", res.Matured}, {"matured, truth", res.MaturedTruth}} {
		fmt.Fprintf(tw, "%s\t%s to %s\t%s\t%s\t%s\t%s\t%s\t\n", v.name, v.v.From.Format("2006-01-02"), v.v.To.Format("2006-01-02"), v.v.Labels,
			commas(v.v.Changed.Count), commas(v.v.Caught.Count), dollars(v.v.Caught.Dollars), pct(v.v.Precision))
	}
	return tw.Flush()
}

func synth(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("synth", flag.ContinueOnError)
	var tf tableFlags
	tf.register(fs, 590_540)
	path := fs.String("out", "", "file to write")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("-out is required")
	}
	tf.table = "synthetic"
	t, err := tf.load(out)
	if err != nil {
		return err
	}
	if err := t.Save(*path); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s (SYNTHETIC)\n", *path)
	return nil
}

func commas(n int) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 && s[i-1] != '-' {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func dollars(v float64) string { return "$" + commas(int(v+0.5)) }

func pct(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100**p)
}
