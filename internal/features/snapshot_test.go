package features

import (
	"bytes"
	"strings"
	"testing"
)

func snapshot(t testing.TB, st State) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := st.Snapshot(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestSnapshotRestore checks the two properties the service relies on at
// restart: a restored state equals the original (its snapshot is
// byte-identical), and scoring after the restore matches an uninterrupted
// run bit for bit.
func TestSnapshotRestore(t *testing.T) {
	txns := synthetic(t, 20000, 20)
	half := len(txns) / 2
	for _, k := range stateKinds {
		t.Run(k.name, func(t *testing.T) {
			orig := newEngine(t, k.new())
			if err := Replay(orig, txns[:half], nil); err != nil {
				t.Fatal(err)
			}
			snap := snapshot(t, orig.State())

			restoredState := k.new()
			if err := restoredState.Restore(bytes.NewReader(snap)); err != nil {
				t.Fatal(err)
			}
			if again := snapshot(t, restoredState); !bytes.Equal(again, snap) {
				t.Fatalf("restored state differs: snapshots of %d and %d bytes", len(snap), len(again))
			}
			if a, b := orig.State().Stats().Keys, restoredState.Stats().Keys; a != b {
				t.Errorf("keys: %d before, %d after", a, b)
			}

			restored := newEngine(t, restoredState)
			ra, rb := orig.NewRow(), restored.NewRow()
			for i := half; i < len(txns); i++ {
				orig.ScoreAndUpdate(&txns[i], ra)
				restored.ScoreAndUpdate(&txns[i], rb)
				if name, differ := diffRows(ra, rb); differ {
					t.Fatalf("txn %d: %s differs after restore", i, name)
				}
			}
		})
	}
}

// SyncMap and the map-based store of the same kind share a format, so the
// service can change its concurrency strategy across a restart.
func TestSnapshotMovesBetweenStores(t *testing.T) {
	txns := synthetic(t, 5000, 5)
	e := newEngine(t, NewSyncMap(NewExact(0)))
	if err := Replay(e, txns, nil); err != nil {
		t.Fatal(err)
	}
	snap := snapshot(t, e.State())
	plain := NewExact(0)
	if err := plain.Restore(bytes.NewReader(snap)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshot(t, plain), snap) {
		t.Error("exact store restored from a SyncMap snapshot differs")
	}
}

func TestRestoreRejectsBadSnapshots(t *testing.T) {
	txns := synthetic(t, 2000, 5)
	for _, k := range stateKinds {
		e := newEngine(t, k.new())
		if err := Replay(e, txns, nil); err != nil {
			t.Fatal(err)
		}
		snap := snapshot(t, e.State())
		before := snapshot(t, e.State())
		for _, cut := range []int{0, 5, len(snap) / 2, len(snap) - 1} {
			if err := e.State().Restore(bytes.NewReader(snap[:cut])); err == nil {
				t.Errorf("%s: truncated at %d of %d bytes accepted", k.name, cut, len(snap))
			}
		}
		if err := e.State().Restore(bytes.NewReader(append(bytes.Clone(snap), 0))); err == nil {
			t.Errorf("%s: trailing garbage accepted", k.name)
		}
		if !bytes.Equal(snapshot(t, e.State()), before) {
			t.Errorf("%s: a failed restore changed the state", k.name)
		}
	}
}

func TestRestoreRejectsOtherKinds(t *testing.T) {
	exact := snapshot(t, NewExact(0))
	tests := []struct {
		name string
		st   State
		snap []byte
		want string
	}{
		{"kind", NewBucketed(BucketedConfig{}), exact, "snapshot is of a"},
		{"ttl", NewExact(9 * 86400), exact, "idle TTL"},
		{"plan", NewBucketed(BucketedConfig{Plan: DefaultSketchPlan}), snapshot(t, NewBucketed(BucketedConfig{})), "config"},
		{"sketch config", NewSketch(SketchConfig{Width: 1 << 10}), snapshot(t, NewSketch(SketchConfig{Width: 1 << 11})), "sketch config"},
		{"shards", NewSharded(2, func() State { return NewExact(0) }), snapshot(t, NewSharded(3, func() State { return NewExact(0) })), "shard count"},
		{"sketch from exact", NewSketch(SketchConfig{Width: 1 << 10}), exact, "snapshot is of a"},
	}
	for _, tt := range tests {
		err := tt.st.Restore(bytes.NewReader(tt.snap))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want an error mentioning %q", tt.name, err, tt.want)
		}
	}
}
