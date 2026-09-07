// Command state runs experiment 4, the velocity state shootout, on one
// dataset: the exact, bucketed and sketch states compared on memory, speed,
// feature error against exact, what idle-key eviction does, and what the
// approximation does to the model's metrics on the validation month.
//
//	go run ./cmd/experiments/state -model models/ieee -out results/state_shootout/state.json
//
// The metrics part scores the validation month with the trained model (the
// one trained on exact-state features, not retrained), each state computing
// the features through the same replay the export uses. The replay stops
// at the end of the validation month, so no test-month payment is scored.
// Every other part replays all rows; those measure the implementation, not
// fraud.
//
// Only aggregates are written.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/features"
	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "state:", err)
		os.Exit(1)
	}
}

// kind is one state implementation under test.
type kind struct {
	Name string
	New  func(idleTTL int64) features.State
}

var kinds = []kind{
	{"exact", func(ttl int64) features.State { return features.NewExact(ttl) }},
	{"bucketed", func(ttl int64) features.State { return features.NewBucketed(features.BucketedConfig{IdleTTL: ttl}) }},
	{"sketch", func(ttl int64) features.State { return features.NewSketch(features.SketchConfig{IdleTTL: ttl}) }},
}

// Result is the whole output file.
type Result struct {
	Data       string        `json:"data"`
	Synthetic  bool          `json:"synthetic"`
	Rows       int           `json:"rows"`
	KeyedOps   int           `json:"keyed_ops"` // (payment, entity) pairs with a key
	Reps       int           `json:"reps"`
	States     []StateResult `json:"states"`
	Validation *Validation   `json:"validation,omitempty"`
	Notes      []string      `json:"notes"`
}

// StateResult is one state's numbers.
type StateResult struct {
	Name    string     `json:"name"`
	Memory  Memory     `json:"memory"`
	Timing  Timing     `json:"timing"`
	Errors  []Family   `json:"error_by_family,omitempty"` // nil for exact, the reference
	Feature []FeatErr  `json:"error_by_feature,omitempty"`
	Metrics *ModelEval `json:"validation_metrics,omitempty"`
}

// Memory is the state's size during and after a full replay, with the
// default idle-key eviction and without it. Keys are live keys as the state
// counts them; the sketch cannot count keys, so its per-key figures divide
// by the exact state's live keys at the same point.
type Memory struct {
	EndKeys           int     `json:"end_keys"`
	EndBytes          int64   `json:"end_bytes_estimated"`
	EndBytesPerKey    float64 `json:"end_bytes_per_key"`
	PeakKeys          int     `json:"peak_keys"`
	PeakBytes         int64   `json:"peak_bytes_estimated"`
	HeapBytes         int64   `json:"heap_bytes_measured"` // live heap after GC at the end, minus before
	NoEvictEndKeys    int     `json:"no_evict_end_keys"`
	NoEvictEndBytes   int64   `json:"no_evict_end_bytes_estimated"`
	NoEvictHeapBytes  int64   `json:"no_evict_heap_bytes_measured"`
	EvictionIdentical bool    `json:"eviction_features_identical"` // every feature bit-identical with and without eviction
}

// Timing is ns per state operation, median and min over reps.
type Timing struct {
	AddMedianNs     float64   `json:"add_median_ns"`
	AddMinNs        float64   `json:"add_min_ns"`
	ReadAddMedianNs float64   `json:"read_add_median_ns"`
	ReadAddMinNs    float64   `json:"read_add_min_ns"`
	ReadMedianNs    float64   `json:"read_median_ns"`
	ReadMinNs       float64   `json:"read_min_ns"`
	AddRuns         []float64 `json:"add_runs_ns"`
	ReadAddRuns     []float64 `json:"read_add_runs_ns"`
	ReadRuns        []float64 `json:"read_runs_ns"`
}

// FeatErr is features.FeatureError with the derived shares.
type FeatErr struct {
	features.FeatureError
	ExactShare float64 `json:"exact_share"`
}

