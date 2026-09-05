package service

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
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

const benchStream = 20000

func benchService(b *testing.B) *Service {
	env := newTestEnv(b)
	cfg := env.config(b)
	cfg.SnapshotPath = ""
	s, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

func BenchmarkAssessPipeline(b *testing.B) {
	s := benchService(b)
	stream := testStream(benchStream, 21)
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
	stream := testStream(benchStream, 22)
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
	stream := testStream(benchStream, 23)
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
