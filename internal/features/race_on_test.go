//go:build race

package features

// raceEnabled shrinks the heaviest tests under the race detector, which
// slows this code about tenfold. The logic checked is the same.
const raceEnabled = true
