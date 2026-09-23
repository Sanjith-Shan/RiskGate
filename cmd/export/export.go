package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/features"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// File names inside the output directory.
const (
	exportCSV     = "export.csv"
	encoderJSON   = "encoder.json"
	manifestJSON  = "features.json"
	replayJSONL   = "test_replay.jsonl"
	backtestTable = "features.table"
)

// Leading columns of export.csv, before the model inputs.
var idColumns = []string{"TransactionID", "TransactionDT", "split", "month", "isFraud"}

type config struct {
	paths data.Paths
	out   string
	state string
}

type summary struct {
	synthetic         bool
	load, fit, write  time.Duration
	rows, fraud       [3]int // by split
	replayed          int
	featureCount, cat int
}

func (s summary) label() string {
	if s.synthetic {
		return "SYNTHETIC: "
	}
	return ""
}

// Manifest is features.json.
type Manifest struct {
	Synthetic        bool              `json:"synthetic"`
	Note             string            `json:"note"`
	State            string            `json:"state"`
	Label            string            `json:"label"`
	IDColumns        []string          `json:"id_columns"`
	Features         []string          `json:"features"`
	Categorical      []string          `json:"categorical"`
	RawFeatures      []string          `json:"raw_features"`
	VelocityFeatures []string          `json:"velocity_features"`
	Calendar         CalendarInfo      `json:"calendar"`
	Rows             map[string]int    `json:"rows"`
	Fraud            map[string]int    `json:"fraud"`
	Files            map[string]string `json:"files"`
}

// CalendarInfo documents the split in features.json.
type CalendarInfo struct {
	Origin          int64  `json:"origin"`
	SecondsPerMonth int64  `json:"seconds_per_month"`
	TrainMonths     []int  `json:"train_months"`
	ValidMonths     []int  `json:"valid_months"`
	TestMonths      []int  `json:"test_months"`
	Rule            string `json:"rule"`
}

func newState(kind string) (features.State, error) {
	switch kind {
	case "exact":
		return features.NewExact(0), nil
	case "bucketed":
		return features.NewBucketed(features.BucketedConfig{}), nil
	case "sketch":
		return features.NewSketch(features.SketchConfig{}), nil
	}
	return nil, fmt.Errorf("unknown -state %q (want exact, bucketed, or sketch)", kind)
}

// modelFeatures is every catalog field except risk_score, which the model
// produces rather than consumes.
func modelFeatures(cat *schema.Catalog) []string {
	var out []string
	for _, f := range cat.Fields() {
		if f.Name != schema.RiskScoreField.Name {
			out = append(out, f.Name)
		}
	}
	return out
}

func run(cfg config, log io.Writer) (summary, error) {
	var sum summary
	t0 := time.Now()
	ds, err := data.Load(cfg.paths)
	if err != nil {
		return sum, err
	}
	sum.load = time.Since(t0)
	sum.synthetic = ds.Synthetic
	if ds.Synthetic {
		fmt.Fprintln(log, "*** SYNTHETIC DATA: every file written by this run is labelled SYNTHETIC. Not a result. ***")
	}
	fmt.Fprintf(log, "%sloaded %d transactions (cache: %v) in %v\n", sum.label(), len(ds.Txns), ds.FromCache, sum.load.Round(time.Millisecond))

	cat := schema.Default()
	names := modelFeatures(cat)

	// Pass 1: replay the train months only and collect the category values
	// the encoder may know. Train months are a prefix of the sorted data.
	t0 = time.Now()
	st, err := newState(cfg.state)
	if err != nil {
		return sum, err
	}
	eng, err := features.NewEngine(cat, st)
	if err != nil {
		return sum, err
	}
	nTrain := 0
	for nTrain < len(ds.Txns) && ds.Calendar.Split(ds.Txns[nTrain].DT) == data.Train {
		nTrain++
	}
	seen := map[string]map[string]struct{}{}
	var strSlots []int
	var strSets []map[string]struct{}
	for _, name := range names {
		if f := cat.MustLookup(name); f.Kind == schema.String {
			seen[name] = map[string]struct{}{}
			strSlots = append(strSlots, f.Slot)
			strSets = append(strSets, seen[name])
		}
	}
	err = features.Replay(eng, ds.Txns[:nTrain], func(_ *data.Txn, row schema.Row) error {
		for i, slot := range strSlots {
			strSets[i][row.Str[slot]] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return sum, err
	}
	enc, err := schema.FitEncoder(cat, names, seen)
	if err != nil {
		return sum, err
	}
	sum.fit = time.Since(t0)

	if err := os.MkdirAll(cfg.out, 0o755); err != nil {
		return sum, err
	}
	marker := filepath.Join(cfg.out, data.SyntheticMarker)
	if ds.Synthetic {
		if err := os.WriteFile(marker, []byte("SYNTHETIC export: built from cmd/synth data, not IEEE-CIS. Not a result.\n"), 0o644); err != nil {
			return sum, err
		}
	} else if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		return sum, err
	}
	if err := enc.Save(filepath.Join(cfg.out, encoderJSON)); err != nil {
		return sum, err
	}

	// Pass 2: the full replay on a fresh state, written as it goes.
	t0 = time.Now()
	st, _ = newState(cfg.state)
	eng, err = features.NewEngine(cat, st)
	if err != nil {
		return sum, err
	}
	if err := writeExport(cfg.out, ds, eng, enc, &sum); err != nil {
		return sum, err
	}
	sum.write = time.Since(t0)

	m := manifest(enc, ds, cfg.state, &sum)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return sum, err
	}
	if err := os.WriteFile(filepath.Join(cfg.out, manifestJSON), append(b, '\n'), 0o644); err != nil {
		return sum, err
	}
	for s := range sum.rows {
		fmt.Fprintf(log, "%s%-5s %7d rows, %6d fraud\n", sum.label(), data.Split(s), sum.rows[s], sum.fraud[s])
	}
	fmt.Fprintf(log, "%swrote %s (%d model inputs, %d categorical), %s, %s, %s, %s (%d test payments) to %s\n",
		sum.label(), exportCSV, sum.featureCount, sum.cat, encoderJSON, manifestJSON, backtestTable, replayJSONL, sum.replayed, cfg.out)
	return sum, nil
}

