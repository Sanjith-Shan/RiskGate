package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/features"
	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
	"github.com/Sanjith-Shan/RiskGate/internal/service"
)

// Leading columns of export.csv (cmd/export), before the model inputs.
var exportIDColumns = []string{"TransactionID", "TransactionDT", "split", "month", "isFraud"}

// splitStats counts one split's rows. A "differ" count is rows with at
// least one difference. Floats are equal when their bits are equal or both
// are missing (NaN); JSON has no NaN payloads to compare.
type splitStats struct {
	Rows int `json:"rows_compared"`
	// Service's logged catalog row (every field but risk_score) against the
	// offline replay's row.
	FeatureRowsDiffer int `json:"feature_rows_differ"`
	// The logged row, encoded with the export's encoder, against the
	// export.csv model-input vector.
	EncodedRowsDiffer int `json:"encoded_rows_differ_from_export_csv"`
	// Logged raw_score against the Go model on the export.csv vector.
	RawScoreDiffer int `json:"raw_score_differ"`
	// Logged probability and risk_score against the offline Scorer on the
	// offline row.
	ProbabilityDiffer int `json:"probability_differ"`
	RiskScoreDiffer   int `json:"risk_score_differ"`
	// Sanity check of the reference itself: the offline replay row encoded
	// against export.csv.
	OfflineVsExportDiffer int     `json:"offline_replay_vs_export_csv_differ"`
	MaxAbsFeatureDiff     float64 `json:"max_abs_feature_diff"`
	MaxAbsRawScoreDiff    float64 `json:"max_abs_raw_score_diff"`
}

func (s *splitStats) bad() bool {
	return s.FeatureRowsDiffer+s.EncodedRowsDiffer+s.RawScoreDiffer+s.ProbabilityDiffer+s.RiskScoreDiffer+s.OfflineVsExportDiffer > 0
}

type compareReport struct {
	DecisionLog      string                 `json:"decision_log"`
	LogLines         int                    `json:"decision_log_lines"`
	OfflineRows      int                    `json:"offline_rows"`
	MissingFromLog   int                    `json:"offline_rows_missing_from_log"`
	ExtraInLog       int                    `json:"log_lines_not_matched"`
	UnscoredInLog    int                    `json:"unscored_log_lines"`
	CatalogFields    int                    `json:"catalog_fields_compared"`
	ModelInputs      int                    `json:"model_inputs_compared"`
	All              splitStats             `json:"all"`
	BySplit          map[string]*splitStats `json:"by_split"`
	FieldMismatches  map[string]int         `json:"field_mismatch_rows"`
	EncoderIdentical bool                   `json:"model_encoder_equals_export_encoder"`
	Took             string                 `json:"took"`
	OK               bool                   `json:"ok"`
}

