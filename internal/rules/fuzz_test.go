package rules

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzParse feeds arbitrary text through the whole pipeline. For any input:
// nothing panics; every rule that parses prints to text that parses back to
// the same tree; and every rule that also checks compiles and evaluates.
func FuzzParse(f *testing.F) {
	for _, br := range benchRules {
		f.Add(br.src)
	}
	inputs, _ := filepath.Glob("testdata/errors/*.rule")
	for _, in := range inputs {
		if b, err := os.ReadFile(in); err == nil {
			f.Add(string(b))
		}
	}
	f.Add("shadow review if -(:amount: - -1) / 0 not in [1, -2.5] or not not true")
	f.Add(`allow if starts_with(lower("ÀBé"), :device_info:) and :card_type: != "x\"y"`)

	env := testEnv()
	row := GenerateRow(rand.New(rand.NewPCG(1, 1)), env.Catalog)
	f.Fuzz(func(t *testing.T, src string) {
		parsed, _ := Parse(src)
		for _, r := range parsed {
			text := r.String()
			again, err := ParseRule(text)
			if err != nil {
				t.Fatalf("printed rule does not parse: %q\n%v", text, err)
			}
			if !EqualRule(r, again) {
				t.Fatalf("round trip changed the rule:\n src   %q\n print %q\n want  %s\n got   %s", r.Text, text, sexpr(r.Cond), sexpr(again.Cond))
			}
			if Print(again.Cond) != Print(r.Cond) {
				t.Fatalf("printing is not a fixed point for %q", text)
			}
		}
		// Load exercises the checker, the linter and the compiler.
		rs, err := Load(src, env, 1)
		if err != nil {
			if ds, ok := err.(Diagnostics); ok {
				_ = ds.Render()
				return
			}
			t.Fatalf("Load returned a non-diagnostic error: %v", err)
		}
		rs.Evaluate(row)
		for _, cr := range rs.Rules {
			if got, want := cr.Match(row), refEval(cr.Rule.Cond, row).b; got != want {
				t.Fatalf("%s: compiled %v, reference %v", cr.Text, got, want)
			}
		}
	})
}

// FuzzGeneratedRoundTrip checks print/parse on generated trees and the
// compiled evaluator against the reference on generated rows, from a
// fuzzer-chosen seed, which explores far more of the generator's space than
// the fixed seeds in the unit tests.
func FuzzGeneratedRoundTrip(f *testing.F) {
	f.Add(uint64(1), uint64(2), uint8(3))
	f.Add(uint64(99), uint64(0), uint8(6))
	env := testEnv()
	f.Fuzz(func(t *testing.T, s1, s2 uint64, depth uint8) {
		rng := rand.New(rand.NewPCG(s1, s2))
		d := int(depth % 8)
		assertRoundTrip(t, GenerateExpr(rng, env, d))
		r := Generate(rng, env, d)
		cr, err := Compile(r)
		if err != nil {
			t.Fatal(err)
		}
		for range 8 {
			row := GenerateRow(rng, env.Catalog)
			if got, want := cr.Match(row), refEval(r.Cond, row).b; got != want {
				t.Fatalf("%s: compiled %v, reference %v on num %v str %q", r, got, want, row.Num, row.Str)
			}
		}
	})
}

// FuzzLower checks the allocation-free lowering helpers against
// strings.ToLower on arbitrary bytes, including invalid UTF-8.
func FuzzLower(f *testing.F) {
	f.Add("Straße", "strasse")
	f.Add("\xff", "�")
	f.Add("İ", "i̇")
	f.Fuzz(func(t *testing.T, s, u string) {
		checkFold(t, s, u)
		checkFold(t, s, strings.ToLower(s))
	})
}
