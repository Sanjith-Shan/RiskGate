package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

const testSecret = "whsec_test_secret"

// testRules exercises every action, a named list, risk_score, and shadow
// rules that match a fair share of the test stream.
const testRules = `# test rule set
allow  if :purchaser_email_domain: in @trusted and :risk_score: < 5
block  if :card_txn_count_1h: >= 6
block  if :risk_score: >= 60
review if :purchaser_email_domain: = "anonymous.com" and :amount: > 100
shadow block  if :distinct_cards_per_device_24h: >= 22
shadow review if :amount: > 150
`

const testLists = `{"trusted": ["yahoo.com"]}`

// testEnv is a service's on-disk configuration, reusable across restarts.
type testEnv struct {
	dir    string
	scorer *model.Scorer
	table  *backtest.Table
}

func newTestEnv(t testing.TB) *testEnv {
	t.Helper()
	cat := schema.Default()
	sc, err := model.LoadScorer(filepath.Join("testdata", "model"), cat)
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{dir: t.TempDir(), scorer: sc}
}

// config returns a full configuration: fresh velocity state, and every
// file (snapshot, logs, rule history) under the env's directory, so a new
// service from the same env is a restart.
func (e *testEnv) config(t testing.TB) Config {
	t.Helper()
	st, desc, err := NewState("exact", "sharded", 8)
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		State:           st,
		StateDesc:       desc,
		Scorer:          e.scorer,
		Rules:           testRules,
		ListsJSON:       []byte(testLists),
		RulesHistoryDir: filepath.Join(e.dir, "rules-history"),
		Table:           e.table,
		DecisionLog:     filepath.Join(e.dir, "decisions.jsonl"),
		LabelLog:        filepath.Join(e.dir, "labels.jsonl"),
		SnapshotPath:    filepath.Join(e.dir, "riskgate.snap"),
		WebhookSecrets:  []string{testSecret},
		Provenance:      "SYNTHETIC DATA",
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (e *testEnv) start(t testing.TB) *Service {
	t.Helper()
	s, err := New(e.config(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// payment is one assess request of the test stream.
type payment struct {
	ID      string
	Created int64
	Cents   int64
	Fields  map[string]any
}

func (p payment) body() []byte {
	b, err := json.Marshal(map[string]any{
		"payment_id": p.ID, "created": p.Created, "amount": p.Cents, "currency": "usd", "risk_fields": p.Fields,
	})
	if err != nil {
		panic(err)
	}
	return b
}

// testStream is a deterministic stream of payments over a few cards,
// devices and email domains, with card-testing bursts, in event-time order.
func testStream(n int, seed uint64) []payment {
	rng := rand.New(rand.NewPCG(seed, 42))
	emails := []string{"gmail.com", "anonymous.com", "yahoo.com", "hotmail.com"}
	devices := []string{"iOS Device", "Windows", "SM-G930V", "MacOS", "Trident/7.0", ""}
	created := int64(1525132800) // event time; any fixed start works
	out := make([]payment, 0, n)
	burstCard := 0.0
	for i := range n {
		created += 1 + rng.Int64N(240)
		card := float64(1000 + rng.IntN(25))
		device := devices[rng.IntN(len(devices))]
		if i%40 >= 30 { // a burst: one card, many payments, one device
			if i%40 == 30 {
				burstCard = float64(1000 + rng.IntN(25))
			}
			card = burstCard
			device = "SM-G930V"
			created -= rng.Int64N(200) // bunch them up, but never go backwards past the last
			created = max(created, out[len(out)-1].Created)
		}
		dollars := float64(rng.IntN(30000)) / 100
		fields := map[string]any{
			"product_code":   []string{"W", "C", "R", "H"}[rng.IntN(4)],
			"card1":          card,
			"card4":          "visa",
			"card6":          "debit",
			"addr1":          float64(100 + rng.IntN(5)),
			"addr2":          87.0,
			"D1":             float64(rng.IntN(3)),
			"P_emaildomain":  emails[rng.IntN(len(emails))],
			"TransactionAmt": dollars,
		}
		if device != "" {
			fields["DeviceInfo"] = device
			fields["DeviceType"] = "mobile"
		}
		out = append(out, payment{
			ID:      fmt.Sprintf("txn_%d", 3_000_000+i),
			Created: created,
			Cents:   int64(dollars*100 + 0.5),
			Fields:  fields,
		})
	}
	return out
}

// assessResponse is the contract's response body.
type assessResponse struct {
	AssessmentID   string   `json:"assessment_id"`
	Decision       string   `json:"decision"`
	RiskScore      int      `json:"risk_score"`
	MatchedRule    *string  `json:"matched_rule"`
	Reasons        []string `json:"reasons"`
	RulesetVersion uint64   `json:"ruleset_version"`
}

// decisionOnly strips the per-process assessment id for comparisons.
func (r assessResponse) decisionOnly() assessResponse {
	r.AssessmentID = ""
	return r
}

// do sends one request to h and returns the recorder.
func do(t testing.TB, h http.Handler, method, path string, body []byte, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// assess sends p and decodes a 200 response.
func assess(t testing.TB, s *Service, p payment, header ...string) assessResponse {
	t.Helper()
	rec := do(t, s.Handler(), http.MethodPost, "/v1/assess", p.body(), header...)
	if rec.Code != http.StatusOK {
		t.Fatalf("assess %s: %d %s", p.ID, rec.Code, rec.Body)
	}
	var r assessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("assess %s: %v in %s", p.ID, err, rec.Body)
	}
	return r
}

func decodeJSON(t testing.TB, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%v in %s", err, b)
	}
}

// fixedClock is a settable wall clock for TTL tests.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }
