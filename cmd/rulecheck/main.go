// Command rulecheck validates a RiskGate rule file the way the service would
// before swapping it in, and prints every error and warning with carets.
//
//	rulecheck [-lists lists.json] [-fmt] rules.txt
//
// The lists file maps each list name to an array of strings or of numbers:
//
//	{"trusted_domains": ["gmail.com", "icloud.com"], "blocked_regions": [123, 204]}
//
// With -fmt, rulecheck prints each rule in canonical form. It exits 1 if the
// file has errors, 2 on bad usage.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

func main() {
	listsPath := flag.String("lists", "", "JSON file of named lists")
	format := flag.Bool("fmt", false, "print the rules in canonical form")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: rulecheck [-lists lists.json] [-fmt] rules.txt")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	os.Exit(run(flag.Arg(0), *listsPath, *format))
}

func run(path, listsPath string, format bool) int {
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	env := rules.Env{Catalog: schema.Default()}
	if listsPath != "" {
		if env.Lists, err = loadLists(listsPath); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", listsPath, err)
			return 2
		}
	}
	rs, err := rules.Load(string(src), env, 0)
	var ds rules.Diagnostics
	if err != nil && !errors.As(err, &ds) {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err == nil {
		ds = rs.Warnings
	}
	if len(ds) > 0 {
		fmt.Println(ds.Render())
		fmt.Println()
	}
	if err != nil {
		fmt.Printf("%s: rule set rejected\n", path)
		return 1
	}
	if format {
		for _, r := range rs.Rules {
			fmt.Println(r.Rule)
		}
		return 0
	}
	plural := "s"
	if len(ds) == 1 {
		plural = ""
	}
	fmt.Printf("%s: %d rules OK, %d warning%s\n", path, len(rs.Rules), len(ds), plural)
	return 0
}

func loadLists(path string) (rules.Lists, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return rules.ParseLists(b)
}
