package rules

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

var update = flag.Bool("update", false, "rewrite the .golden files in testdata/errors")

func testEnv() Env {
	return Env{Catalog: schema.Default(), Lists: SampleLists()}
}

// TestGoldenDiagnostics loads each testdata/errors/*.rule file and compares
// every diagnostic it produces, rendered with carets, against the .golden
// file beside it. These files are the spec for what an analyst sees; review
// a diff in them the way you would review copy.
func TestGoldenDiagnostics(t *testing.T) {
	files, err := filepath.Glob("testdata/errors/*.rule")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no golden inputs found")
	}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".rule")
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			got := renderLoad(t, string(src))
			golden := strings.TrimSuffix(file, ".rule") + ".golden"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("diagnostics differ from %s\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
			}
		})
	}
}

func renderLoad(t *testing.T, src string) string {
	t.Helper()
	rs, err := Load(src, testEnv(), 1)
	var ds Diagnostics
	switch {
	case err == nil:
		ds = rs.Warnings
	case errors.As(err, &ds):
	default:
		t.Fatalf("Load returned a non-diagnostic error: %v", err)
	}
	if len(ds) == 0 {
		t.Fatal("expected at least one diagnostic")
	}
	return ds.Render() + "\n"
}
