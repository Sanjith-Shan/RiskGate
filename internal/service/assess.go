package service

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// POST /v1/assess, the contract agreed with Clearinghouse:
//
//	request  {"payment_id", "created" (event time, Unix s), "amount" (cents),
//	          "currency", "risk_fields": {IEEE-CIS columns, see data.RF*}}
//	headers  Idempotency-Key: <payment_id>:<attempt>   (optional)
//	         RiskGate-Deadline-Ms: <ms>                 (informational)
//	response {"assessment_id", "decision": allow|block|review,
//	          "risk_score": 0-99, "matched_rule": text|null,
//	          "reasons": [<= 3 strings], "ruleset_version"}
//
// The deadline is Clearinghouse's to enforce: it stops waiting and fails
// open. RiskGate always answers, because a late answer still updated the
// velocity state and still belongs in the decision log; it only counts the
// miss.

const (
	// MaxAssessBody bounds a request body. A real one is under 1 KB.
	MaxAssessBody = 64 << 10

	headerIdempotencyKey = "Idempotency-Key"
	headerDeadline       = "RiskGate-Deadline-Ms"

	// assessmentIDLen is len("asmt_") + 8 hex boot id + 16 hex sequence.
	assessmentIDLen = 5 + 8 + 16
)

type assessRequest struct {
	PaymentID  string         `json:"payment_id"`
	Created    int64          `json:"created"`
	Amount     int64          `json:"amount"`
	Currency   string         `json:"currency"`
	RiskFields map[string]any `json:"risk_fields"`
}

// assessBuf is the per-request scratch space, pooled so that a request in
// steady state allocates only what JSON decoding and the reason strings
// need.
type assessBuf struct {
	body    []byte
	req     assessRequest
	row     schema.Row
	x       []float64 // model inputs
	contrib []float64 // Saabas contributions
	out     []byte
	reasons []string
	shadow  []*rules.CompiledRule
}

func newAssessBuf(cat *schema.Catalog, modelInputs int) *assessBuf {
	return &assessBuf{
		body:    make([]byte, 0, 2048),
		req:     assessRequest{RiskFields: make(map[string]any, 16)},
		row:     cat.NewRow(),
		x:       make([]float64, modelInputs),
		contrib: make([]float64, modelInputs),
		out:     make([]byte, 0, 1024),
		reasons: make([]string, 0, model.MaxReasons),
		shadow:  make([]*rules.CompiledRule, 0, 8),
	}
}

// assessment is the outcome of the pipeline for one payment.
type assessment struct {
	rv        *ruleVersion
	d         rules.Decision
	scored    bool
	riskScore int
	prob, raw float64
	bias      float64
}

var (
	errBodyTooLarge = errors.New("body too large")
	contentTypeJSON = []string{"application/json"}
)

func (s *Service) handleAssess(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	code := s.serveAssess(w, r, start)
	s.metrics.count(routeAssess, code)
}

// serveAssess handles one request and returns the status it wrote.
func (s *Service) serveAssess(w http.ResponseWriter, r *http.Request, start time.Time) int {
	b := s.bufs.Get().(*assessBuf)
	defer s.bufs.Put(b)

	var err error
	if b.body, err = readLimited(r.Body, b.body[:0], MaxAssessBody); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			return writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body over 64 KiB")
		}
		return writeError(w, http.StatusBadRequest, "unreadable_body", err.Error())
	}

	// Idempotency first: a replayed key must not touch the velocity state.
	var claim *idemEntry
	if key := r.Header.Get(headerIdempotencyKey); key != "" {
		stored, owned, err := s.idem.claim(r.Context(), key, fnv64a(b.body))
		switch {
		case errors.Is(err, errKeyReused):
			return writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused", err.Error())
		case err != nil:
			return writeError(w, http.StatusServiceUnavailable, "cancelled", err.Error())
		case stored != nil:
			s.metrics.idempotentHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			_, _ = w.Write(stored)
			return http.StatusOK
		}
		claim = owned
	}
	code := s.assessBody(w, r, b, start)
	if claim != nil {
		if code == http.StatusOK {
			s.idem.finish(claim, append([]byte(nil), b.out...))
		} else {
			s.idem.abandon(claim)
		}
	}
	return code
}

