package model

import (
	"math"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// A velocity count or sum is missing when the payment has no key for the
// entity; the reason must say so instead of formatting NaN.
func TestDescribeMissingEntity(t *testing.T) {
	c := schema.Default()
	for name, want := range map[string]string{
		"device_amount_sum_24h": "no device to track on this payment",
		"card_txn_count_1h":     "no card to track on this payment",
		"uid_amount_sum_7d":     "no customer to track on this payment",
	} {
		if got := describe(c.MustLookup(name), math.NaN(), "", false, nil); got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}