// Family aggregates the feature error over one family of features.
type Family struct {
	Family          string  `json:"family"`
	Features        int     `json:"features"`
	Rows            int     `json:"rows"` // feature-row pairs with both values present
	ExactShare      float64 `json:"exact_share"`
	MeanRel         float64 `json:"mean_rel"` // mean of |apx - ref| / max(|ref|, 1)
	MeanAbs         float64 `json:"mean_abs"`
	MaxAbs          float64 `json:"max_abs"`
	OverShare       float64 `json:"over_share"`
	UnderShare      float64 `json:"under_share"`
	MissingMismatch int     `json:"missing_mismatch"`
}

// Validation describes the rows the metrics are computed on.
type Validation struct {
	Model string `json:"model"`
	Rows  int    `json:"rows"`
	Fraud int    `json:"fraud"`
	Score string `json:"score"`
}

// ModelEval is the model's quality on the validation month with one
// state's features.
type ModelEval struct {
	ROCAUC          float64 `json:"roc_auc"`
	PRAUC           float64 `json:"pr_auc"` // average precision
	RecallAt1PctFPR float64 `json:"recall_at_1pct_fpr"`
	// ScoreChanged is the share of validation rows whose raw score differs
	// from the exact state's.
	ScoreChanged float64 `json:"score_changed_share"`
}

// run writes the JSON to out (or -out) and progress to logw.
func run(args []string, out, logw io.Writer) error {
	fs := flag.NewFlagSet("state", flag.ContinueOnError)
	dataDir := fs.String("data", "data", "data directory (raw/, cache/)")
	synthetic := fs.Bool("synthetic", false, "use cmd/synth's SYNTHETIC data instead of IEEE-CIS")
	modelDir := fs.String("model", "", "trained model directory; without one the validation metrics are skipped")
	reps := fs.Int("reps", 5, "timing repetitions (median and min are reported)")
	outPath := fs.String("out", "", "JSON file to write (default: print)")
	only := fs.String("states", "exact,bucketed,sketch", "comma-separated states to run")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths := data.RealPaths(*dataDir)
	if *synthetic {
		paths = data.SyntheticPaths(*dataDir)
	}
	ds, err := data.Load(paths)
	if err != nil {
		return err
	}
	var scorer *model.Scorer
	if *modelDir != "" {
		if scorer, err = model.LoadScorer(*modelDir, schema.Default()); err != nil {
			return err
		}
		if scorer.Synthetic() != ds.Synthetic {
			return errors.New("model and data disagree on whether they are synthetic")
		}
	}
	var sel []kind
	for _, k := range kinds {
		if slices.Contains(strings.Split(*only, ","), k.Name) {
			sel = append(sel, k)
		}
	}
	res, err := shootout(ds, sel, scorer, max(*reps, 1), func(s string) { fmt.Fprintln(logw, s) })
	if err != nil {
		return err
	}
	if *modelDir != "" {
		res.Validation.Model = *modelDir
	}
	res.Data = "IEEE-CIS (real)"
	if ds.Synthetic {
		res.Data = "SYNTHETIC (cmd/synth)"
	}
	b, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		return err
	}
	if *outPath == "" {
		_, err = out.Write(append(b, '\n'))
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(*outPath, append(b, '\n'), 0o644)
}

