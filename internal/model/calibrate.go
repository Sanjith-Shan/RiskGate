package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
)

// Calibrator maps a raw score to a calibrated fraud probability. It is an
// isotonic regression fitted in Python (scikit-learn's IsotonicRegression,
// out_of_bounds="clip") on the validation month and exported as the knots of
// its piecewise-linear function.
//
// It is fitted on the raw score, not on sigmoid(raw). Isotonic regression is
// invariant to monotone transforms of its input, so the fit is the same
// either way, and working on raw keeps math.Exp and NumPy's exp, which are
// not guaranteed to agree to the last bit, out of the path.
type Calibrator struct {
	Method string    `json:"method"` // "isotonic"
	Input  string    `json:"input"`  // "raw_score"
	X      []float64 `json:"x"`      // strictly increasing knots (X_thresholds_)
	Y      []float64 `json:"y"`      // non-decreasing values in [0, 1] (y_thresholds_)
}

// LoadCalibrator reads calibration.json.
func LoadCalibrator(path string) (*Calibrator, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Calibrator
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Calibrator) validate() error {
	if c.Method != "isotonic" || c.Input != "raw_score" {
		return fmt.Errorf("calibration: want method isotonic on raw_score, got %q on %q", c.Method, c.Input)
	}
	if len(c.X) == 0 || len(c.X) != len(c.Y) {
		return errors.New("calibration: x and y must be non-empty and the same length")
	}
	for i := range c.X {
		if math.IsNaN(c.X[i]) || math.IsInf(c.X[i], 0) || !(c.Y[i] >= 0 && c.Y[i] <= 1) {
			return fmt.Errorf("calibration: bad knot %d", i)
		}
		if i > 0 && (c.X[i] <= c.X[i-1] || c.Y[i] < c.Y[i-1]) {
			return fmt.Errorf("calibration: knots not increasing at %d", i)
		}
	}
	return nil
}

// Apply returns the calibrated probability for a raw score: values outside
// the knots clamp to the end values (scikit-learn's out_of_bounds="clip") and
// values between knots interpolate linearly. It has np.interp's semantics and
// is written operation for operation like interpolate() in
// python/riskgate.py, which is what training and evaluation use.
//
// That Python function exists because np.interp itself is not reproducible to
// the last bit: NumPy's C loop computes slope*(x-xp[j]) + fp[j], and whether
// the compiler fuses that into one multiply-add depends on how NumPy was
// built (the arm64 macOS wheel fuses, a baseline x86-64 build cannot). Here
// and in Python each operation rounds separately, on every platform.
func (c *Calibrator) Apply(raw float64) float64 {
	xp, fp := c.X, c.Y
	n := len(xp)
	switch {
	case raw != raw:
		return raw
	case raw <= xp[0]:
		return fp[0]
	case raw >= xp[n-1]:
		return fp[n-1]
	}
	// Largest j with xp[j] <= raw; here xp[0] < raw < xp[n-1], so j < n-1.
	lo, hi := 0, n-1
	for hi-lo > 1 {
		mid := int(uint(lo+hi) >> 1)
		if raw < xp[mid] {
			hi = mid
		} else {
			lo = mid
		}
	}
	j := lo
	slope := (fp[j+1] - fp[j]) / (xp[j+1] - xp[j])
	// The float64 conversion forbids fusing into a multiply-add (Go spec,
	// "Arithmetic operators"), which arm64 would otherwise do.
	return float64(slope*(raw-xp[j])) + fp[j]
}

// RiskScore turns a calibrated probability into the 0-99 risk_score rules
// read: floor(p * 100), clamped to [0, 99]. It is a probability in percent,
// not a percentile: risk_score 20 means about a one-in-five chance of fraud
// as calibrated on the validation month, however many payments score above
// or below it. NaN, which a finite model cannot produce, maps to 0.
func RiskScore(p float64) int {
	s := math.Floor(p * 100)
	switch {
	case !(s > 0):
		return 0
	case s > 99:
		return 99
	}
	return int(s)
}
