// Command riskgate runs the RiskGate service and its offline companions.
//
//	riskgate [serve] [flags]   run the HTTP service (the default)
//	riskgate table [flags]     build the backtest feature table from the dataset
//	riskgate audit [flags]     replay a decision log and check every decision
//
// Every serve flag can also be set from the environment as RISKGATE_<NAME>,
// with dashes as underscores: -snapshot-dir is RISKGATE_SNAPSHOT_DIR. A flag
// given on the command line wins. Webhook secrets are normally set only by
// environment (RISKGATE_WEBHOOK_SECRETS), to keep them out of `ps`.
//
// Run `riskgate <command> -h` for each command's flags.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "table":
		err = table(args)
	case "audit":
		var ok bool
		ok, err = audit(args, os.Stdout)
		if err == nil && !ok {
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "riskgate: unknown command %q (want serve, table or audit)\n", cmd)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "riskgate:", err)
		os.Exit(1)
	}
}
