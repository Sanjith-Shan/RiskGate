// Command parity checks RiskGate's Go model evaluator against LightGBM on
// every row of an exported month: it recomputes each raw score from the
// exported feature vector and compares it with the score LightGBM wrote
// (python/parity.py). The target is bit-identical on every row.
//
//	go run ./cmd/parity -model models/current -export data/export/export.csv \
//	    -scores results/parity_scores.csv
//
// It exits 1 if any row differs or any scored row is missing from the export.
package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Sanjith-Shan/RiskGate/internal/model"
)

type report struct {
	Model       string  `json:"model"`
	Trees       int     `json:"trees"`
	Rows        int     `json:"rows_checked"`
	Exact       int     `json:"bit_identical"`
	MaxAbsDiff  float64 `json:"max_abs_diff"`
	MissingRows int     `json:"scored_rows_missing_from_export"`
}

func main() {
	modelDir := flag.String("model", "models/current", "model directory holding model.txt")
	export := flag.String("export", "", "export CSV written by cmd/export")
	scores := flag.String("scores", "", "LightGBM raw scores written by python/parity.py")
	jsonOut := flag.String("json", "", "also write the report as JSON to this file")
	flag.Parse()
	if *export == "" || *scores == "" {
		flag.Usage()
		os.Exit(2)
	}
	r, err := run(filepath.Join(*modelDir, model.ModelFile), *export, *scores)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parity:", err)
		os.Exit(2)
	}
	fmt.Printf("model %s: %d trees\n", r.Model, r.Trees)
	fmt.Printf("rows checked:   %d\n", r.Rows)
	fmt.Printf("bit-identical:  %d\n", r.Exact)
	fmt.Printf("max |go - lgb|: %g\n", r.MaxAbsDiff)
	if r.MissingRows > 0 {
		fmt.Printf("scored rows missing from export: %d\n", r.MissingRows)
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "parity:", err)
			os.Exit(2)
		}
	}
	if r.Exact != r.Rows || r.MissingRows > 0 || r.Rows == 0 {
		os.Exit(1)
	}
}

func run(modelPath, exportPath, scoresPath string) (report, error) {
	m, err := model.Load(modelPath)
	if err != nil {
		return report{}, err
	}
	want, err := readScores(scoresPath)
	if err != nil {
		return report{}, err
	}
	r := report{Model: modelPath, Trees: m.NumTrees()}

	f, err := os.Open(exportPath)
	if err != nil {
		return r, err
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.ReuseRecord = true
	header, err := cr.Read()
	if err != nil {
		return r, fmt.Errorf("%s: %w", exportPath, err)
	}
	col := map[string]int{}
	for i, h := range header {
		if _, dup := col[h]; !dup { // a repeated column (amount) holds the same values
			col[h] = i
		}
	}
	idCol, ok := col["TransactionID"]
	if !ok {
		return r, fmt.Errorf("%s: no TransactionID column", exportPath)
	}
	idx := make([]int, m.NumFeatures())
	for i, name := range m.FeatureNames() {
		if idx[i], ok = col[name]; !ok {
			return r, fmt.Errorf("%s: no column for model feature %q", exportPath, name)
		}
	}

	x := make([]float64, m.NumFeatures())
	seen := 0
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return r, fmt.Errorf("%s: %w", exportPath, err)
		}
		id, err := strconv.ParseInt(rec[idCol], 10, 64)
		if err != nil {
			return r, fmt.Errorf("%s:%d: TransactionID: %w", exportPath, line, err)
		}
		lgbRaw, ok := want[id]
		if !ok {
			continue // another month
		}
		seen++
		for i, c := range idx {
			if rec[c] == "" {
				x[i] = math.NaN()
			} else if x[i], err = strconv.ParseFloat(rec[c], 64); err != nil {
				return r, fmt.Errorf("%s:%d: %s: %w", exportPath, line, header[c], err)
			}
		}
		got := m.PredictRaw(x)
		r.Rows++
		if math.Float64bits(got) == math.Float64bits(lgbRaw) {
			r.Exact++
		} else {
			r.MaxAbsDiff = max(r.MaxAbsDiff, math.Abs(got-lgbRaw))
			if r.Rows-r.Exact <= 5 {
				fmt.Fprintf(os.Stderr, "TransactionID %d: go %v, lightgbm %v\n", id, got, lgbRaw)
			}
		}
	}
	r.MissingRows = len(want) - seen
	return r, nil
}

func readScores(path string) (map[int64]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cr := csv.NewReader(f)
	if _, err := cr.Read(); err != nil { // TransactionID,raw_score
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[int64]float64{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		id, err1 := strconv.ParseInt(rec[0], 10, 64)
		v, err2 := strconv.ParseFloat(rec[1], 64)
		if err := errors.Join(err1, err2); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out[id] = v
	}
}