// shootout runs every part of the experiment for each state.
func shootout(ds *data.Dataset, sel []kind, scorer *model.Scorer, reps int, log func(string)) (*Result, error) {
	txns := ds.Txns
	cat := schema.Default()
	evs := keyedEvents(txns)
	res := &Result{Synthetic: ds.Synthetic, Rows: len(txns), KeyedOps: len(evs), Reps: reps}
	res.Notes = []string{
		"Timings are per state operation on one goroutine, over the dataset's own (payment, entity) stream in event order.",
		"add: Add for every keyed event, with EvictIdle every 65,536 adds as the engine does. read_add: ReadAdd, the engine's per-key operation. read: ReadAdd minus Add over the last 100,000 payments' keyed events, each from the same warm state restored from a snapshot, per event; a derived difference, so noisy, and it can come out near zero or slightly negative when reading is nearly free.",
		"Bytes are each state's own estimate (Stats); heap_bytes_measured is the live heap after GC. The sketch's size is fixed; its per-key figure divides by the exact state's live keys.",
		"Feature error replays every row through exact and the approximate state in lockstep (features.CompareStates).",
	}
	var exactScores []float64
	if scorer != nil {
		v := validationRows(ds)
		res.Validation = &Validation{Rows: v.hi - v.lo, Fraud: v.fraud,
			Score: "raw LightGBM log-odds from the Go evaluator; the model was trained on exact-state features and is not retrained"}
	}
	for _, k := range sel {
		log("state " + k.Name + ": memory")
		sr := StateResult{Name: k.Name}
		var err error
		if sr.Memory, err = memory(cat, txns, k); err != nil {
			return nil, err
		}
		log("state " + k.Name + ": timing")
		if sr.Timing, err = timing(txns, evs, k, reps); err != nil {
			return nil, err
		}
		if k.Name != "exact" {
			log("state " + k.Name + ": feature error")
			errs, err := features.CompareStates(cat, txns, features.NewExact(0), k.New(0))
			if err != nil {
				return nil, err
			}
			for _, e := range errs {
				sr.Feature = append(sr.Feature, FeatErr{e, e.ExactFraction()})
			}
			sr.Errors = byFamily(errs)
		}
		if scorer != nil {
			log("state " + k.Name + ": validation metrics")
			scores, labels, err := validationScores(cat, ds, k, scorer)
			if err != nil {
				return nil, err
			}
			m := evaluate(labels, scores)
			if exactScores == nil && k.Name == "exact" {
				exactScores = scores
			}
			if exactScores != nil {
				changed := 0
				for i, s := range scores {
					if math.Float64bits(s) != math.Float64bits(exactScores[i]) {
						changed++
					}
				}
				m.ScoreChanged = float64(changed) / float64(len(scores))
			}
			sr.Metrics = &m
		}
		res.States = append(res.States, sr)
	}
	// The sketch counts no keys; give it the exact state's.
	var exact *StateResult
	for i := range res.States {
		if res.States[i].Name == "exact" {
			exact = &res.States[i]
		}
	}
	for i := range res.States {
		m := &res.States[i].Memory
		if m.EndKeys < 0 && exact != nil {
			m.EndKeys, m.PeakKeys = exact.Memory.EndKeys, exact.Memory.PeakKeys
		}
		if m.EndKeys > 0 {
			m.EndBytesPerKey = float64(m.EndBytes) / float64(m.EndKeys)
		}
	}
	return res, nil
}

type keyedEvent struct {
	k  features.Key
	ev features.Event
}

// keyedEvents is every (payment, entity) pair with a key, in replay order.
func keyedEvents(txns []data.Txn) []keyedEvent {
	var out []keyedEvent
	for i := range txns {
		keys, ok := features.KeysOf(&txns[i])
		ev := features.EventOf(&txns[i])
		for e := range features.Entity(features.NumEntities) {
			if ok[e] {
				out = append(out, keyedEvent{keys[e], ev})
			}
		}
	}
	return out
}

// memory replays everything through the engine twice, with the default
// eviction and with none, sampling the state's size as it goes, and checks
// that eviction changed no feature.
func memory(cat *schema.Catalog, txns []data.Txn, k kind) (Memory, error) {
	var m Memory
	pass := func(evict bool) (keys int, bytes, heap int64, peakKeys int, peakBytes int64, sum uint64, err error) {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		st := k.New(0)
		e, err := features.NewEngine(cat, st)
		if err != nil {
			return
		}
		if !evict {
			e.SetEvictEvery(0)
		}
		h := fnv.New64a()
		var buf [8]byte
		n := 0
		err = features.Replay(e, txns, func(_ *data.Txn, row schema.Row) error {
			for _, v := range row.Num {
				b := math.Float64bits(v)
				for i := range buf {
					buf[i] = byte(b >> (8 * i))
				}
				h.Write(buf[:])
			}
			if n++; n%features.DefaultEvictEvery == 0 {
				s := st.Stats()
				peakKeys, peakBytes = max(peakKeys, s.Keys), max(peakBytes, s.Bytes)
			}
			return nil
		})
		if err != nil {
			return
		}
		s := st.Stats()
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(st)
		return s.Keys, s.Bytes, int64(after.HeapAlloc) - int64(before.HeapAlloc),
			max(peakKeys, s.Keys), max(peakBytes, s.Bytes), h.Sum64(), nil
	}
	keys, bytes, heap, pk, pb, sum, err := pass(true)
	if err != nil {
		return m, err
	}
	nkeys, nbytes, nheap, _, _, nsum, err := pass(false)
	if err != nil {
		return m, err
	}
	m = Memory{EndKeys: keys, EndBytes: bytes, HeapBytes: heap, PeakKeys: pk, PeakBytes: pb,
		NoEvictEndKeys: nkeys, NoEvictEndBytes: nbytes, NoEvictHeapBytes: nheap, EvictionIdentical: sum == nsum}
	if keys < 0 { // sketches count no keys
		m.PeakKeys = -1
	}
	return m, nil
}

