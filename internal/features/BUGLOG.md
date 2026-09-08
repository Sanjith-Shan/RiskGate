# internal/features bug log

Real bugs in the velocity states, in the order they were found. Each has a
regression test.

## 1. The sketch reported never-seen keys as seen (2026-09-23)

**Found by** experiment 4 (`cmd/experiments/state`), which compares every
feature of every IEEE-CIS row between the exact state and the sketch
(`features.CompareStates`). The sketch had 392,560 missing-value mismatches
in the eight `seconds_since_first` / `seconds_since_last` features, and
`uid_seconds_since_first` was bit-exact on only 23.5% of rows. The unit
tests had not caught it: they check only that counts and sums never
undercount, and say nothing about recency.

**Root cause.** First and last seen were a min-sketch and a max-sketch of
event times (4 rows x 16,384 cells), shared by every key of every entity.
`Read` took, over the key's four cells, the latest of the minimums as first
seen and the earliest of the maximums as last seen. It called the key seen
when every cell held a time and that time was within the idle TTL. The
time bounds were one-sided, as documented, but membership was not.
Membership is the question "has any key touched all four of these cells?",
and with 214,468 keys over six months the answer is soon yes for every
cell. After that, a key never seen before reads as seen. Its first seen
comes from some other key, often months old, and its last seen from
whichever colliding key paid most recently. The last-seen time keeps the TTL
check passing, so nothing ever reads as unseen again. That error runs the
harmful way. `seconds_since_first` is missing for a brand-new customer or
card, and the model leans on exactly that value. The sketch turned it into
"known for months".

A sketch cannot fix this by being wider. A missing value is a statement that
nobody touched these cells, and any collision falsifies it. A count-min
sketch overcounts gracefully, but a membership bit has nothing to degrade
to.

**Fix.** First and last seen are no longer sketched
(`internal/features/sketch.go`). A fixed-size, 8-way set-associative table
(`SketchConfig.RecencySlots`, default 262,144 slots of 24 bytes, 6.3 MB)
stores each key's full 64-bit hash with its first and last times. It
follows Exact's rules. A key idle for the TTL reads as unseen, and it
starts over on its next event. When a set is full, the slot seen least
recently is replaced, and an idle key is always the least recent. The error
is one-sided in the safe direction. The table can forget a live key early
(it reads as unseen, or later with a first-seen time that is too recent).
It never reports a key as seen that was not, except on a full 64-bit hash
collision. When a key reads as seen, `Last` is exact and `First` is never
earlier than the truth, so `seconds_since_first` never overestimates. Memory
stays fixed. The price is a working-set size: with fewer live keys than
about half the slots, forgetting is rare. The count-min counts and sums are
untouched, so the sketch still never undercounts. The snapshot format moved
to `riskgate/sketch/v2`, so a v1 snapshot is refused rather than misread.

**Regression tests** (`sketch_internal_test.go`).
- `TestSketchNeverSeenKeyReadsUnseen` fills a small sketch with 2,000 keys
  and reads 1,000 never-added ones. Before the fix, all 1,000 read as seen.
- `TestSketchRecencyMatchesExact`: with room for every key, first and last
  seen match Exact bit for bit on 20,000 synthetic payments, TTL restarts
  included.
- `TestSketchRecencyErrorIsOneSided`: with 64 slots, far too few, keys are
  forgotten, and every error is one-sided. The sketch is never present where
  Exact is missing, `seconds_since_last` is exact, and `seconds_since_first`
  is never over.
The last two also failed before the fix. `TestSketchNeverUndercounts` still
passes.

**Effect on real data** (experiment 4, `results/state_shootout/`). Recency
missing-value mismatches went from 392,560 to 18. Those 18 are 9 payments
whose key was forgotten early, when more than 8 live keys shared a set. 25
`seconds_since_first` values came out too small, and none too large.
`uid_seconds_since_first` is bit-exact on 99.996% of rows, up from 23.5%.
With the unretrained exact-state model on the validation month, the
sketch's ROC-AUC went from 0.8314 to 0.8427 (exact 0.8450), and its PR-AUC
from 0.2867 to 0.2938 (exact 0.2983).
