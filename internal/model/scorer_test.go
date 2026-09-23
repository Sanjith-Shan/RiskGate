package model

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// handModel is small enough to check by hand. Tree 0 splits on
// card_txn_count_1h, then on purchaser_email_domain (code 0, anonymous.com,
// goes left); tree 1 splits on amount.
//
//	tree 0: root (value -2.5): count <= 4.5 (NaN left) ? leaf0 -3 : node1
//	        node1 (value -1.5): email in {0} ? leaf1 -1 : leaf2 -2
//	tree 1: root (value 0): amount <= 300 ? leaf0 -0.1 : leaf1 0.4
const handModel = `tree
version=v4
num_class=1
num_tree_per_iteration=1
label_index=0
max_feature_idx=2
objective=binary sigmoid:1
feature_names=amount purchaser_email_domain card_txn_count_1h
feature_infos=[0:1000] 0:1 [0:50]
tree_sizes=1 1

Tree=0
num_leaves=3
num_cat=1
split_feature=2 1
split_gain=1 1
threshold=4.5 0
decision_type=10 1
left_child=-1 -2
right_child=1 -3
leaf_value=-3 -1 -2
leaf_weight=1 1 1
leaf_count=1 1 1
internal_value=-2.5 -1.5
internal_weight=2 2
internal_count=2 2
cat_boundaries=0 1
cat_threshold=1
is_linear=0
shrinkage=1


Tree=1
num_leaves=2
num_cat=0
split_feature=0
split_gain=1
threshold=300
decision_type=2
left_child=-1
right_child=-2
leaf_value=-0.10000000000000001 0.40000000000000002
leaf_weight=1 1
leaf_count=1 1
internal_value=0
internal_weight=2
internal_count=2
is_linear=0
shrinkage=0.1


end of trees

feature_importances:
card_txn_count_1h=1

parameters:
[boosting: gbdt]
end of parameters
`

func writeModelDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]any{
		EncoderFile: map[string]any{
			"features":    []string{"amount", "purchaser_email_domain", "card_txn_count_1h"},
			"categorical": map[string][]string{"purchaser_email_domain": {"anonymous.com", "gmail.com"}},
		},
		CalibrationFile: Calibrator{Method: "isotonic", Input: "raw_score", X: []float64{-3, 0}, Y: []float64{0.01, 0.5}},
		MetadataFile: map[string]any{
			"feature_names": []string{"amount", "purchaser_email_domain", "card_txn_count_1h"},
			"feature_stats": map[string]Stat{"card_txn_count_1h": {P50: 0}, "amount": {P50: 68.5}},
			"data":          map[string]any{"synthetic": true},
		},
	}
	for name, v := range files {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ModelFile), []byte(handModel), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func row(c *schema.Catalog, amount float64, email string, count float64) schema.Row {
	r := c.NewRow()
	r.Num[c.MustLookup("amount").Slot] = amount
	r.Str[c.MustLookup("purchaser_email_domain").Slot] = email
	r.Num[c.MustLookup("card_txn_count_1h").Slot] = count
	return r
}

func TestScorerByHand(t *testing.T) {
	c := schema.Default()
	s, err := LoadScorer(writeModelDir(t), c)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Synthetic() {
		t.Error("metadata says synthetic")
	}

	r := row(c, 500, "anonymous.com", 9)
	score, p, raw := s.Score(r)
	// raw = leaf1 of tree 0 (-1) + leaf1 of tree 1 (0.4).
	if want := -1 + 0.40000000000000002; raw != want {
		t.Fatalf("raw = %v, want %v", raw, want)
	}
	if want := float64((0.5-0.01)/(0-(-3))*(raw-(-3))) + 0.01; p != want {
		t.Errorf("p = %v, want %v", p, want)
	}
	if score != 40 {
		t.Errorf("risk score = %d, want 40", score)
	}

	x := make([]float64, 3)
	contrib := make([]float64, 3)
	bias := s.Contributions(r, x, contrib)
	// Tree 0: count moves -2.5 -> -1.5 (+1), email moves -1.5 -> -1 (+0.5).
	// Tree 1: amount moves 0 -> 0.4 (+0.4). Bias is the root values, -2.5 + 0.
	if bias != -2.5 || contrib[2] != 1 || contrib[1] != 0.5 || contrib[0] != 0.40000000000000002 {
		t.Errorf("bias %v contributions %v", bias, contrib)
	}

	got := s.Explain(r)
	want := []string{
		"9 earlier payments on this card in the last hour (typical: 0)",
		`purchaser email domain is "anonymous.com"`,
		"amount is $500.00 (typical: $68.50)",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d reasons: %+v", len(got), got)
	}
	for i := range want {
		if got[i].Text != want[i] {
			t.Errorf("reason %d = %q, want %q", i, got[i].Text, want[i])
		}
	}

	// A low-risk payment: the count split sends it left, the only positive
	// contribution is none, so there are no reasons.
	if got := s.Explain(row(c, 20, "gmail.com", 1)); len(got) != 0 {
		t.Errorf("low-risk payment got reasons %+v", got)
	}
}

