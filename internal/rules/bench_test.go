package rules

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

var benchRules = []struct{ name, src string }{
	{"attr_vs_const", "block if :risk_score: >= 85"},
	{"velocity", "block if :card_txn_count_1h: >= 8 and :amount: > 3 * :card_mean_amount_7d:"},
	{"named_list", "allow if :purchaser_email_domain: in @trusted_domains"},
	{"lowered_large_list", "block if lower(:purchaser_email_domain:) in @risky_domains"},
	{"missing_and_text", `review if :product_code: = "C" and is_missing(:device_info:)`},
	{"prefix", `review if starts_with(lower(:device_info:), "sm-") and :distinct_cards_per_device_24h: > 3`},
}

func benchRows(n int) []schema.Row {
	rng := rand.New(rand.NewPCG(7, 8))
	rows := make([]schema.Row, n)
	for i := range rows {
		rows[i] = GenerateRow(rng, schema.Default())
	}
	return rows
}

// BenchmarkRule reports ns and allocations per rule evaluation.
func BenchmarkRule(b *testing.B) {
	rows := benchRows(1024)
	for _, br := range benchRules {
		b.Run(br.name, func(b *testing.B) {
			rs := mustLoad(b, br.src)
			cr := rs.Rules[0]
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				cr.Match(rows[i&1023])
				i++
			}
		})
	}
}

// BenchmarkRuleSet evaluates a whole rule set per payment, the online path.
func BenchmarkRuleSet(b *testing.B) {
	var srcs []string
	for _, br := range benchRules {
		srcs = append(srcs, br.src)
	}
	rs := mustLoad(b, strings.Join(srcs, "\n"))
	rows := benchRows(1024)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		rs.Evaluate(rows[i&1023])
		i++
	}
}

// BenchmarkGenerated evaluates a mix of generated rules, a rough measure of
// the cost of deeper trees than people write.
func BenchmarkGenerated(b *testing.B) {
	env := testEnv()
	rng := rand.New(rand.NewPCG(9, 10))
	crs := make([]*CompiledRule, 256)
	for i := range crs {
		cr, err := Compile(Generate(rng, env, 3))
		if err != nil {
			b.Fatal(err)
		}
		crs[i] = cr
	}
	rows := benchRows(1024)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		crs[i&255].Match(rows[i&1023])
		i++
	}
}

func BenchmarkLoad(b *testing.B) {
	var srcs []string
	for _, br := range benchRules {
		srcs = append(srcs, br.src)
	}
	src := strings.Join(srcs, "\n")
	env := testEnv()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Load(src, env, 1); err != nil {
			b.Fatal(err)
		}
	}
}
