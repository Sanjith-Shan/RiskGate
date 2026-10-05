package stream

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/features"
	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
	"github.com/Sanjith-Shan/RiskGate/internal/service"
)

// Decider is what the joiner needs to turn a feature row into a decision:
// the model, the rule set, and the decision-log encoder. One Decider is
// shared by every joiner task of a process; all of it is read-only.
type Decider struct {
	Catalog  *schema.Catalog
	Scorer   *model.Scorer // nil: risk_score is missing, as in the service
	Rules    *rules.RuleSet
	Encoder  *service.DecisionEncoder
	riskSlot int
	nIn      int
	shape    *features.Engine // FillRaw, FillMissing and EntitySlots only; its state is never touched
	slots    [features.NumEntities][]int
	now      func() time.Time
}

// NewDecider builds a Decider. The encoder is built here so the decisions
// topic carries exactly the service's decision-log lines.
func NewDecider(cat *schema.Catalog, scorer *model.Scorer, rs *rules.RuleSet, logContributions int) (*Decider, error) {
	shape, err := features.NewEngine(cat, features.NewExact(0))
	if err != nil {
		return nil, err
	}
	risk, ok := cat.Lookup(schema.RiskScoreField.Name)
	if !ok {
		return nil, fmt.Errorf("stream: catalog has no risk_score")
	}
	d := &Decider{Catalog: cat, Scorer: scorer, Rules: rs, riskSlot: risk.Slot, shape: shape, now: time.Now}
	var names []string
	var sum string
	if scorer != nil {
		names, sum, d.nIn = scorer.FeatureNames(), scorer.SHA256(), scorer.NumFeatures()
	}
	d.Encoder = service.NewDecisionEncoder(cat, names, sum, logContributions)
	for ent := range features.Entity(features.NumEntities) {
		d.slots[ent] = shape.EntitySlots(ent)
	}
	return d, nil
}

// join is one payment waiting for its parts.
type join struct {
	base   *BasePart
	have   uint8 // entities received
	values [features.NumEntities][]float64
}

func (j *join) complete() bool { return j.base != nil && j.have == j.base.Mask }

// joiner assembles each payment's row from its parts, scores it, applies
// the rules and writes the decision. It owns one parts partition.
type joiner struct {
	part      int32
	decParts  int32
	d         *Decider
	pending   map[string]*join
	lastSrc   map[int32]int64  // per payments partition, the highest base-part offset received
	lastAgg   map[int32]uint64 // per aggregator partition, the highest sequence number received
	decided   uint64
	dups      uint64
	row       schema.Row
	rec       *service.DecisionRecord
	x         []float64 // model inputs, scratch
	shadowBuf []*rules.CompiledRule
}

func newJoiner(d *Decider, part, decParts int32) *joiner {
	return &joiner{
		part: part, decParts: decParts, d: d,
		pending: map[string]*join{}, lastSrc: map[int32]int64{}, lastAgg: map[int32]uint64{},
		row: d.Catalog.NewRow(), rec: service.NewDecisionRecord(d.Catalog, d.nIn), x: make([]float64, d.nIn),
	}
}

func (j *joiner) handle(value []byte, out []output) ([]output, error) {
	base, ent, err := decodePart(value)
	if err != nil {
		return out, err
	}
	var id string
	if base != nil {
		if last, ok := j.lastSrc[base.Src]; ok && base.Offset <= last {
			j.dups++
			return out, nil
		}
		j.lastSrc[base.Src] = base.Offset
		id = base.PaymentID
		p := j.entry(id)
		p.base = base
	} else {
		if ent.Entity >= features.NumEntities || len(ent.Values) != len(j.d.slots[ent.Entity]) {
			return out, fmt.Errorf("stream: entity part for %s: entity %d with %d values", ent.PaymentID, ent.Entity, len(ent.Values))
		}
		if last, ok := j.lastAgg[ent.Agg]; ok && ent.Seq <= last {
			j.dups++
			return out, nil
		}
		j.lastAgg[ent.Agg] = ent.Seq
		id = ent.PaymentID
		p := j.entry(id)
		p.have |= 1 << ent.Entity
		p.values[ent.Entity] = ent.Values
	}
	p := j.pending[id]
	if !p.complete() {
		return out, nil
	}
	delete(j.pending, id)
	return append(out, j.decide(p)), nil
}

func (j *joiner) entry(id string) *join {
	p := j.pending[id]
	if p == nil {
		p = &join{}
		j.pending[id] = p
	}
	return p
}

