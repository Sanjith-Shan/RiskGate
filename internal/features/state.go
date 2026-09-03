package features

import (
	"io"
	"math"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// NumWindows is the number of trailing windows, len(schema.Windows).
const NumWindows = 3

// windowSeconds holds schema.Windows' lengths; distinctWindow is the index
// of the 24-hour window, the one distinct-card counts use.
var (
	windowSeconds  [NumWindows]int64
	distinctWindow = -1
)

// maxWindow is the longest window; nothing older than it is ever needed for
// counts and sums.
var maxWindow int64

func init() {
	if len(schema.Windows) != NumWindows {
		panic("features: schema.Windows changed; update NumWindows")
	}
	for i, w := range schema.Windows {
		windowSeconds[i] = w.Seconds
		if i > 0 && w.Seconds <= windowSeconds[i-1] {
			panic("features: schema.Windows must be in increasing order")
		}
		if w.Seconds == 86400 {
			distinctWindow = i
		}
	}
	if distinctWindow < 0 {
		panic("features: no 24h window for the distinct-card features")
	}
	maxWindow = windowSeconds[NumWindows-1]
	if windowSeconds[weekWindow] != 7*86400 {
		panic("features: the last window must be 7d")
	}
}

// weekWindow is the index of the 7-day window the mean and ratio use.
const weekWindow = NumWindows - 1

// Event is what the state remembers about one payment for one entity.
type Event struct {
	Time  int64  // TransactionDT, seconds
	Milli int64  // amount in thousandths of a dollar
	Card  uint64 // card1's bit pattern, or NoCard
}

// NoCard marks an event without a card1. It is a NaN bit pattern, which
// bits never produces for a present card.
const NoCard = ^uint64(0)

// EventOf builds t's event.
//
// Amounts are accumulated as integer thousandths of a dollar. IEEE-CIS
// amounts have at most three decimal places, so this is exact, and integer
// sums can be added to and subtracted from as events enter and leave a
// window without the drift a float64 running sum would pick up. That keeps
// every implementation, and every replay, bit-identical.
func EventOf(t *data.Txn) Event {
	ev := Event{Time: t.DT, Milli: toMilli(t.Amount), Card: NoCard}
	if present(t.Card1) {
		ev.Card = floatBits(t.Card1)
	}
	return ev
}

// toMilli converts dollars to thousandths. A missing amount counts as zero
// dollars (the payment still counts); IEEE-CIS never has one.
func toMilli(amount float64) int64 {
	if !present(amount) {
		return 0
	}
	return int64(math.Round(amount * 1000))
}

// Aggregates is what a state knows about one key at one instant: every
// event recorded for the key so far, restricted to each trailing window.
type Aggregates struct {
	Count [NumWindows]float64 // payments in the window
	Sum   [NumWindows]float64 // dollars in the window
	// Distinct is the number of distinct cards in the 24h window, or NaN for
	// entities that do not track it.
	Distinct float64
	// Seen reports whether the key has been active within the idle TTL.
	// First and Last are its first and latest event times when it has.
	Seen        bool
	First, Last int64
}

// Stats describes a state's size.
type Stats struct {
	Keys  int   // live keys, or -1 when the structure cannot tell (sketches)
	Bytes int64 // estimated heap bytes; see each implementation for the model
}

// State stores per-key event history for the velocity features.
//
// Semantics every implementation shares:
//
//   - Read(k, now) describes the events added for k so far that fall in the
//     trailing windows ending at now: an event at time t' is inside window W
//     when now - W < t'. The state holds no event later than the one being
//     scored when driven by Replay, so "added so far" is "strictly earlier by
//     (DT, TransactionID)". Read never modifies the state and is safe to call
//     concurrently with other Reads.
//   - Add records an event. Event times for one key are expected to be
//     nondecreasing; a late event is recorded at the key's latest time.
//   - A key idle for at least the idle TTL is forgotten: it reads as unseen,
//     and its next Add starts it afresh. This is decided from event times
//     alone, so when EvictIdle physically frees the memory changes nothing.
//   - ReadAdd is Read followed by Add as one step on one key.
//
// The approximate implementations document how they deviate. None are safe
// for concurrent mutation; wrap them with NewSharded, NewLocked, or use
// NewSyncMap.
type State interface {
	Read(k Key, now int64) Aggregates
	Add(k Key, ev Event)
	ReadAdd(k Key, ev Event) Aggregates
	// EvictIdle frees keys idle at now and returns how many it freed.
	EvictIdle(now int64) int
	Stats() Stats
	// Snapshot writes the full state; Restore replaces the state with one
	// read from a snapshot of the same kind and configuration.
	Snapshot(w io.Writer) error
	Restore(r io.Reader) error
}

// DefaultIdleTTL is how long a key survives without payments: 30 days. It
// must be at least the longest window, so forgetting a key can only ever
// change seconds_since_first and seconds_since_last, never a count or sum.
const DefaultIdleTTL = 30 * data.SecondsPerDay

func checkTTL(ttl int64) int64 {
	if ttl == 0 {
		return DefaultIdleTTL
	}
	if ttl < maxWindow {
		panic("features: idle TTL shorter than the longest window")
	}
	return ttl
}

// unseen is what Read returns for a key with no live history.
func unseen(e Entity) Aggregates {
	a := Aggregates{Distinct: math.NaN()}
	if e.tracksDistinct() {
		a.Distinct = 0
	}
	return a
}
