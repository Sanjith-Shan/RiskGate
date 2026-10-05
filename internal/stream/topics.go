package stream

import (
	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/features"
)

// Topics names the pipeline's topics. Prefix them all with one string so
// that two pipelines (an experiment and its negative control) can share a
// cluster.
type Topics struct {
	Payments     string // in: assess request JSON, keyed by card; heartbeats
	EntityEvents string // router -> aggregators, keyed by entity key
	Parts        string // router and aggregators -> joiner, keyed by payment id
	Decisions    string // out: decision-log JSON lines, keyed by payment id
	Disputes     string // in: dispute webhook events, keyed by payment id
	DeadLetters  string // payments the router could not parse
}

// TopicsWithPrefix returns the topic names under prefix, e.g. "riskgate".
func TopicsWithPrefix(prefix string) Topics {
	return Topics{
		Payments:     prefix + ".payments",
		EntityEvents: prefix + ".entity-events",
		Parts:        prefix + ".parts",
		Decisions:    prefix + ".decisions",
		Disputes:     prefix + ".disputes",
		DeadLetters:  prefix + ".payments-dlq",
	}
}

// Consumer group names, one per stage.
func (t Topics) group(stage string) string { return t.Payments + "." + stage }

// Partitioning. Every hop picks its partition itself (a manual partitioner),
// from a hash RiskGate defines, so placement does not depend on a client
// library's default partitioner and can be computed offline.

// EntityPartition is where an entity key's state lives: features.Key.Hash,
// the same deterministic hash the sharded state and snapshots use.
func EntityPartition(k features.Key, n int32) int32 { return int32(k.Hash() % uint64(n)) }

// PaymentPartition is where a payment's parts are joined and its decision is
// written: FNV-1a over the payment id.
func PaymentPartition(paymentID string, n int32) int32 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(paymentID); i++ {
		h ^= uint64(paymentID[i])
		h *= 1099511628211
	}
	return int32(h % uint64(n))
}

// CardPartition is where the producer writes a payment: by card1, as a
// payment service would key its events by account. Payments without a card
// share one partition.
func CardPartition(t *data.Txn, n int32) int32 {
	k, ok := features.CardKey(t.Card1)
	if !ok {
		return 0
	}
	return EntityPartition(k, n)
}