func compare(args []string, stdout io.Writer) (bool, error) {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	logPath := fs.String("log", "var/exp2/decisions.jsonl", "decision log the service wrote during `send`")
	dataDir := fs.String("data", "data", "data directory the export was built from")
	exportDir := fs.String("export", "data/export_real", "cmd/export output (export.csv, encoder.json)")
	modelDir := fs.String("model", "models/ieee", "model directory the service ran")
	jsonOut := fs.String("json", "", "write the aggregate report as JSON here")
	examples := fs.Int("examples", 10, "row-level mismatch examples printed to stderr (never written to -json)")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	start := time.Now()
	cat := schema.Default()
	scorer, err := model.LoadScorer(*modelDir, cat)
	if err != nil {
		return false, err
	}
	enc, err := schema.LoadEncoder(filepath.Join(*exportDir, "encoder.json"), cat)
	if err != nil {
		return false, err
	}
	menc, err := schema.LoadEncoder(filepath.Join(*modelDir, model.EncoderFile), cat)
	if err != nil {
		return false, err
	}
	ds, err := data.Load(data.RealPaths(*dataDir))
	if err != nil {
		return false, err
	}
	if ds.Synthetic {
		return false, errors.New("refusing: the dataset is SYNTHETIC")
	}
	lf, err := os.Open(*logPath)
	if err != nil {
		return false, err
	}
	defer lf.Close()
	ef, err := os.Open(filepath.Join(*exportDir, "export.csv"))
	if err != nil {
		return false, err
	}
	defer ef.Close()

	c, err := newComparer(cat, scorer, enc, lf, ef, *examples)
	if err != nil {
		return false, err
	}
	c.rep.DecisionLog = *logPath
	c.rep.EncoderIdentical = slices.Equal(enc.Features, menc.Features) && encodersEqual(enc, menc)

	engine, err := features.NewEngine(cat, features.NewExact(0))
	if err != nil {
		return false, err
	}
	if err := features.Replay(engine, ds.Txns, func(t *data.Txn, row schema.Row) error {
		return c.row(t, row, ds.Calendar.Split(t.DT).String())
	}); err != nil {
		return false, err
	}
	if err := c.finish(); err != nil {
		return false, err
	}
	rep := c.rep
	rep.Took = time.Since(start).Round(time.Millisecond).String()
	rep.OK = !rep.All.bad() && rep.MissingFromLog == 0 && rep.ExtraInLog == 0 && rep.UnscoredInLog == 0 && rep.All.Rows == rep.OfflineRows

	fmt.Fprintf(stdout, "offline rows %d, log lines %d, compared %d (missing from log %d, unmatched log lines %d)\n",
		rep.OfflineRows, rep.LogLines, rep.All.Rows, rep.MissingFromLog, rep.ExtraInLog)
	for _, name := range []string{"all", "train", "valid", "test"} {
		s := &rep.All
		if name != "all" {
			if s = rep.BySplit[name]; s == nil {
				continue
			}
		}
		fmt.Fprintf(stdout, "%-5s rows %7d: features differ %d, encoded vs export.csv differ %d, raw score differ %d, probability differ %d, risk_score differ %d, offline vs export %d; max |feature diff| %g, max |raw diff| %g\n",
			name, s.Rows, s.FeatureRowsDiffer, s.EncodedRowsDiffer, s.RawScoreDiffer, s.ProbabilityDiffer, s.RiskScoreDiffer, s.OfflineVsExportDiffer, s.MaxAbsFeatureDiff, s.MaxAbsRawScoreDiff)
	}
	for f, n := range rep.FieldMismatches {
		fmt.Fprintf(stdout, "  field %s: %d rows differ\n", f, n)
	}
	if rep.OK {
		fmt.Fprintln(stdout, "OK: every row bit-identical")
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o644); err != nil {
			return false, err
		}
	}
	return rep.OK, nil
}

func encodersEqual(a, b *schema.Encoder) bool {
	if len(a.Categorical) != len(b.Categorical) {
		return false
	}
	for k, v := range a.Categorical {
		if !slices.Equal(v, b.Categorical[k]) {
			return false
		}
	}
	return true
}

type comparer struct {
	cat      *schema.Catalog
	scorer   *model.Scorer
	enc      *schema.Encoder
	riskSlot int

	log     *bufio.Reader
	logEOF  bool
	pending *service.LogEntry
	csv     *csv.Reader
	csvCols int

	vecExport, vecLogged, vecOffline []float64
	examples                         int
	rep                              compareReport
}