// decide is the service's assess pipeline after the feature step: the same
// row layout, the same scorer call, the same rule evaluation, the same log
// line.
func (j *joiner) decide(p *join) output {
	d, b, row, rec := j.d, p.base, j.row, j.rec
	d.shape.FillRaw(&b.Txn, row)
	for ent := range features.Entity(features.NumEntities) {
		if b.Mask&(1<<ent) == 0 {
			d.shape.FillMissing(ent, row)
			continue
		}
		for i, s := range d.slots[ent] {
			row.Num[s] = p.values[ent][i]
		}
	}
	rec.Scored = d.Scorer != nil
	if rec.Scored {
		rec.RiskScore, rec.Prob, rec.Raw, rec.Bias = d.Scorer.ScoreContributions(row, j.x, rec.Contrib)
		row.Num[d.riskSlot] = float64(rec.RiskScore)
	} else {
		rec.RiskScore, rec.Prob, rec.Raw, rec.Bias = 0, math.NaN(), math.NaN(), math.NaN()
	}
	dec := d.Rules.EvaluateAppend(row, j.shadowBuf[:0])
	j.shadowBuf = dec.Shadow[:0]

	now := d.now()
	rec.AssessmentID = assessmentID(b.PaymentID)
	rec.PaymentID = b.PaymentID
	rec.Created = b.Created
	rec.At = now.UnixMilli()
	rec.Version = dec.Version
	rec.Action = dec.Action
	rec.RuleID = ""
	if dec.Rule != nil {
		rec.RuleID = dec.Rule.ID
	}
	rec.Shadow = rec.Shadow[:0]
	for _, r := range dec.Shadow {
		rec.Shadow = append(rec.Shadow, r.ID)
	}
	copy(rec.Num, row.Num)
	copy(rec.Str, row.Str)
	rec.Latency = now.Sub(time.UnixMilli(b.Produced))
	j.decided++
	return output{
		topic: topicDecisions, partition: PaymentPartition(b.PaymentID, j.decParts),
		key: b.PaymentID, value: d.Encoder.Append(nil, rec),
	}
}

// assessmentID is deterministic, so a decision re-sent after a crash is the
// same decision: "asmt_" + hex("kafk") + FNV-1a of the payment id.
func assessmentID(paymentID string) (id [29]byte) {
	h := uint64(14695981039346656037)
	for i := 0; i < len(paymentID); i++ {
		h ^= uint64(paymentID[i])
		h *= 1099511628211
	}
	b := append([]byte("asmt_6b61666b"), fmt.Sprintf("%016x", h)...)
	copy(id[:], b)
	return id
}

const joinerSnapshotVersion = 1

func (j *joiner) snapshot() ([]byte, error) {
	e := &enc{}
	e.uvarint(joinerSnapshotVersion)
	e.uvarint(j.decided)
	e.uvarint(j.dups)
	srcs := slices.Sorted(maps.Keys(j.lastSrc))
	e.uvarint(uint64(len(srcs)))
	for _, s := range srcs {
		e.varint(int64(s))
		e.varint(j.lastSrc[s])
	}
	aggs := slices.Sorted(maps.Keys(j.lastAgg))
	e.uvarint(uint64(len(aggs)))
	for _, a := range aggs {
		e.varint(int64(a))
		e.uvarint(j.lastAgg[a])
	}
	ids := slices.Sorted(maps.Keys(j.pending))
	e.uvarint(uint64(len(ids)))
	for _, id := range ids {
		p := j.pending[id]
		e.str(id)
		if p.base != nil {
			e.byte(1)
			e.bytes(appendBasePart(nil, p.base))
		} else {
			e.byte(0)
		}
		e.byte(p.have)
		for ent := range features.Entity(features.NumEntities) {
			if p.have&(1<<ent) != 0 {
				e.uvarint(uint64(len(p.values[ent])))
				for _, v := range p.values[ent] {
					e.f64(v)
				}
			}
		}
	}
	return e.b, nil
}

func (j *joiner) restore(b []byte) error {
	d := &dec{b: b}
	if v := d.uvarint(); d.err == nil && v != joinerSnapshotVersion {
		return fmt.Errorf("stream: joiner snapshot version %d", v)
	}
	j.decided = d.uvarint()
	j.dups = d.uvarint()
	for n := d.uvarint(); n > 0 && d.err == nil; n-- {
		s := int32(d.varint())
		j.lastSrc[s] = d.varint()
	}
	for n := d.uvarint(); n > 0 && d.err == nil; n-- {
		a := int32(d.varint())
		j.lastAgg[a] = d.uvarint()
	}
	for n := d.uvarint(); n > 0 && d.err == nil; n-- {
		id := d.str()
		p := &join{}
		if d.byte() == 1 {
			base, _, err := decodePart(d.raw())
			if err != nil {
				return err
			}
			if base == nil {
				return fmt.Errorf("stream: joiner snapshot: pending %s has an entity part as its base", id)
			}
			p.base = base
		}
		p.have = d.byte()
		for ent := range features.Entity(features.NumEntities) {
			if p.have&(1<<ent) != 0 {
				k := d.uvarint()
				if k > 64 {
					return errShort
				}
				p.values[ent] = make([]float64, k)
				for i := range p.values[ent] {
					p.values[ent][i] = d.f64()
				}
			}
		}
		j.pending[id] = p
	}
	return d.done()
}