func TestScorerMissingAndUnseen(t *testing.T) {
	c := schema.Default()
	s, err := LoadScorer(writeModelDir(t), c)
	if err != nil {
		t.Fatal(err)
	}
	// Missing count takes the default (left) branch: leaf0, -3.
	if _, _, raw := s.Score(row(c, 10, "", math.NaN())); raw != -3+-0.10000000000000001 {
		t.Errorf("missing count: raw %v", raw)
	}
	// An unseen domain encodes as NaN, and NaN goes right at a categorical split.
	r := row(c, 10, "new-domain.biz", 9)
	if _, _, raw := s.Score(r); raw != -2+-0.10000000000000001 {
		t.Errorf("unseen domain: raw %v", raw)
	}
	if n := testing.AllocsPerRun(100, func() { s.Score(r) }); n != 0 {
		t.Errorf("Score allocates %v times", n)
	}
}

func TestScorerRejectsMismatchedFiles(t *testing.T) {
	c := schema.Default()
	dir := writeModelDir(t)
	enc := `{"features":["card_txn_count_1h","amount","purchaser_email_domain"],"categorical":{"purchaser_email_domain":["a"]}}`
	if err := os.WriteFile(filepath.Join(dir, EncoderFile), []byte(enc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadScorer(dir, c); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Errorf("reordered encoder: err = %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	for name, edit := range map[string][2]string{
		"multiclass":    {"num_class=1", "num_class=3"},
		"regression":    {"objective=binary sigmoid:1", "objective=regression"},
		"old version":   {"version=v4", "version=v3"},
		"linear tree":   {"is_linear=0\nshrinkage=1\n", "is_linear=1\nshrinkage=1\n"},
		"child cycle":   {"right_child=1 -3", "right_child=1 0"},
		"leaf range":    {"left_child=-1 -2", "left_child=-1 -9"},
		"feature range": {"split_feature=2 1", "split_feature=7 1"},
		"short values":  {"leaf_value=-3 -1 -2", "leaf_value=-3 -1"},
		"bad float":     {"threshold=4.5 0", "threshold=four 0"},
		"cat index":     {"threshold=4.5 0", "threshold=4.5 3"},
		"cat words":     {"cat_threshold=1", "cat_threshold=1 2"},
		"tree number":   {"Tree=1", "Tree=5"},
		"truncated":     {"end of trees", ""},
		"random forest": {"label_index=0", "label_index=0\naverage_output"},
	} {
		text := strings.Replace(handModel, edit[0], edit[1], 1)
		if text == handModel {
			t.Fatalf("%s: edit did not apply", name)
		}
		if _, err := Parse(strings.NewReader(text)); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
	if _, err := Parse(strings.NewReader(handModel)); err != nil {
		t.Fatal(err)
	}
}

func TestDescribe(t *testing.T) {
	c := schema.Default()
	f := c.MustLookup
	nan := math.NaN()
	for _, tc := range []struct {
		field string
		v     float64
		str   string
		known bool
		st    *Stat
		want  string
	}{
		{"card_txn_count_1h", 1, "", false, nil, "1 earlier payment on this card in the last hour"},
		{"uid_txn_count_24h", 0, "", false, nil, "no earlier payments on this customer in the last 24 hours"},
		{"device_amount_sum_7d", 1234.5, "", false, &Stat{P50: 80}, "$1,234.50 paid on this device in the last 7 days (typical: $80.00)"},
		{"email_mean_amount_7d", nan, "", false, nil, "no earlier payments on this email domain in the last 7 days"},
		{"card_mean_amount_7d", 42, "", false, nil, "this card averaged $42.00 per payment over the last 7 days"},
		{"card_amount_ratio_7d", 4.24, "", false, &Stat{P50: 1}, "amount is 4.2x the 7-day average for this card (typical: 1.0x)"},
		{"card_seconds_since_first", 150, "", false, nil, "this card was first seen 3 minutes ago"},
		{"card_seconds_since_last", nan, "", false, nil, "first payment seen from this card"},
		{"uid_seconds_since_last", 40, "", false, nil, "previous payment on this customer was 40 seconds ago"},
		{"device_seconds_since_first", 5 * 86400, "", false, nil, "this device was first seen 5 days ago"},
		{"distinct_cards_per_device_24h", 4, "", false, nil, "4 cards used on this device in the last 24 hours"},
		{"distinct_cards_per_email_24h", nan, "", false, nil, "no email domain to count cards on"},
		{"distance", 12, "", false, nil, "anonymized distance is 12"},
		{"billing_region", nan, "", false, nil, "anonymized billing region code is missing"},
		{"product_code", 0, "C", true, nil, `product code is "C"`},
		{"device_info", nan, "", false, nil, "device info string is missing"},
		{"purchaser_email_domain", nan, "x.biz", false, nil, `purchaser email domain "x.biz" was never seen in training`},
	} {
		if got := describe(f(tc.field), tc.v, tc.str, tc.known, tc.st); got != tc.want {
			t.Errorf("%s(%v, %q):\n got %q\nwant %q", tc.field, tc.v, tc.str, got, tc.want)
		}
	}
}

// largeScorer wraps the 500-tree fixture, whose inputs are the catalog
// fields, in a Scorer. Category code k is the string "v%03d" of k.
func largeScorer(tb testing.TB) (*Scorer, *schema.Catalog, [][]float64, []float64) {
	tb.Helper()
	m, rows, want := loadFixture(tb, "large")
	c := schema.Default()
	seen := map[string]map[string]struct{}{}
	for _, name := range m.FeatureNames() {
		if c.MustLookup(name).Kind == schema.String {
			seen[name] = map[string]struct{}{}
			for k := range 120 {
				seen[name][fmt.Sprintf("v%03d", k)] = struct{}{}
			}
		}
	}
	enc, err := schema.FitEncoder(c, m.FeatureNames(), seen)
	if err != nil {
		tb.Fatal(err)
	}
	cal := &Calibrator{Method: "isotonic", Input: "raw_score", X: []float64{-5, 0}, Y: []float64{0, 1}}
	s, err := NewScorer(c, enc, m, cal, Metadata{})
	if err != nil {
		tb.Fatal(err)
	}
	return s, c, rows, want
}

// toRow turns a model input vector back into a catalog row. It reports false
// for inputs no string can encode to, such as category -0.5.
func toRow(c *schema.Catalog, names []string, x []float64) (schema.Row, bool) {
	r := c.NewRow()
	for i, name := range names {
		f := c.MustLookup(name)
		if f.Kind == schema.Number {
			r.Num[f.Slot] = x[i]
			continue
		}
		switch v := x[i]; {
		case math.IsNaN(v):
		case v >= 0 && v < 120 && v == math.Trunc(v):
			r.Str[f.Slot] = fmt.Sprintf("v%03d", int(v))
		default:
			return r, false
		}
	}
	return r, true
}

// TestScorerParity runs the fixture rows through the whole scoring path
// (catalog row, Encoder, model) and checks LightGBM's bits come out.
func TestScorerParity(t *testing.T) {
	s, c, rows, want := largeScorer(t)
	checked := 0
	for i, x := range rows {
		r, ok := toRow(c, s.Model().FeatureNames(), x)
		if !ok {
			continue
		}
		checked++
		if _, _, raw := s.Score(r); math.Float64bits(raw) != math.Float64bits(want[i]) {
			t.Fatalf("row %d: Score raw %v, LightGBM %v", i, raw, want[i])
		}
		if i%50 == 0 {
			if reasons := s.Explain(r); len(reasons) > MaxReasons {
				t.Fatalf("row %d: %d reasons", i, len(reasons))
			}
		}
	}
	t.Logf("%d of %d rows representable as catalog rows, all bit-identical", checked, len(rows))
	if checked < len(rows)/2 {
		t.Fatalf("only %d rows checked", checked)
	}
}

// BenchmarkScore is the full path the service runs per payment: encode a
// catalog row, sum 500 trees, calibrate.
func BenchmarkScore(b *testing.B) {
	s, c, rows, _ := largeScorer(b)
	var rs []schema.Row
	for _, x := range rows[:1500] {
		if r, ok := toRow(c, s.Model().FeatureNames(), x); ok {
			rs = append(rs, r)
		}
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		s.Score(rs[i])
		if i++; i == len(rs) {
			i = 0
		}
	}
}

func BenchmarkExplain(b *testing.B) {
	s, c, rows, _ := largeScorer(b)
	r, _ := toRow(c, s.Model().FeatureNames(), rows[0])
	b.ReportAllocs()
	for b.Loop() {
		s.Explain(r)
	}
}
