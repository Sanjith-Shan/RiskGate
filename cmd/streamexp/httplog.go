package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// httplog replays every payment through the HTTP service, one request at a
// time in event-time order (experiment 2's method), and keeps the decision
// log for k1 to compare the stream with.
func httplog(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("httplog", flag.ExitOnError)
	var c common
	c.register(fs, "httplog")
	port := fs.Int("port", 18181, "port for the service")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.setup("httplog"); err != nil {
		return err
	}
	e := &c.env
	replay := filepath.Join("build", "replay_all.jsonl")
	if _, err := os.Stat(replay); err != nil {
		if _, err := e.run(ctx, "serveparity", "replayfile", "-data", e.data, "-out", replay); err != nil {
			return err
		}
	}
	prov := provenance()
	prov.Load = baseline(10 * time.Second)

	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	cmd := exec.Command(e.exe("riskgate"), "serve", "-addr", fmt.Sprintf("127.0.0.1:%d", *port), "-model", e.model,
		"-rules", e.rules, "-lists", e.lists, "-state", "exact", "-sharding", "sharded", "-shards", "64",
		"-snapshot-dir", e.work, "-snapshot-interval", "0", "-log-level", "warn")
	cmd.Env = append(os.Environ(), "RISKGATE_WEBHOOK_SECRETS=whsec_exp_"+hex.EncodeToString(secret))
	logf, err := os.Create(filepath.Join(e.work, "serve.log"))
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	url := fmt.Sprintf("http://127.0.0.1:%d", *port)
	for i := 0; ; i++ {
		if resp, err := http.Get(url + "/healthz"); err == nil {
			resp.Body.Close()
			break
		}
		if i > 120 {
			return fmt.Errorf("service did not start; see %s", logf.Name())
		}
		time.Sleep(time.Second)
	}
	sampler := startSampler()
	start := time.Now()
	out, err := e.run(ctx, "serveparity", "send", "-input", replay, "-url", url+"/v1/assess")
	if err != nil {
		sampler.finish()
		return err
	}
	took := time.Since(start)
	prov.Load.RunCPUPctMean, prov.Load.RunCPUPctMax = sampler.finish()
	// The decision log's writer flushes every 200 ms; give it a few rounds,
	// then stop the service (killing it loses nothing already written).
	time.Sleep(3 * time.Second)
	logPath := filepath.Join(e.work, "decisions.jsonl")
	fmt.Printf("HTTP decision log: %s (%s)\n", logPath, strings.TrimSpace(string(out)))
	return appendRow(c.results, row{
		Experiment: "httplog", Run: c.runID, Provenance: prov,
		Config: map[string]any{"model": e.model, "rules": e.rules, "state": "exact/sharded/64", "requests": "sequential, one in flight"},
		Result: map[string]any{"send": strings.TrimSpace(string(out)), "took_s": took.Seconds(), "decision_log": logPath},
		Note:   "the reference decision log for k1; its timing is not a throughput claim",
	})
}
