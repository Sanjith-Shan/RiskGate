package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// Benchmarks for the assess path, at three depths:
//
//	BenchmarkAssessPipeline  features + model + rules on a parsed Txn: the
//	                         part that must not allocate
//	BenchmarkAssessHandler   the whole handler: body read, JSON decode,
//	                         pipeline, reasons, response, decision log
//	BenchmarkAssessHTTP      over a real loopback connection (httptest),
//	                         including net/http's own costs
//
// The stream's event time moves forward on every pass, so velocity state
// stays at a steady size instead of piling every pass into one window.
//
// By default they run the small test model, test rules and a synthetic
// stream. Two environment variables switch them to a real configuration
// (experiment 5's single-request cost):
//
//	RISKGATE_BENCH_MODEL=../../models/ieee    model directory; with it the
//	    benchmarks deploy rules/default.rules and rules/lists.json
//	    (override with RISKGATE_BENCH_RULES and RISKGATE_BENCH_LISTS)
//	RISKGATE_BENCH_REQUESTS=../../data/export_real/test_replay.jsonl
//	    assess requests to use as the stream, in file order
//
// Paths are relative to this package's directory, where go test runs.

const benchStream = 20000

var benchModel struct {
	once   sync.Once
	scorer *model.Scorer
	err    error
}

func benchService(b *testing.B) *Service {
	env := newTestEnv(b)
	cfg := env.config(b)
	cfg.SnapshotPath = ""
	if dir := os.Getenv("RISKGATE_BENCH_MODEL"); dir != "" {
		benchModel.once.Do(func() {
			benchModel.scorer, benchModel.err = model.LoadScorer(dir, schema.Default())
		})
		if benchModel.err != nil {
			b.Fatal(benchModel.err)
		}
		cfg.Scorer = benchModel.scorer
		cfg.Provenance = "IEEE-CIS (local only)"
		rulesPath := envOr("RISKGATE_BENCH_RULES", filepath.Join("..", "..", "rules", "default.rules"))
		listsPath := envOr("RISKGATE_BENCH_LISTS", filepath.Join("..", "..", "rules", "lists.json"))
		rs, err := os.ReadFile(rulesPath)
		if err != nil {
			b.Fatal(err)
		}
		if cfg.ListsJSON, err = os.ReadFile(listsPath); err != nil {
			b.Fatal(err)
		}
		cfg.Rules = string(rs)
	}
	s, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var benchRequests struct {
	once sync.Once
	ps   []payment
	err  error
}

// benchPayments is the stream: RISKGATE_BENCH_REQUESTS if set, else the
// synthetic test stream.
func benchPayments(b *testing.B, seed uint64) []payment {
	path := os.Getenv("RISKGATE_BENCH_REQUESTS")
	if path == "" {
		return testStream(benchStream, seed)
	}
	benchRequests.once.Do(func() {
		benchRequests.ps, benchRequests.err = readPayments(path)
	})
	if benchRequests.err != nil {
		b.Fatal(benchRequests.err)
	}
	return benchRequests.ps
}

func readPayments(path string) ([]payment, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []payment
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var r assessRequest
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, err
		}
		out = append(out, payment{ID: r.PaymentID, Created: r.Created, Cents: r.Amount, Fields: r.RiskFields})
	}
	return out, sc.Err()
}

func BenchmarkAssessPipeline(b *testing.B) {
	s := benchService(b)
	stream := benchPayments(b, 21)
	txns := make([]data.Txn, len(stream))
	for i, p := range stream {
		req := &assessRequest{PaymentID: p.ID, Created: p.Created, Amount: p.Cents, RiskFields: p.Fields}
		var err error
		if txns[i], err = s.txnOf(req); err != nil {
			b.Fatal(err)
		}
	}
	span := txns[len(txns)-1].DT - txns[0].DT + 1
	buf := s.bufs.Get().(*assessBuf)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t := txns[i%len(txns)]
		t.DT += int64(i/len(txns)) * span
		s.assess(&t, buf)
	}
}

// discardWriter is a ResponseWriter that allocates nothing per request.
type discardWriter struct{ h http.Header }

func (w *discardWriter) Header() http.Header         { return w.h }
func (w *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *discardWriter) WriteHeader(int)             {}

func BenchmarkAssessHandler(b *testing.B) {
	s := benchService(b)
	stream := benchPayments(b, 22)
	bodies := make([][]byte, len(stream))
	for i, p := range stream {
		bodies[i] = p.body()
	}
	h := s.Handler()
	w := &discardWriter{h: http.Header{}}
	body := bytes.NewReader(nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/assess", body)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i > 0 && i%len(bodies) == 0 {
			// Start over with fresh state rather than replay the same
			// event times into a full window.
			b.StopTimer()
			s = benchService(b)
			h = s.Handler()
			b.StartTimer()
		}
		body.Reset(bodies[i%len(bodies)])
		req.Body = io.NopCloser(body)
		h.ServeHTTP(w, req)
	}
}

func BenchmarkAssessHTTP(b *testing.B) {
	s := benchService(b)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	stream := benchPayments(b, 23)
	bodies := make([][]byte, len(stream))
	for i, p := range stream {
		bodies[i] = p.body()
	}
	client := srv.Client()
	url := srv.URL + "/v1/assess"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := client.Post(url, "application/json", bytes.NewReader(bodies[i%len(bodies)]))
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			b.Fatal(resp.Status)
		}
	}
}

// BenchmarkModel splits the model's share of the pipeline: Score (encode,
// sum the trees, calibrate), Contributions (Saabas, for the reasons), and
// ScoreContributions, the one walk that gives both and that the pipeline
// runs, on feature rows the engine computed for the bench stream.
func BenchmarkModel(b *testing.B) {
	s := benchService(b)
	if s.scorer == nil {
		b.Skip("no model")
	}
	stream := benchPayments(b, 24)
	rows := make([]schema.Row, len(stream))
	for i, p := range stream {
		t, err := s.txnOf(&assessRequest{PaymentID: p.ID, Created: p.Created, Amount: p.Cents, RiskFields: p.Fields})
		if err != nil {
			b.Fatal(err)
		}
		rows[i] = s.cat.NewRow()
		s.engine.ScoreAndUpdate(&t, rows[i])
	}
	x := make([]float64, s.scorer.NumFeatures())
	contrib := make([]float64, s.scorer.NumFeatures())
	b.Run("Score", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			s.scorer.Score(rows[i%len(rows)])
		}
	})
	b.Run("Contributions", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			s.scorer.Contributions(rows[i%len(rows)], x, contrib)
		}
	})
	b.Run("ScoreContributions", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			s.scorer.ScoreContributions(rows[i%len(rows)], x, contrib)
		}
	})
}