// timing measures ns per Add, ReadAdd and Read.
func timing(txns []data.Txn, evs []keyedEvent, k kind, reps int) (Timing, error) {
	var t Timing
	perOp := func(d time.Duration, n int) float64 { return float64(d.Nanoseconds()) / float64(n) }
	for range reps {
		st := k.New(0)
		start := time.Now()
		for i, e := range evs {
			st.Add(e.k, e.ev)
			if (i+1)%features.DefaultEvictEvery == 0 {
				st.EvictIdle(e.ev.Time)
			}
		}
		t.AddRuns = append(t.AddRuns, perOp(time.Since(start), len(evs)))

		st = k.New(0)
		start = time.Now()
		for i, e := range evs {
			st.ReadAdd(e.k, e.ev)
			if (i+1)%features.DefaultEvictEvery == 0 {
				st.EvictIdle(e.ev.Time)
			}
		}
		t.ReadAddRuns = append(t.ReadAddRuns, perOp(time.Since(start), len(evs)))
	}
	// Reads in context. A Read on its own must be at the time of a payment
	// whose predecessors are all in the state, which only a replay
	// provides, so the read cost is measured as a difference: from two
	// identical warm states (restored from one snapshot of everything
	// before the last tail payments), ReadAdd and Add over the tail's
	// keyed events, per event.
	tail := min(100000, len(txns)/2)
	warm := k.New(0)
	for i, e := range keyedEvents(txns[:len(txns)-tail]) {
		warm.Add(e.k, e.ev)
		if (i+1)%features.DefaultEvictEvery == 0 {
			warm.EvictIdle(e.ev.Time)
		}
	}
	var snap bytes.Buffer
	if err := warm.Snapshot(&snap); err != nil {
		return t, err
	}
	probe := keyedEvents(txns[len(txns)-tail:])
	restored := func() (features.State, error) {
		st := k.New(0)
		return st, st.Restore(bytes.NewReader(snap.Bytes()))
	}
	timeTail := func(readAdd bool) (time.Duration, error) {
		st, err := restored()
		if err != nil {
			return 0, err
		}
		start := time.Now()
		for _, e := range probe {
			if readAdd {
				st.ReadAdd(e.k, e.ev)
			} else {
				st.Add(e.k, e.ev)
			}
		}
		return time.Since(start), nil
	}
	for rep := range reps {
		// Alternate which goes first, so warm-up favours neither.
		first := rep%2 == 0
		a, err := timeTail(first)
		if err != nil {
			return t, err
		}
		b, err := timeTail(!first)
		if err != nil {
			return t, err
		}
		if !first {
			a, b = b, a
		}
		t.ReadRuns = append(t.ReadRuns, perOp(a-b, len(probe)))
	}
	t.AddMedianNs, t.AddMinNs = medianMin(t.AddRuns)
	t.ReadAddMedianNs, t.ReadAddMinNs = medianMin(t.ReadAddRuns)
	t.ReadMedianNs, t.ReadMinNs = medianMin(t.ReadRuns)
	return t, nil
}

func medianMin(xs []float64) (median, lo float64) {
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[len(s)/2], s[0]
}

// family names the kind of velocity feature: count, sum, distinct, derived
// (mean and ratio) or recency (seconds since first and last).
func family(name string) string {
	switch {
	case strings.Contains(name, "_txn_count_"):
		return "count"
	case strings.Contains(name, "_amount_sum_"):
		return "sum"
	case strings.HasPrefix(name, "distinct_cards_"):
		return "distinct"
	case strings.Contains(name, "_seconds_since_"):
		return "recency"
	}
	return "derived (mean, ratio)"
}