func writeExport(dir string, ds *data.Dataset, eng *features.Engine, enc *schema.Encoder, sum *summary) error {
	csvFile, err := os.Create(filepath.Join(dir, exportCSV))
	if err != nil {
		return err
	}
	defer csvFile.Close()
	jsonFile, err := os.Create(filepath.Join(dir, replayJSONL))
	if err != nil {
		return err
	}
	defer jsonFile.Close()
	cw := bufio.NewWriterSize(csvFile, 1<<20)
	jw := bufio.NewWriterSize(jsonFile, 1<<20)
	jenc := json.NewEncoder(jw)

	var line []byte
	for _, c := range idColumns {
		line = append(append(line, c...), ',')
	}
	for i, f := range enc.Features {
		if i > 0 {
			line = append(line, ',')
		}
		line = append(line, f...)
	}
	line = append(line, '\n')
	if _, err := cw.Write(line); err != nil {
		return err
	}

	// The backtester's columnar table gets the unencoded rows of the same
	// replay; risk_score stays NaN until a trained model fills it.
	tb := backtest.NewBuilder(eng.Catalog(), len(ds.Txns))
	vec := make([]float64, len(enc.Features))
	err = features.Replay(eng, ds.Txns, func(t *data.Txn, row schema.Row) error {
		tb.Append(row, backtest.Meta{ID: t.ID, DT: t.DT, Amount: t.Amount, Fraud: t.IsFraud})
		month := ds.Calendar.Month(t.DT)
		split := data.SplitOfMonth(month)
		sum.rows[split]++
		if t.IsFraud == 1 {
			sum.fraud[split]++
		}
		enc.Encode(row, vec)
		line = line[:0]
		line = strconv.AppendInt(line, t.ID, 10)
		line = append(line, ',')
		line = strconv.AppendInt(line, t.DT, 10)
		line = append(line, ',')
		line = append(line, split.String()...)
		line = append(line, ',')
		line = strconv.AppendInt(line, int64(month), 10)
		line = append(line, ',')
		line = strconv.AppendInt(line, int64(t.IsFraud), 10)
		for _, v := range vec {
			line = append(line, ',')
			if !math.IsNaN(v) {
				line = strconv.AppendFloat(line, v, 'g', -1, 64)
			}
		}
		line = append(line, '\n')
		if _, err := cw.Write(line); err != nil {
			return err
		}
		if split == data.Test {
			sum.replayed++
			return jenc.Encode(data.NewReplayEvent(t))
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, w := range []*bufio.Writer{cw, jw} {
		if err := w.Flush(); err != nil {
			return err
		}
	}
	if err := tb.Table().Save(filepath.Join(dir, backtestTable)); err != nil {
		return err
	}
	if err := jsonFile.Close(); err != nil {
		return err
	}
	return csvFile.Close()
}

func manifest(enc *schema.Encoder, ds *data.Dataset, state string, sum *summary) Manifest {
	m := Manifest{
		Synthetic: ds.Synthetic,
		Note:      "Real IEEE-CIS data.",
		State:     state,
		Label:     "isFraud",
		IDColumns: idColumns,
		Features:  enc.Features,
		Calendar: CalendarInfo{
			Origin:          ds.Calendar.Origin,
			SecondsPerMonth: data.SecondsPerMonth,
			TrainMonths:     []int{0, 1, 2, 3},
			ValidMonths:     []int{4},
			TestMonths:      []int{5},
			Rule: "month = floor((TransactionDT - origin) / seconds_per_month), origin = start of the first " +
				"transaction's day; a partial seventh month is folded into month 5 (test)",
		},
		Rows:  map[string]int{},
		Fraud: map[string]int{},
		Files: map[string]string{
			"export_csv": exportCSV, "encoder": encoderJSON, "test_replay": replayJSONL,
			"backtest_table": backtestTable,
		},
	}
	if ds.Synthetic {
		m.Note = "SYNTHETIC data from cmd/synth. Nothing computed from it is a result."
	}
	raw := map[string]bool{}
	for _, f := range schema.RawFields {
		raw[f.Name] = true
	}
	for i, name := range enc.Features {
		if enc.IsCategorical(i) {
			m.Categorical = append(m.Categorical, name)
		}
		if raw[name] {
			m.RawFeatures = append(m.RawFeatures, name)
		} else {
			m.VelocityFeatures = append(m.VelocityFeatures, name)
		}
	}
	sum.featureCount, sum.cat = len(enc.Features), len(m.Categorical)
	for s := range sum.rows {
		m.Rows[data.Split(s).String()] = sum.rows[s]
		m.Fraud[data.Split(s).String()] = sum.fraud[s]
	}
	return m
}
