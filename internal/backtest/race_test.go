//go:build race

package backtest

// raceEnabled shrinks the largest tests under the race detector, which
// slows the closure evaluator by an order of magnitude. The full sizes run
// without -race, and cmd/backtest difftest runs at full scale.
const raceEnabled = true