// assessBody parses, assesses, responds and logs.
func (s *Service) assessBody(w http.ResponseWriter, r *http.Request, b *assessBuf, start time.Time) int {
	// Reset every field: Unmarshal leaves absent ones untouched, and the
	// buffer holds the previous request's. The map is cleared and reused.
	clear(b.req.RiskFields)
	b.req = assessRequest{RiskFields: b.req.RiskFields}
	if err := json.Unmarshal(b.body, &b.req); err != nil {
		return writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
	}
	if b.req.RiskFields == nil { // "risk_fields": null
		b.req.RiskFields = make(map[string]any, 16)
	}
	t, err := s.txnOf(&b.req)
	if err != nil {
		return writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	}

	a := s.assess(&t, b)
	id := s.nextAssessmentID()

	b.reasons = b.reasons[:0]
	if a.d.Rule != nil {
		b.reasons = append(b.reasons, a.rv.reason[a.d.Rule.Index])
	}
	if a.scored {
		for _, reason := range s.scorer.ExplainContributions(b.row, b.x, b.contrib, model.MaxReasons-len(b.reasons)) {
			b.reasons = append(b.reasons, reason.Text)
		}
	}

	b.out = appendAssessResponse(b.out[:0], id[:], &a, b.reasons)
	// Assigning the header slice directly skips Header.Set's per-call
	// allocation; net/http only reads it.
	w.Header()["Content-Type"] = contentTypeJSON
	_, _ = w.Write(b.out)

	elapsed := time.Since(start)
	s.metrics.latency.observe(elapsed)
	var deadline int64
	if v := r.Header.Get(headerDeadline); v != "" {
		deadline, _ = strconv.ParseInt(v, 10, 64)
	}
	if deadline > 0 && elapsed > time.Duration(deadline)*time.Millisecond {
		s.metrics.deadlineExceeded.Add(1)
	}
	s.logDecision(id, &b.req, &a, b, elapsed, deadline)
	return http.StatusOK
}

// txnOf turns a request into the transaction the feature engine reads.
// risk_fields is parsed by data.ParseRiskFields, the parser the replay
// export uses, so an online payment and its offline row cannot disagree
// about a field.
func (s *Service) txnOf(req *assessRequest) (data.Txn, error) {
	if req.PaymentID == "" {
		return data.Txn{}, errors.New("payment_id is required")
	}
	if req.Created <= 0 {
		return data.Txn{}, errors.New("created (event time, Unix seconds) is required")
	}
	if req.Amount < 0 {
		return data.Txn{}, errors.New("amount must not be negative")
	}
	t, err := data.ParseRiskFields(req.RiskFields)
	if err != nil {
		return t, err
	}
	// risk_fields.TransactionAmt carries the exact dollar amount the model
	// was trained on; the cents are rounded. Use the cents only without it.
	if math.IsNaN(t.Amount) {
		t.Amount = float64(req.Amount) / 100
	}
	t.DT = data.DTFromUnix(req.Created)
	if id, ok := transactionID(req.PaymentID); ok {
		t.ID = id // ties in event time only matter to Replay, but keep it faithful
	}
	return t, nil
}