func newComparer(cat *schema.Catalog, sc *model.Scorer, enc *schema.Encoder, logR, csvR io.Reader, examples int) (*comparer, error) {
	c := &comparer{
		cat: cat, scorer: sc, enc: enc, riskSlot: cat.MustLookup(schema.RiskScoreField.Name).Slot,
		log: bufio.NewReaderSize(logR, 1<<20), csv: csv.NewReader(bufio.NewReaderSize(csvR, 1<<20)),
		vecExport: make([]float64, len(enc.Features)), vecLogged: make([]float64, len(enc.Features)),
		vecOffline: make([]float64, len(enc.Features)), examples: examples,
	}
	c.csv.ReuseRecord = true
	header, err := c.csv.Read()
	if err != nil {
		return nil, fmt.Errorf("export.csv: %w", err)
	}
	want := append(slices.Clone(exportIDColumns), enc.Features...)
	if !slices.Equal(header, want) {
		return nil, errors.New("export.csv header is not the id columns followed by the encoder's features")
	}
	if !slices.Equal(sc.FeatureNames(), enc.Features) {
		return nil, errors.New("model features differ from the export encoder's")
	}
	c.csvCols = len(header)
	c.rep.BySplit = map[string]*splitStats{}
	c.rep.FieldMismatches = map[string]int{}
	c.rep.CatalogFields = len(cat.Fields()) - 1 // risk_score is checked as a score
	c.rep.ModelInputs = len(enc.Features)
	return c, nil
}

