package features

import (
	"math"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// FeatureError summarizes how one velocity feature from an approximate
// state differs from the reference over a replay.
type FeatureError struct {
	Name string
	// Rows is the number of rows where both values are present.
	Rows int
	// MissingMismatch counts rows where exactly one side is missing.
	MissingMismatch int
	// Exact counts rows (of Rows) where the values are bit-identical.
	Exact int
	// MeanAbs and MaxAbs are the mean and largest |approx - ref|.
	MeanAbs, MaxAbs float64
	// MeanRel is the mean of |approx - ref| / max(|ref|, 1), which keeps
	// small reference values from dominating.
	MeanRel float64
	// Over and Under count rows where approx is above or below ref.
	Over, Under int
}

// ExactFraction is Exact / Rows (1 when there are no rows).
func (f FeatureError) ExactFraction() float64 {
	if f.Rows == 0 {
		return 1
	}
	return float64(f.Exact) / float64(f.Rows)
}

// CompareStates replays txns through two engines in lockstep, one on ref
// (normally NewExact) and one on approx, and reports the error of every
// velocity feature. Both states should start empty. This is experiment 4's
// accuracy measurement.
func CompareStates(cat *schema.Catalog, txns []data.Txn, ref, approx State) ([]FeatureError, error) {
	refEng, err := NewEngine(cat, ref)
	if err != nil {
		return nil, err
	}
	apxEng, err := NewEngine(cat, approx)
	if err != nil {
		return nil, err
	}
	fields := schema.VelocityFieldNames()
	slots := make([]int, len(fields))
	out := make([]FeatureError, len(fields))
	for i, f := range fields {
		slots[i] = cat.MustLookup(f.Name).Slot
		out[i].Name = f.Name
	}
	sumAbs := make([]float64, len(fields))
	sumRel := make([]float64, len(fields))
	apxRow := apxEng.NewRow()
	err = Replay(refEng, txns, func(t *data.Txn, refRow schema.Row) error {
		apxEng.ScoreAndUpdate(t, apxRow)
		for i, s := range slots {
			r, a := refRow.Num[s], apxRow.Num[s]
			rNaN, aNaN := math.IsNaN(r), math.IsNaN(a)
			fe := &out[i]
			switch {
			case rNaN && aNaN:
				continue
			case rNaN != aNaN:
				fe.MissingMismatch++
				continue
			}
			fe.Rows++
			if math.Float64bits(r) == math.Float64bits(a) {
				fe.Exact++
				continue
			}
			d := math.Abs(a - r)
			sumAbs[i] += d
			sumRel[i] += d / max(math.Abs(r), 1)
			fe.MaxAbs = max(fe.MaxAbs, d)
			if a > r {
				fe.Over++
			} else if a < r {
				fe.Under++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		if n := float64(out[i].Rows); n > 0 {
			out[i].MeanAbs = sumAbs[i] / n
			out[i].MeanRel = sumRel[i] / n
		}
	}
	return out, nil
}
