package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
	"time"
)

func TestEnvOverridesFlagsNotSetOnCommandLine(t *testing.T) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	c := serveFlags(fs)
	t.Setenv("RISKGATE_SHARDS", "16")
	t.Setenv("RISKGATE_SNAPSHOT_INTERVAL", "5s")
	t.Setenv("RISKGATE_ADDR", ":9999")
	if err := fs.Parse([]string{"-addr", ":7000"}); err != nil {
		t.Fatal(err)
	}
	if err := envOverrides(fs); err != nil {
		t.Fatal(err)
	}
	if c.shards != 16 || c.snapshotInterval != 5*time.Second || c.addr != ":7000" {
		t.Fatalf("got shards %d, interval %v, addr %q", c.shards, c.snapshotInterval, c.addr)
	}
	t.Setenv("RISKGATE_SHARDS", "many")
	fs = flag.NewFlagSet("serve", flag.ContinueOnError)
	serveFlags(fs)
	_ = fs.Parse(nil)
	if err := envOverrides(fs); err == nil || !strings.Contains(err.Error(), "RISKGATE_SHARDS") {
		t.Fatalf("bad env value: %v", err)
	}
}

func TestUnder(t *testing.T) {
	for _, tc := range []struct{ dir, path, want string }{
		{"var", "", "var/x.jsonl"},
		{"var", "other.jsonl", "other.jsonl"},
		{"var", "off", ""},
		{"", "", ""},
	} {
		if got := under(tc.dir, tc.path, "x.jsonl"); got != tc.want {
			t.Errorf("under(%q, %q) = %q, want %q", tc.dir, tc.path, got, tc.want)
		}
	}
}

func TestAuditCommandNeedsHistory(t *testing.T) {
	var out bytes.Buffer
	if _, err := audit([]string{"-rules-history", t.TempDir(), "-log", "/nonexistent"}, &out); err == nil {
		t.Fatal("audit with an empty history should fail")
	}
}
