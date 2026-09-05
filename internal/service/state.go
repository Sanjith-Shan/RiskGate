package service

import (
	"fmt"

	"github.com/Sanjith-Shan/RiskGate/internal/features"
)

// NewState builds the concurrent velocity state the service runs on.
//
// kind is the data structure (experiment 4): exact, bucketed or sketch.
// mode is how it is made safe for concurrent requests (experiment 5):
// sharded (n independently locked shards), locked (one mutex), or syncmap
// (per-key entries in a sync.Map; exact and bucketed only). The returned
// description goes into snapshots, which can only be restored into the
// same configuration.
//
// A sharded sketch gives every shard its own full-size sketch (about 55 MB
// each with the defaults), so keep shards low with -state sketch.
func NewState(kind, mode string, shards int) (features.State, string, error) {
	var newState func() features.State
	switch kind {
	case "exact":
		newState = func() features.State { return features.NewExact(0) }
	case "bucketed":
		newState = func() features.State { return features.NewBucketed(features.BucketedConfig{}) }
	case "sketch":
		newState = func() features.State { return features.NewSketch(features.SketchConfig{}) }
	default:
		return nil, "", fmt.Errorf("unknown state %q (want exact, bucketed or sketch)", kind)
	}
	switch mode {
	case "sharded":
		if shards < 1 {
			return nil, "", fmt.Errorf("shards must be at least 1, got %d", shards)
		}
		return features.NewSharded(shards, newState), fmt.Sprintf("%s/sharded/%d", kind, shards), nil
	case "locked":
		return features.NewLocked(newState), kind + "/locked", nil
	case "syncmap":
		if kind == "sketch" {
			return nil, "", fmt.Errorf("syncmap needs a per-key state (exact or bucketed), not sketch")
		}
		return features.NewSyncMap(newState()), kind + "/syncmap", nil
	}
	return nil, "", fmt.Errorf("unknown sharding %q (want sharded, locked or syncmap)", mode)
}
