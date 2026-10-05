package features

import (
	"math"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// TestPartitionedEqualsReplay is the property the stream pipeline rests on.
// Split velocity state by entity key across P independent engines, each
// seeing only the payments that carry one of its keys, compute each payment's
// features one entity at a time with ScoreAndUpdateEntity, and join them with
// FillRaw and FillMissing: every row must be bit-identical to Replay's.
func TestPartitionedEqualsReplay(t *testing.T) {
	txns := synthetic(t, 20000, 20)
	want := replayAll(t, newEngine(t, NewExact(0)), txns)

	for _, parts := range []int{1, 3, 8} {
		engines := make([]*Engine, parts)
		for i := range engines {
			engines[i] = newEngine(t, NewExact(0))
		}
		join := engines[0]
		scratch := join.NewRow()
		for i := range txns {
			tx := &txns[i]
			row := join.NewRow()
			join.FillRaw(tx, row)
			keys, ok := KeysOf(tx)
			for ent := range Entity(NumEntities) {
				if !ok[ent] {
					join.FillMissing(ent, row)
					continue
				}
				owner := engines[keys[ent].Hash()%uint64(parts)]
				if !owner.ScoreAndUpdateEntity(tx, ent, scratch) {
					t.Fatalf("payment %d: key present but ScoreAndUpdateEntity reported none", tx.ID)
				}
				for _, s := range owner.EntitySlots(ent) {
					row.Num[s] = scratch.Num[s]
				}
			}
			if !rowsEqual(row, want[i]) {
				t.Fatalf("%d partitions: payment %d differs from Replay", parts, tx.ID)
			}
		}
	}
}

// TestEntitySlotsCoverVelocity checks that the four entities' slots are
// disjoint and, with the raw fields, cover every numeric field but risk_score.
func TestEntitySlotsCoverVelocity(t *testing.T) {
	e := newEngine(t, NewExact(0))
	seen := map[int]Entity{}
	for ent := range Entity(NumEntities) {
		for _, s := range e.EntitySlots(ent) {
			if prev, dup := seen[s]; dup {
				t.Fatalf("slot %d belongs to %v and %v", s, prev, ent)
			}
			seen[s] = ent
		}
	}
	velocity := len(schema.VelocityFieldNames())
	if len(seen) != velocity {
		t.Fatalf("entity slots cover %d fields, schema has %d velocity features", len(seen), velocity)
	}
}

func rowsEqual(a, b schema.Row) bool {
	for i := range a.Num {
		x, y := a.Num[i], b.Num[i]
		if math.Float64bits(x) != math.Float64bits(y) && !(math.IsNaN(x) && math.IsNaN(y)) {
			return false
		}
	}
	for i := range a.Str {
		if a.Str[i] != b.Str[i] {
			return false
		}
	}
	return true
}
