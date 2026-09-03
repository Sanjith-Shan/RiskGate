package rules

import (
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

func TestEvaluateAppendMatchesEvaluate(t *testing.T) {
	cat := schema.Default()
	rs, err := Load(`allow if :amount: < 5
block if :card_txn_count_1h: >= 3
shadow block if :amount: > 100
shadow review if not is_missing(:device_info:)
`, Env{Catalog: cat}, 1)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	buf := make([]*CompiledRule, 0, 4)
	for range 2000 {
		row := GenerateRow(rng, cat)
		want := rs.Evaluate(row)
		got := rs.EvaluateAppend(row, buf)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("EvaluateAppend %+v, Evaluate %+v", got, want)
		}
	}
	row := GenerateRow(rng, cat)
	row.Num[cat.MustLookup("amount").Slot] = 500
	if n := testing.AllocsPerRun(100, func() { rs.EvaluateAppend(row, buf) }); n != 0 {
		t.Fatalf("EvaluateAppend allocates %v times with a buffer", n)
	}
}