// next returns the next unconsumed log entry, or nil at the end.
func (c *comparer) next() (*service.LogEntry, error) {
	if c.pending != nil || c.logEOF {
		return c.pending, nil
	}
	for {
		b, err := c.log.ReadBytes('\n')
		if len(b) > 0 && (err == nil || json.Valid(b)) {
			var e service.LogEntry
			if uerr := json.Unmarshal(b, &e); uerr != nil {
				return nil, fmt.Errorf("decision log line %d: %w", c.rep.LogLines+1, uerr)
			}
			c.rep.LogLines++
			c.pending = &e
			return c.pending, nil
		}
		if errors.Is(err, io.EOF) {
			c.logEOF = true
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (c *comparer) example(format string, a ...any) {
	if c.examples > 0 {
		c.examples--
		fmt.Fprintf(os.Stderr, format+"\n", a...)
	}
}

func eq(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b) || (math.IsNaN(a) && math.IsNaN(b))
}

func absDiff(a, b float64) float64 {
	if math.IsNaN(a) || math.IsNaN(b) {
		if math.IsNaN(a) && math.IsNaN(b) {
			return 0
		}
		return math.Inf(1)
	}
	return math.Abs(a - b)
}

func (c *comparer) row(t *data.Txn, off schema.Row, split string) error {
	c.rep.OfflineRows++
	// export.csv advances with the offline replay: same rows, same order.
	rec, err := c.csv.Read()
	if err != nil {
		return fmt.Errorf("export.csv row %d: %w", c.rep.OfflineRows, err)
	}
	if id, err := strconv.ParseInt(rec[0], 10, 64); err != nil || id != t.ID {
		return fmt.Errorf("export.csv row %d is TransactionID %s, offline replay has %d", c.rep.OfflineRows, rec[0], t.ID)
	}
	for i := range c.vecExport {
		s := rec[len(exportIDColumns)+i]
		if s == "" {
			c.vecExport[i] = math.NaN()
		} else if c.vecExport[i], err = strconv.ParseFloat(s, 64); err != nil {
			return fmt.Errorf("export.csv row %d: %w", c.rep.OfflineRows, err)
		}
	}

	e, err := c.next()
	if err != nil {
		return err
	}
	want := data.PaymentIDPrefix + strconv.FormatInt(t.ID, 10)
	if e == nil || e.PaymentID != want {
		c.rep.MissingFromLog++ // the log is a subsequence of the replay; keep e pending
		return nil
	}
	c.pending = nil
	s := c.rep.BySplit[split]
	if s == nil {
		s = &splitStats{}
		c.rep.BySplit[split] = s
	}
	var st splitStats
	st.Rows = 1

	if e.Created != data.ReplayEpochUnix+t.DT {
		c.rep.FieldMismatches["created"]++
		st.FeatureRowsDiffer = 1
	}
	logged, err := e.Row(c.cat)
	if err != nil {
		return fmt.Errorf("decision log line %d: %w", c.rep.LogLines, err)
	}
	// Full catalog vector, every field but the model's own output.
	featBad := false
	for _, f := range c.cat.Fields() {
		if f.Slot == c.riskSlot && f.Kind == schema.Number {
			continue
		}
		same := true
		if f.Kind == schema.Number {
			a, b := logged.Num[f.Slot], off.Num[f.Slot]
			if !eq(a, b) {
				same = false
				st.MaxAbsFeatureDiff = max(st.MaxAbsFeatureDiff, absDiff(a, b))
				c.example("TransactionID %d %s: service %v, offline %v", t.ID, f.Name, a, b)
			}
		} else if logged.Str[f.Slot] != off.Str[f.Slot] {
			same = false
			c.example("TransactionID %d %s: service %q, offline %q", t.ID, f.Name, logged.Str[f.Slot], off.Str[f.Slot])
		}
		if !same {
			featBad = true
			c.rep.FieldMismatches[f.Name]++
		}
	}
	if featBad {
		st.FeatureRowsDiffer = 1
	}

	// Encoded model inputs against export.csv, and the reference itself.
	c.enc.Encode(logged, c.vecLogged)
	c.enc.Encode(off, c.vecOffline)
	for i := range c.vecExport {
		if !eq(c.vecLogged[i], c.vecExport[i]) {
			st.EncodedRowsDiffer = 1
		}
		if !eq(c.vecOffline[i], c.vecExport[i]) {
			st.OfflineVsExportDiffer = 1
		}
	}

	// Scores.
	if !e.Scored || e.RawScore == nil || e.Probability == nil {
		c.rep.UnscoredInLog++
		st.RawScoreDiffer, st.ProbabilityDiffer, st.RiskScoreDiffer = 1, 1, 1
	} else {
		raw := c.scorer.Model().PredictRaw(c.vecExport)
		if !eq(*e.RawScore, raw) {
			st.RawScoreDiffer = 1
			st.MaxAbsRawScoreDiff = absDiff(*e.RawScore, raw)
			c.example("TransactionID %d raw score: service %v, offline %v", t.ID, *e.RawScore, raw)
		}
		off.Num[c.riskSlot] = math.NaN()
		risk, prob, _ := c.scorer.Score(off)
		if !eq(*e.Probability, prob) {
			st.ProbabilityDiffer = 1
		}
		if risk != e.RiskScore || !eq(logged.Num[c.riskSlot], float64(risk)) {
			st.RiskScoreDiffer = 1
		}
	}
	add(s, &st)
	add(&c.rep.All, &st)
	return nil
}

func add(dst, src *splitStats) {
	dst.Rows += src.Rows
	dst.FeatureRowsDiffer += src.FeatureRowsDiffer
	dst.EncodedRowsDiffer += src.EncodedRowsDiffer
	dst.RawScoreDiffer += src.RawScoreDiffer
	dst.ProbabilityDiffer += src.ProbabilityDiffer
	dst.RiskScoreDiffer += src.RiskScoreDiffer
	dst.OfflineVsExportDiffer += src.OfflineVsExportDiffer
	dst.MaxAbsFeatureDiff = max(dst.MaxAbsFeatureDiff, src.MaxAbsFeatureDiff)
	dst.MaxAbsRawScoreDiff = max(dst.MaxAbsRawScoreDiff, src.MaxAbsRawScoreDiff)
}

// finish counts log lines that matched no offline row, and checks
// export.csv has no rows the replay did not produce.
func (c *comparer) finish() error {
	for {
		e, err := c.next()
		if err != nil {
			return err
		}
		if e == nil {
			break
		}
		c.rep.ExtraInLog++
		c.pending = nil
	}
	if _, err := c.csv.Read(); !errors.Is(err, io.EOF) {
		return errors.New("export.csv has more rows than the offline replay")
	}
	return nil
}
