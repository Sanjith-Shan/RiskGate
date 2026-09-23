package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
)

// Encoder turns a Row into the model's input vector. Numbers pass through
// (NaN stays NaN). Strings become integer category codes from a dictionary
// fitted on the training months; an unseen or missing string becomes NaN,
// which LightGBM treats as missing.
//
// The Go export writes exactly Encode's output, and Python trains on that file
// without re-deriving anything. The same Encoder runs in the service. That is
// how train/serve parity holds for the input side.
type Encoder struct {
	Features    []string            `json:"features"`    // model input order
	Categorical map[string][]string `json:"categorical"` // field -> code -> value
	slots       []encSlot
	codes       []map[string]float64
}

type encSlot struct {
	kind Kind
	slot int
}

// FitEncoder builds an encoder over the given feature names. seen reports the
// distinct values of each string feature on the training rows; codes are
// assigned in sorted order so the result is deterministic.
func FitEncoder(c *Catalog, features []string, seen map[string]map[string]struct{}) (*Encoder, error) {
	e := &Encoder{Features: features, Categorical: map[string][]string{}}
	for _, name := range features {
		f, ok := c.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("encoder: unknown feature %q", name)
		}
		if f.Kind == String {
			vals := make([]string, 0, len(seen[name]))
			for v := range seen[name] {
				if v != "" {
					vals = append(vals, v)
				}
			}
			sort.Strings(vals)
			e.Categorical[name] = vals
		}
	}
	return e, e.Bind(c)
}

// Bind resolves feature names against a catalog. Called after loading JSON.
func (e *Encoder) Bind(c *Catalog) error {
	e.slots = make([]encSlot, len(e.Features))
	e.codes = make([]map[string]float64, len(e.Features))
	for i, name := range e.Features {
		f, ok := c.Lookup(name)
		if !ok {
			return fmt.Errorf("encoder: unknown feature %q", name)
		}
		e.slots[i] = encSlot{f.Kind, f.Slot}
		if f.Kind == String {
			m := make(map[string]float64, len(e.Categorical[name]))
			for code, v := range e.Categorical[name] {
				m[v] = float64(code)
			}
			e.codes[i] = m
		}
	}
	return nil
}

// Encode writes the model input for r into out, which must have len(Features).
func (e *Encoder) Encode(r Row, out []float64) {
	for i, s := range e.slots {
		if s.kind == Number {
			out[i] = r.Num[s.slot]
			continue
		}
		code, ok := e.codes[i][r.Str[s.slot]]
		if !ok {
			code = math.NaN()
		}
		out[i] = code
	}
}

// IsCategorical reports whether model input i is a category code.
func (e *Encoder) IsCategorical(i int) bool { return e.slots[i].kind == String }

func (e *Encoder) Save(path string) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func LoadEncoder(path string, c *Catalog) (*Encoder, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e Encoder
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	return &e, e.Bind(c)
}