// assess is the pipeline: features (score before update, atomic per key),
// model, rules. It allocates nothing unless a shadow rule matches.
func (s *Service) assess(t *data.Txn, b *assessBuf) assessment {
	s.engine.ScoreAndUpdate(t, b.row)
	var a assessment
	if s.scorer != nil {
		a.riskScore, a.prob, a.raw = s.scorer.Score(b.row)
		a.bias = s.scorer.Contributions(b.row, b.x, b.contrib)
		a.scored = true
		b.row.Num[s.riskSlot] = float64(a.riskScore)
	} else {
		a.prob, a.raw, a.bias = math.NaN(), math.NaN(), math.NaN()
	}
	a.rv = s.rules.current()
	a.d = a.rv.Set.EvaluateAppend(b.row, b.shadow)
	if cap(a.d.Shadow) > cap(b.shadow) {
		b.shadow = a.d.Shadow // grew: keep the larger buffer
	}

	s.metrics.decisions[a.d.Action].Add(1)
	if a.d.Rule != nil {
		a.rv.count[a.d.Rule.Index].Add(1)
	}
	for _, st := range a.rv.live {
		st.evaluated.Add(1)
	}
	for _, r := range a.d.Shadow {
		a.rv.shadow[r.Index].matched.Add(1)
	}
	return a
}

// nextAssessmentID returns "asmt_<boot id><sequence>" without allocating.
// Unique across restarts through the random boot id, and ordered within
// one process.
func (s *Service) nextAssessmentID() (id [assessmentIDLen]byte) {
	const hexdigits = "0123456789abcdef"
	copy(id[:], "asmt_")
	copy(id[5:], s.bootID[:])
	n := s.seq.Add(1)
	for i := assessmentIDLen - 1; i >= 13; i-- {
		id[i] = hexdigits[n&0xf]
		n >>= 4
	}
	return id
}

func appendAssessResponse(b, id []byte, a *assessment, reasons []string) []byte {
	b = append(b, `{"assessment_id":"`...)
	b = append(b, id...)
	b = append(b, `","decision":"`...)
	b = append(b, a.d.Action.String()...)
	b = append(b, `","risk_score":`...)
	b = strconv.AppendInt(b, int64(a.riskScore), 10)
	b = append(b, `,"matched_rule":`...)
	if a.d.Rule != nil {
		b = appendString(b, a.d.Rule.Text)
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"reasons":[`...)
	for i, r := range reasons {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendString(b, r)
	}
	b = append(b, `],"ruleset_version":`...)
	b = strconv.AppendUint(b, a.d.Version, 10)
	return append(b, '}', '\n')
}

// logDecision hands the decision to the log writer; see DecisionLog for why
// it never blocks.
func (s *Service) logDecision(id [assessmentIDLen]byte, req *assessRequest, a *assessment, b *assessBuf, elapsed time.Duration, deadline int64) {
	if s.dlog == nil {
		return
	}
	rec := s.dlog.get()
	rec.assessmentID = id
	rec.paymentID = req.PaymentID
	rec.created = req.Created
	rec.at = s.now().UnixMilli()
	rec.version = a.d.Version
	rec.action = a.d.Action
	rec.ruleID = ""
	if a.d.Rule != nil {
		rec.ruleID = a.d.Rule.ID
	}
	for _, r := range a.d.Shadow {
		rec.shadow = append(rec.shadow, r.ID)
	}
	rec.scored, rec.riskScore, rec.prob, rec.raw, rec.bias = a.scored, a.riskScore, a.prob, a.raw, a.bias
	copy(rec.num, b.row.Num)
	copy(rec.str, b.row.Str)
	copy(rec.contrib, b.contrib)
	rec.latency = elapsed
	rec.deadlineMs = deadline
	s.dlog.put(rec)
}

// fnv64a hashes a request body for the idempotency store's reuse check.
// Inlined rather than hash/fnv, whose constructor allocates.
func fnv64a(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

// readLimited reads r into buf, failing with errBodyTooLarge past limit.
func readLimited(r io.Reader, buf []byte, limit int) ([]byte, error) {
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if len(buf) > limit {
			return buf, errBodyTooLarge
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return buf, err
		}
	}
}

// writeError writes {"error": {"code", "message"}} and returns code.
func writeError(w http.ResponseWriter, status int, code, msg string) int {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
	return status
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
