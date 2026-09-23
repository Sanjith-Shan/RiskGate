package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/data/synth"
	"github.com/Sanjith-Shan/RiskGate/internal/features"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// TestExport runs the command on a small SYNTHETIC set and checks every
// output against an independent replay: the CSV must parse back to the exact
// bits the engine and encoder produce.
func TestExport(t *testing.T) {
	dir := t.TempDir()
	txns, _ := synth.Generate(synth.Config{Rows: 6000, Seed: 11})
	if err := synth.WriteCSV(filepath.Join(dir, "synth"), txns); err != nil {
		t.Fatal(err)
	}
	cfg := config{paths: data.SyntheticPaths(dir), out: filepath.Join(dir, "out"), state: "exact"}
	sum, err := run(cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := backtest.Load(filepath.Join(cfg.out, backtestTable), schema.Default())
	if err != nil {
		t.Fatal(err)
	}
	if tb.N != len(txns) {
		t.Errorf("backtest table has %d rows, want %d", tb.N, len(txns))
	}
	if !sum.synthetic {
		t.Error("synthetic input not reported as synthetic")
	}
	if _, err := os.Stat(filepath.Join(cfg.out, data.SyntheticMarker)); err != nil {
		t.Error("no SYNTHETIC marker in the output")
	}

	var m Manifest
	b, err := os.ReadFile(filepath.Join(cfg.out, manifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	cat := schema.Default()
	if !m.Synthetic || len(m.Features) != len(cat.Fields())-1 || m.Features[0] != "amount" {
		t.Fatalf("manifest: synthetic %v, %d features starting %q", m.Synthetic, len(m.Features), m.Features[0])
	}
	if len(m.RawFeatures) != len(schema.RawFields) || len(m.Categorical) != 7 ||
		len(m.RawFeatures)+len(m.VelocityFeatures) != len(m.Features) {
		t.Errorf("manifest groups: %d raw, %d velocity, %d categorical", len(m.RawFeatures), len(m.VelocityFeatures), len(m.Categorical))
	}

	enc, err := schema.LoadEncoder(filepath.Join(cfg.out, encoderJSON), cat)
	if err != nil {
		t.Fatal(err)
	}
	// The encoder knows only train-month categories.
	ds, err := data.Load(cfg.paths)
	if err != nil {
		t.Fatal(err)
	}
	trainDevices := map[string]bool{}
	for _, tx := range ds.Txns {
		if ds.Calendar.Split(tx.DT) == data.Train {
			trainDevices[tx.DeviceInfo] = true
		}
	}
	for _, v := range enc.Categorical["device_info"] {
		if !trainDevices[v] {
			t.Errorf("encoder knows device %q, which never appears in a train month", v)
		}
	}

	// Recompute independently and compare the CSV cell by cell.
	eng, err := features.NewEngine(cat, features.NewExact(0))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(cfg.out, exportCSV))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(bufio.NewReader(f))
	header, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	if want := append(append([]string{}, idColumns...), enc.Features...); len(header) != len(want) {
		t.Fatalf("header has %d columns, want %d", len(header), len(want))
	}
	vec := make([]float64, len(enc.Features))
	n := 0
	err = features.Replay(eng, ds.Txns, func(tx *data.Txn, row schema.Row) error {
		rec, err := r.Read()
		if err != nil {
			return err
		}
		month := ds.Calendar.Month(tx.DT)
		if rec[0] != strconv.FormatInt(tx.ID, 10) || rec[2] != data.SplitOfMonth(month).String() ||
			rec[3] != strconv.Itoa(month) || rec[4] != strconv.Itoa(int(tx.IsFraud)) {
			t.Fatalf("row %d leading columns %v", n, rec[:5])
		}
		enc.Encode(row, vec)
		for i, want := range vec {
			cell := rec[len(idColumns)+i]
			got := math.NaN()
			if cell != "" {
				if got, err = strconv.ParseFloat(cell, 64); err != nil {
					t.Fatal(err)
				}
			}
			if math.Float64bits(got) != math.Float64bits(want) && !(math.IsNaN(got) && math.IsNaN(want)) {
				t.Fatalf("row %d %s: file has %q, engine gives %v", n, enc.Features[i], cell, want)
			}
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(); err == nil {
		t.Error("the CSV has more rows than the dataset")
	}

	// The replay file is exactly the test month, and round-trips.
	byID := map[int64]data.Txn{}
	tests := 0
	for _, tx := range ds.Txns {
		byID[tx.ID] = tx
		if ds.Calendar.Split(tx.DT) == data.Test {
			tests++
		}
	}
	jf, err := os.Open(filepath.Join(cfg.out, replayJSONL))
	if err != nil {
		t.Fatal(err)
	}
	defer jf.Close()
	dec := json.NewDecoder(jf)
	lines := 0
	for dec.More() {
		var ev data.ReplayEvent
		if err := dec.Decode(&ev); err != nil {
			t.Fatal(err)
		}
		got, err := ev.Txn()
		if err != nil {
			t.Fatal(err)
		}
		want := byID[got.ID]
		if ds.Calendar.Split(want.DT) != data.Test || got.DT != want.DT || got.Amount != want.Amount ||
			got.DeviceInfo != want.DeviceInfo || got.IsFraud != want.IsFraud {
			t.Fatalf("replay line %d: got %+v, want %+v", lines, got, want)
		}
		lines++
	}
	if lines != tests || sum.replayed != tests {
		t.Errorf("replay file has %d lines, summary %d, test month %d", lines, sum.replayed, tests)
	}
}

func TestUnknownState(t *testing.T) {
	if _, err := newState("fast"); err == nil {
		t.Error("unknown state accepted")
	}
}
