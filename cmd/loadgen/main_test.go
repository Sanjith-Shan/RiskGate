package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseRates(t *testing.T) {
	for in, want := range map[string][]float64{
		"1000":           {1000},
		"1000, 2000,500": {1000, 2000, 500},
		"1000:3000:1000": {1000, 2000, 3000},
		"1000:2500:1000": {1000, 2000},
		"0.1:0.3:0.1":    {0.1, 0.2, 0.30000000000000004},
	} {
		got, err := parseRates(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("parseRates(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "0", "-5", "1000:10:5", "1:2:0", "1,,2"} {
		if _, err := parseRates(bad); err == nil {
			t.Errorf("parseRates(%q) accepted", bad)
		}
	}
}

func TestRunJSONSweep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"decision":"allow"}`))
	}))
	defer srv.Close()
	input := filepath.Join(t.TempDir(), "reqs.jsonl")
	if err := os.WriteFile(input, []byte(`{"payment_id":"p","amount":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), options{
		url: srv.URL, input: input, rates: "100,200", duration: 200 * time.Millisecond,
		timeout: time.Second, maxInFlight: 100, uniqueIDs: true, deadline: time.Second, jsonOut: true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	var out output
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout.String())
	}
	if len(out.Runs) != 2 || out.Runs[0].Sent != 20 || out.Runs[1].Sent != 40 {
		t.Fatalf("runs %+v", out.Runs)
	}
	if out.MaxRateWithinDeadline == nil || *out.MaxRateWithinDeadline != 200 {
		t.Fatalf("max rate within deadline %v", out.MaxRateWithinDeadline)
	}
	if out.Machine.GoVersion == "" || !strings.Contains(stderr.String(), "machine:") {
		t.Fatalf("machine label missing; stderr:\n%s", stderr.String())
	}
}

func TestRunRequiresInput(t *testing.T) {
	if err := run(context.Background(), options{}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted missing -input")
	}
}
