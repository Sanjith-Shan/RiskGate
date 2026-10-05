package stream

import (
	"maps"
	"sync"
)

// StageMetrics counts one stage's work in one process.
type StageMetrics struct {
	Records, Batches, Outputs uint64
	BusyNanos                 uint64
	FirstBatchMs              int64 // since the runner started; how long a restart took to do work

	Checkpoints, CheckpointNanos, SnapshotBytes uint64
	Assigned, Restores, RestoreNanos            uint64
	Revoked, Lost                               uint64

	// route
	Payments, Heartbeats, DeadLetters, OrderViolations uint64
	// aggregate and join
	Duplicates, Late, Decided uint64
}

// Metrics is a process's stage metrics, safe for concurrent use.
type Metrics struct {
	mu     sync.Mutex
	stages map[string]*StageMetrics
}

func (m *Metrics) add(stage string, fn func(*StageMetrics)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stages == nil {
		m.stages = map[string]*StageMetrics{}
	}
	s := m.stages[stage]
	if s == nil {
		s = &StageMetrics{}
		m.stages[stage] = s
	}
	fn(s)
}

// Snapshot returns a copy of every stage's metrics.
func (m *Metrics) Snapshot() map[string]StageMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]StageMetrics, len(m.stages))
	for k, v := range maps.All(m.stages) {
		out[k] = *v
	}
	return out
}
