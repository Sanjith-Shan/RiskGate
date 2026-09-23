package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeInputs turns a model fixture's cases.csv into an export CSV and a
// scores CSV. If perturb is set, one score is replaced by a wrong one.
func writeInputs(t *testing.T, perturb bool) (export, scores string) {
	t.Helper()
	f, err := os.Open("../../internal/model/testdata/no_missing/cases.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dir := t.TempDir()
	var ex, sc strings.Builder
	s := bufio.NewScanner(f)
	s.Scan()
	cols := strings.Split(s.Text(), ",")
	nf := len(cols) - 1
	ex.WriteString("TransactionID,split")
	for i := range nf {
		fmt.Fprintf(&ex, ",Column_%d", i)
	}
	ex.WriteString("\n")
	sc.WriteString("TransactionID,raw_score\n")
	for id := 1; s.Scan(); id++ {
		cells := strings.Split(s.Text(), ",")
		for i, c := range cells[:nf] {
			if c == "nan" {
				cells[i] = "" // the export writes missing as empty
			}
		}
		fmt.Fprintf(&ex, "%d,test,%s\n", id, strings.Join(cells[:nf], ","))
		want := cells[nf]
		if perturb && id == 3 {
			want = "12345.5"
		}
		fmt.Fprintf(&sc, "%d,%s\n", id, want)
	}
	export, scores = filepath.Join(dir, "export.csv"), filepath.Join(dir, "scores.csv")
	if err := os.WriteFile(export, []byte(ex.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scores, []byte(sc.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return export, scores
}

const fixtureModel = "../../internal/model/testdata/no_missing/model.txt"

func TestRunAllRowsMatch(t *testing.T) {
	export, scores := writeInputs(t, false)
	r, err := run(fixtureModel, export, scores)
	if err != nil {
		t.Fatal(err)
	}
	if r.Rows == 0 || r.Exact != r.Rows || r.MaxAbsDiff != 0 || r.MissingRows != 0 {
		t.Fatalf("report %+v", r)
	}
}

func TestRunReportsMismatch(t *testing.T) {
	export, scores := writeInputs(t, true)
	r, err := run(fixtureModel, export, scores)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exact != r.Rows-1 || r.MaxAbsDiff == 0 {
		t.Fatalf("want exactly one mismatch, got %+v", r)
	}
}
