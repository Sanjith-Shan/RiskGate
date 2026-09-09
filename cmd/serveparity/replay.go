package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
)

// assessLine is an assess request body: data.ReplayEvent without the label.
type assessLine struct {
	PaymentID  string         `json:"payment_id"`
	Created    int64          `json:"created"`
	Amount     int64          `json:"amount"`
	Currency   string         `json:"currency"`
	RiskFields map[string]any `json:"risk_fields"`
}

func replayFile(args []string) error {
	fs := flag.NewFlagSet("replayfile", flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory (raw/, cache/)")
	out := fs.String("out", "data/replay_all.jsonl", "output JSONL (row-level: keep it under data/)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ds, err := data.Load(data.RealPaths(*dataDir))
	if err != nil {
		return err
	}
	if ds.Synthetic {
		return errors.New("refusing: the dataset is SYNTHETIC")
	}
	n, err := writeReplay(*out, ds.Txns)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %d assess requests (all months, (DT, ID) order, no labels) to %s\n", n, *out)
	return nil
}

func writeReplay(path string, txns []data.Txn) (int, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(w)
	for i := range txns {
		if i > 0 && !data.Less(&txns[i-1], &txns[i]) {
			return i, fmt.Errorf("transactions not in (DT, ID) order at index %d", i)
		}
		ev := data.NewReplayEvent(&txns[i])
		if err := enc.Encode(assessLine{ev.PaymentID, ev.Created, ev.Amount, ev.Currency, ev.RiskFields}); err != nil {
			return i, err
		}
	}
	if err := w.Flush(); err != nil {
		return len(txns), err
	}
	return len(txns), f.Close()
}

func send(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	input := fs.String("input", "data/replay_all.jsonl", "assess requests, one per line, in order")
	url := fs.String("url", "http://127.0.0.1:8080/v1/assess", "assess endpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer f.Close()
	n, elapsed, err := sendAll(f, *url, os.Stderr)
	if err != nil {
		return err
	}
	fmt.Printf("sent %d requests sequentially in %v (%.0f req/s), every response 200\n",
		n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds())
	return nil
}

// sendAll posts every line in order, one request in flight at a time, over
// one keep-alive connection, and fails on the first non-200.
func sendAll(r io.Reader, url string, progress io.Writer) (int, time.Duration, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 1,
			DisableCompression:  true,
		},
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	start := time.Now()
	n := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		resp, err := client.Post(url, "application/json", bytes.NewReader(line))
		if err != nil {
			return n, time.Since(start), fmt.Errorf("request %d: %w", n+1, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return n, time.Since(start), fmt.Errorf("request %d: %s: %s", n+1, resp.Status, bytes.TrimSpace(body))
		}
		n++
		if n%50000 == 0 {
			fmt.Fprintf(progress, "sent %d in %v\n", n, time.Since(start).Round(time.Second))
		}
	}
	return n, time.Since(start), sc.Err()
}