func byFamily(errs []features.FeatureError) []Family {
	var order []string
	agg := map[string]*Family{}
	sumAbs, sumRel := map[string]float64{}, map[string]float64{}
	exact, over, under := map[string]int{}, map[string]int{}, map[string]int{}
	for _, e := range errs {
		f := family(e.Name)
		a := agg[f]
		if a == nil {
			a = &Family{Family: f}
			agg[f] = a
			order = append(order, f)
		}
		a.Features++
		a.Rows += e.Rows
		a.MissingMismatch += e.MissingMismatch
		a.MaxAbs = max(a.MaxAbs, e.MaxAbs)
		sumAbs[f] += e.MeanAbs * float64(e.Rows)
		sumRel[f] += e.MeanRel * float64(e.Rows)
		exact[f] += e.Exact
		over[f] += e.Over
		under[f] += e.Under
	}
	out := make([]Family, 0, len(order))
	for _, f := range order {
		a := agg[f]
		if n := float64(a.Rows); n > 0 {
			a.ExactShare = float64(exact[f]) / n
			a.MeanAbs = sumAbs[f] / n
			a.MeanRel = sumRel[f] / n
			a.OverShare = float64(over[f]) / n
			a.UnderShare = float64(under[f]) / n
		}
		out = append(out, *a)
	}
	return out
}

type span struct{ lo, hi, fraud int }

// validationRows returns the index range of the validation month in the
// sorted transactions. Months are contiguous in event order.
func validationRows(ds *data.Dataset) span {
	var s span
	s.lo, s.hi = -1, -1
	for i := range ds.Txns {
		if ds.Calendar.Split(ds.Txns[i].DT) != data.Valid {
			continue
		}
		if s.lo < 0 {
			s.lo = i
		}
		s.hi = i + 1
		if ds.Txns[i].IsFraud == 1 {
			s.fraud++
		}
	}
	return s
}

// validationScores replays the payments up to the end of the validation
// month through state k and returns the model's raw score and the label of
// each validation payment. Test-month payments are never replayed.
func validationScores(cat *schema.Catalog, ds *data.Dataset, k kind, sc *model.Scorer) (scores []float64, labels []bool, err error) {
	v := validationRows(ds)
	if v.lo < 0 {
		return nil, nil, errors.New("no validation rows")
	}
	e, err := features.NewEngine(cat, k.New(0))
	if err != nil {
		return nil, nil, err
	}
	i := 0
	err = features.Replay(e, ds.Txns[:v.hi], func(t *data.Txn, row schema.Row) error {
		if i >= v.lo {
			if ds.Calendar.Split(t.DT) != data.Valid {
				return fmt.Errorf("row %d is not in the validation month", i)
			}
			_, _, raw := sc.Score(row)
			scores = append(scores, raw)
			labels = append(labels, t.IsFraud == 1)
		}
		i++
		return nil
	})
	return scores, labels, err
}

// evaluate computes ROC-AUC, average precision and recall at 1% FPR, with
// scikit-learn's definitions: tied scores form one threshold; ROC-AUC is the
// trapezoidal area; average precision is sum over thresholds of (recall
// gain) x precision; recall at 1% FPR is the recall at the lowest
// threshold whose false-positive rate is at most 1%.
func evaluate(labels []bool, scores []float64) ModelEval {
	idx := make([]int, len(scores))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		switch {
		case scores[a] > scores[b]:
			return -1
		case scores[a] < scores[b]:
			return 1
		}
		return 0
	})
	pos, neg := 0, 0
	for _, y := range labels {
		if y {
			pos++
		} else {
			neg++
		}
	}
	var m ModelEval
	if pos == 0 || neg == 0 {
		return m
	}
	tp, fp := 0, 0
	prevTPR, prevFPR := 0.0, 0.0
	for i := 0; i < len(idx); {
		j := i
		for j < len(idx) && scores[idx[j]] == scores[idx[i]] {
			if labels[idx[j]] {
				tp++
			} else {
				fp++
			}
			j++
		}
		tpr, fpr := float64(tp)/float64(pos), float64(fp)/float64(neg)
		m.ROCAUC += (fpr - prevFPR) * (tpr + prevTPR) / 2
		m.PRAUC += (tpr - prevTPR) * float64(tp) / float64(tp+fp)
		if fpr <= 0.01 {
			m.RecallAt1PctFPR = tpr
		}
		prevTPR, prevFPR = tpr, fpr
		i = j
	}
	return m
}
