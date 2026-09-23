package model

import (
	"bufio"
	"compress/gzip"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Fixtures are written by python/gen_fixtures.py (run it to regenerate). Each holds a LightGBM model
// and LightGBM's raw scores for a set of inputs.
var fixtures = []string{
	"nan_missing",       // NaN missing type, default left and default right
	"zero_missing",      // zero_as_missing: Zero missing type, NaN folded to 0
	"no_missing",        // use_missing=false: NaN treated as 0
	"categorical",       // multi-word bitsets, one-hot splits, unseen/negative/NaN categories
	"single_leaf",       // a model that is one constant tree
	"single_leaf_mixed", // constant trees between ordinary ones
	"large",             // 500 trees, 63 leaves, 55 features: the benchmark model
}

// open returns name or name.gz from dir, whichever exists.
func open(t testing.TB, dir, name string) io.ReadCloser {
	t.Helper()
	p := filepath.Join("testdata", dir, name)
	if f, err := os.Open(p); err == nil {
		return f
	}
	f, err := os.Open(p + ".gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	return struct {
		io.Reader
		io.Closer
	}{zr, f}
}

func loadFixture(t testing.TB, name string) (*Model, [][]float64, []float64) {
	t.Helper()
	r := open(t, name, "model.txt")
	defer r.Close()
	m, err := Parse(r)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	rows, want := readCSV(t, name)
	return m, rows, want
}

// readCSV reads cases.csv: input columns, then the expected value.
func readCSV(t testing.TB, name string) ([][]float64, []float64) {
	t.Helper()
	r := open(t, name, "cases.csv")
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	sc.Scan() // header
	var rows [][]float64
	var want []float64
	for sc.Scan() {
		cells := strings.Split(sc.Text(), ",")
		vals := make([]float64, len(cells))
		for i, c := range cells {
			v, err := strconv.ParseFloat(c, 64)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			vals[i] = v
		}
		rows = append(rows, vals[:len(vals)-1])
		want = append(want, vals[len(vals)-1])
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows, want
}

// TestParityWithLightGBM is experiment 2's model half on the fixtures: every
// row's raw score must have exactly LightGBM's bits.
func TestParityWithLightGBM(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			m, rows, want := loadFixture(t, name)
			var maxDiff float64
			exact := 0
			for i, x := range rows {
				got := m.PredictRaw(x)
				if math.Float64bits(got) == math.Float64bits(want[i]) {
					exact++
					continue
				}
				maxDiff = max(maxDiff, math.Abs(got-want[i]))
				if exact+10 > i { // report the first few only
					t.Errorf("row %d: got %v, LightGBM %v", i, got, want[i])
				}
			}
			t.Logf("%d trees, %d rows, %d bit-identical, max |diff| %g", m.NumTrees(), len(rows), exact, maxDiff)
			if exact != len(rows) {
				t.Fatalf("%d of %d rows differ from LightGBM", len(rows)-exact, len(rows))
			}
		})
	}
}

func TestSaabasSumsToRawScore(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			m, rows, _ := loadFixture(t, name)
			out := make([]float64, m.NumFeatures())
			for i, x := range rows {
				bias := m.Contributions(x, out)
				sum := bias
				for _, c := range out {
					sum += c
				}
				if raw := m.PredictRaw(x); math.Abs(sum-raw) > 1e-9 {
					t.Fatalf("row %d: bias+contributions = %v, raw = %v", i, sum, raw)
				}
			}
		})
	}
}

func TestCalibratorMatchesPython(t *testing.T) {
	c, err := LoadCalibrator(filepath.Join("testdata", "calibration", "calibration.json"))
	if err != nil {
		t.Fatal(err)
	}
	rows, want := readCSV(t, "calibration")
	for i, x := range rows {
		got := c.Apply(x[0])
		if math.Float64bits(got) != math.Float64bits(want[i]) && !(math.IsNaN(got) && math.IsNaN(want[i])) {
			t.Errorf("Apply(%v) = %v, np.interp = %v", x[0], got, want[i])
		}
	}
	t.Logf("%d knots, %d rows bit-identical to riskgate.interpolate", len(c.X), len(rows))
}

func TestRiskScore(t *testing.T) {
	for _, tc := range []struct {
		p    float64
		want int
	}{
		{0, 0}, {0.0099, 0}, {0.01, 1}, {0.2, 20}, {0.999, 99}, {1, 99},
		{-0.1, 0}, {1.5, 99}, {math.NaN(), 0},
	} {
		if got := RiskScore(tc.p); got != tc.want {
			t.Errorf("RiskScore(%v) = %d, want %d", tc.p, got, tc.want)
		}
	}
}

func TestCalibratorRejectsBadKnots(t *testing.T) {
	for _, c := range []Calibrator{
		{Method: "platt", Input: "raw_score", X: []float64{0}, Y: []float64{0}},
		{Method: "isotonic", Input: "probability", X: []float64{0}, Y: []float64{0}},
		{Method: "isotonic", Input: "raw_score"},
		{Method: "isotonic", Input: "raw_score", X: []float64{0, 0}, Y: []float64{0, 1}},
		{Method: "isotonic", Input: "raw_score", X: []float64{0, 1}, Y: []float64{1, 0}},
		{Method: "isotonic", Input: "raw_score", X: []float64{0, 1}, Y: []float64{0, 2}},
	} {
		if err := c.validate(); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	one := Calibrator{X: []float64{0}, Y: []float64{0.3}}
	if got := one.Apply(-5); got != 0.3 {
		t.Errorf("single knot: got %v", got)
	}
}

func TestPredictDoesNotAllocate(t *testing.T) {
	m, rows, _ := loadFixture(t, "large")
	out := make([]float64, m.NumFeatures())
	x := rows[0]
	if n := testing.AllocsPerRun(100, func() { m.PredictRaw(x) }); n != 0 {
		t.Errorf("PredictRaw: %v allocs", n)
	}
	if n := testing.AllocsPerRun(100, func() { m.Contributions(x, out) }); n != 0 {
		t.Errorf("Contributions: %v allocs", n)
	}
}

func TestPredictPanicsOnWrongWidth(t *testing.T) {
	m, _, _ := loadFixture(t, "no_missing")
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	m.PredictRaw(make([]float64, m.NumFeatures()+1))
}

// BenchmarkPredictRaw scores the 500-tree, 63-leaf, 55-feature model.
func BenchmarkPredictRaw(b *testing.B) {
	m, rows, _ := loadFixture(b, "large")
	b.ReportAllocs()
	var sink float64
	i := 0
	for b.Loop() {
		sink += m.PredictRaw(rows[i])
		if i++; i == len(rows) {
			i = 0
		}
	}
	_ = sink
}

func BenchmarkContributions(b *testing.B) {
	m, rows, _ := loadFixture(b, "large")
	out := make([]float64, m.NumFeatures())
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		m.Contributions(rows[i], out)
		if i++; i == len(rows) {
			i = 0
		}
	}
}

func BenchmarkParse(b *testing.B) {
	r := open(b, "large", "model.txt")
	text, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		if _, err := Parse(strings.NewReader(string(text))); err != nil {
			b.Fatal(err)
		}
	}
}
