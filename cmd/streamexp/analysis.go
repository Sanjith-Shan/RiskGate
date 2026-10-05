package main

import (
	"flag"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/features"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
	"github.com/Sanjith-Shan/RiskGate/internal/stream"
)

// naive is the design the pipeline does not use, measured offline. A
// consumer group on a payments topic partitioned by card, where each
// partition keeps every entity's velocity state for its own payments only.
// Card and uid (which contains card1) are co-partitioned, so they come out
// right; device and email domain aggregate across cards, so each partition
// sees only its share of their traffic. This computes, for P partitions,
// how many payments would get different features than the offline export,
// by entity. It is a deterministic computation of the same Engine on the
// same partitioning a Kafka run would use, so it needs no broker.
func naive(args []string) error {
	fs := flag.NewFlagSet("naive", flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory")
	results := fs.String("results", "results/stream/naive.jsonl", "results file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	txns, err := loadTxns(*dataDir, 0)
	if err != nil {
		return err
	}
	cat := schema.Default()
	ref, err := features.NewEngine(cat, features.NewExact(0))
	if err != nil {
		return err
	}
	shape := ref
	slots := [features.NumEntities][]int{}
	for ent := range features.Entity(features.NumEntities) {
		slots[ent] = shape.EntitySlots(ent)
	}
	type outcome struct {
		Partitions     int            `json:"payments_partitions"`
		RowsDiffer     int            `json:"payments_with_any_feature_differing"`
		ByEntity       map[string]int `json:"payments_differing_by_entity"`
		ShareDiffering float64        `json:"share_differing"`
	}
	start := time.Now()
	var outs []outcome
	for _, p := range []int{1, 2, 4, 8, 16} {
		engines := make([]*features.Engine, p)
		for i := range engines {
			if engines[i], err = features.NewEngine(cat, features.NewExact(0)); err != nil {
				return err
			}
		}
		ref, _ = features.NewEngine(cat, features.NewExact(0))
		o := outcome{Partitions: p, ByEntity: map[string]int{}}
		got, want := cat.NewRow(), cat.NewRow()
		for i := range txns {
			t := &txns[i]
			ref.ScoreAndUpdate(t, want)
			engines[stream.CardPartition(t, int32(p))].ScoreAndUpdate(t, got)
			any := false
			for ent := range features.Entity(features.NumEntities) {
				for _, s := range slots[ent] {
					if !same(got.Num[s], want.Num[s]) {
						o.ByEntity[ent.String()]++
						any = true
						break
					}
				}
			}
			if any {
				o.RowsDiffer++
			}
		}
		o.ShareDiffering = float64(o.RowsDiffer) / float64(len(txns))
		fmt.Printf("P=%d: %d of %d payments differ %v\n", p, o.RowsDiffer, len(txns), o.ByEntity)
		outs = append(outs, o)
	}
	prov := provenance()
	return appendRow(*results, row{
		Experiment: "naive-card-partitioning", Run: "naive-" + time.Now().UTC().Format("20060102T150405"), Provenance: prov,
		Config:   map[string]any{"payments": len(txns), "partitioning": "stream.CardPartition (card1 hash)", "state": "exact"},
		Result:   map[string]any{"by_partitions": outs, "took_s": time.Since(start).Seconds()},
		Quotable: true, Note: "offline computation with the pipeline's partitioning; load-independent",
	})
}

func same(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b) || (math.IsNaN(a) && math.IsNaN(b))
}

// skew reports how the entity events of the whole dataset spread over the
// aggregator partitions: the hot-key question. An email domain is one key,
// so gmail.com's events all land on one partition whatever the count.
func skew(args []string) error {
	fs := flag.NewFlagSet("skew", flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory")
	results := fs.String("results", "results/stream/skew.jsonl", "results file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	txns, err := loadTxns(*dataDir, 0)
	if err != nil {
		return err
	}
	type keyCount struct {
		Key   string `json:"key"`
		Count int    `json:"events"`
	}
	type outcome struct {
		Partitions   int        `json:"entity_partitions"`
		PerPartition []int      `json:"events_per_partition"`
		MaxOverMean  float64    `json:"max_over_mean"`
		TopKeys      []keyCount `json:"top_keys,omitempty"`
	}
	perKey := map[features.Key]int{}
	var total int
	for i := range txns {
		keys, ok := features.KeysOf(&txns[i])
		for ent := range features.Entity(features.NumEntities) {
			if ok[ent] {
				perKey[keys[ent]]++
				total++
			}
		}
	}
	var outs []outcome
	for _, p := range []int{4, 8, 16, 32} {
		o := outcome{Partitions: p, PerPartition: make([]int, p)}
		for k, n := range perKey {
			o.PerPartition[stream.EntityPartition(k, int32(p))] += n
		}
		o.MaxOverMean = float64(slices.Max(o.PerPartition)) / (float64(total) / float64(p))
		outs = append(outs, o)
	}
	var top []keyCount
	for k, n := range perKey {
		top = append(top, keyCount{k.Entity.String() + " " + keyLabel(k), n})
	}
	slices.SortFunc(top, func(a, b keyCount) int { return b.Count - a.Count })
	outs[0].TopKeys = top[:5]
	for _, o := range outs {
		fmt.Printf("P=%d max/mean %.2f %v\n", o.Partitions, o.MaxOverMean, o.PerPartition)
	}
	fmt.Println("top keys:", top[:5], "of", total, "entity events")
	return appendRow(*results, row{
		Experiment: "entity-partition-skew", Run: "skew-" + time.Now().UTC().Format("20060102T150405"), Provenance: provenance(),
		Config:   map[string]any{"payments": len(txns), "entity_events": total, "partitioner": "features.Key.Hash mod P"},
		Result:   map[string]any{"by_partitions": outs},
		Quotable: true, Note: "counts from the data; load-independent",
	})
}

// keyLabel names a key for the hot-key list. Card and uid keys are
// anonymized numbers; only device strings and email domains are shown.
func keyLabel(k features.Key) string {
	if k.Entity == features.Device || k.Entity == features.Email {
		return k.Str
	}
	return "(id withheld)"
}
