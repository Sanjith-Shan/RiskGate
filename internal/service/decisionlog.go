package service

import (
	"bufio"
	"cmp"
	"io"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// The decision log.
//
// Every assessment appends one JSON line holding everything needed to
// replay and explain it: the full feature vector in catalog order, the
// rule-set version, the decision and the rule that made it, the shadow
// rules that matched, the model's raw score and Saabas contributions.
// `riskgate audit` re-derives every decision from those lines.
//
// The log must never slow a payment down, so the request goroutine only
// copies the record into a pooled struct and hands it to a bounded channel;
// one background goroutine encodes, buffers and writes. When the channel is
// full, because the disk has stalled or cannot keep up, the record is
// dropped and counted (riskgate_decision_log_dropped_total) rather than
// blocking the payment. That is a deliberate trade: the payment path's
// latency budget is a hard promise to Clearinghouse, while the log is an
// audit trail, and a non-zero drop count is visible and alarmable. A system
// that needed a complete log would write to a durable queue with its own
// back-pressure, or fail closed; RiskGate's audit reports drops explicitly
// instead of silently passing over them.
//
// Records are buffered for up to FlushInterval, so a crash loses at most
// that much of the log; graceful shutdown drains and fsyncs it.

// DefaultLogBuffer is the channel capacity: about a second of traffic at
// the rates experiment 5 measures.
const DefaultLogBuffer = 16384

// DefaultFlushInterval bounds how long a record sits in the write buffer.
const DefaultFlushInterval = 200 * time.Millisecond

// logRecord is one decision. It is pooled; the slices keep their capacity.
type logRecord struct {
	assessmentID [assessmentIDLen]byte
	paymentID    string
	created      int64
	at           int64 // wall clock, Unix milliseconds
	version      uint64
	action       rules.Action
	ruleID       string
	shadow       []string
	scored       bool
	riskScore    int
	prob, raw    float64
	num          []float64
	str          []string
	bias         float64
	contrib      []float64
	latency      time.Duration
	deadlineMs   int64

	barrier chan struct{} // non-nil: a Sync marker, not a decision
}

// DecisionLog writes records in the background. The zero value is not
// usable; see NewDecisionLog. A nil *DecisionLog discards everything.
type DecisionLog struct {
	cat          *schema.Catalog
	modelInputs  []string // contribution names, in model order
	topN         int      // contributions to log; <= 0 means all
	catalogStamp string
	modelSHA256  string // model.Scorer.SHA256; "" when unknown

	ch   chan *logRecord
	pool sync.Pool
	done chan struct{}
	// mu makes a send on ch and Close exclusive, so that a handler still
	// running after shutdown's drain timeout drops its record rather than
	// sending on a closed channel. Senders share it; only Close takes it
	// exclusively.
	mu     sync.RWMutex
	closed bool
	w      *bufio.Writer
	f      *os.File

	written, dropped atomic.Uint64
	writeErrs        atomic.Uint64
}

// NewDecisionLog appends to path, creating it if needed. modelInputs names
// the contribution entries and modelSHA256 identifies the model (nil and ""
// when there is no model).
func NewDecisionLog(path string, cat *schema.Catalog, modelInputs []string, modelSHA256 string, topN, buffer int, flushEvery time.Duration) (*DecisionLog, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return newDecisionLog(f, cat, modelInputs, modelSHA256, topN, buffer, flushEvery), nil
}

func newDecisionLog(f *os.File, cat *schema.Catalog, modelInputs []string, modelSHA256 string, topN, buffer int, flushEvery time.Duration) *DecisionLog {
	if buffer <= 0 {
		buffer = DefaultLogBuffer
	}
	if flushEvery <= 0 {
		flushEvery = DefaultFlushInterval
	}
	l := &DecisionLog{
		cat: cat, modelInputs: modelInputs, topN: topN, catalogStamp: catalogStamp(cat), modelSHA256: modelSHA256,
		ch: make(chan *logRecord, buffer), done: make(chan struct{}),
		f: f, w: bufio.NewWriterSize(f, 256<<10),
	}
	nIn := len(modelInputs)
	l.pool.New = func() any {
		return &logRecord{
			num:     make([]float64, cat.NumCount()),
			str:     make([]string, cat.StrCount()),
			contrib: make([]float64, nIn),
		}
	}
	go l.run(flushEvery)
	return l
}

// get returns a record to fill; put hands it to the writer. A record from
// get must be passed to put exactly once.
func (l *DecisionLog) get() *logRecord { return l.pool.Get().(*logRecord) }

func (l *DecisionLog) put(r *logRecord) {
	l.mu.RLock()
	sent := false
	if !l.closed {
		select {
		case l.ch <- r:
			sent = true
		default:
		}
	}
	l.mu.RUnlock()
	if !sent {
		l.dropped.Add(1)
		l.recycle(r)
	}
}

func (l *DecisionLog) recycle(r *logRecord) {
	r.shadow = r.shadow[:0]
	r.paymentID, r.ruleID = "", ""
	l.pool.Put(r)
}

// Sync waits until every record handed over before the call is written to
// the file (not fsynced). Readers of the log file call it first.
func (l *DecisionLog) Sync() {
	if l == nil {
		return
	}
	b := &logRecord{barrier: make(chan struct{})}
	l.mu.RLock()
	if l.closed {
		l.mu.RUnlock()
		return
	}
	l.ch <- b // blocking on purpose: a reader asked to wait
	l.mu.RUnlock()
	<-b.barrier
}

// Close drains the channel, flushes and fsyncs. Records put after Close
// are dropped and counted, so it is safe to call while requests that
// outlived shutdown's drain timeout are still running.
func (l *DecisionLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	close(l.ch)
	l.mu.Unlock()
	<-l.done
	err := l.w.Flush()
	if fi, serr := l.f.Stat(); serr == nil && fi.Mode().IsRegular() {
		if serr := l.f.Sync(); err == nil {
			err = serr
		}
	}
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (l *DecisionLog) run(flushEvery time.Duration) {
	defer close(l.done)
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	var buf []byte
	var order []int
	for {
		select {
		case r, ok := <-l.ch:
			if !ok {
				return
			}
			if r.barrier != nil {
				l.flush()
				close(r.barrier)
				continue
			}
			buf, order = l.encode(buf[:0], r, order)
			l.recycle(r)
			if _, err := l.w.Write(buf); err != nil {
				l.writeErrs.Add(1)
				continue
			}
			l.written.Add(1)
		case <-tick.C:
			l.flush()
		}
	}
}

func (l *DecisionLog) flush() {
	if err := l.w.Flush(); err != nil {
		l.writeErrs.Add(1)
	}
}

// catalogStamp identifies the catalog's field order, so the audit can tell
// a log written under a different catalog from a wrong decision.
func catalogStamp(cat *schema.Catalog) string {
	h := uint64(14695981039346656037) // FNV-1a
	for _, f := range cat.Fields() {
		for i := 0; i < len(f.Name); i++ {
			h ^= uint64(f.Name[i])
			h *= 1099511628211
		}
		h ^= uint64(f.Kind)
		h *= 1099511628211
	}
	return strconv.FormatUint(h, 16)
}

// encode renders r as one JSON line. Field names are short but readable;
// LogEntry is the matching decoder.
func (l *DecisionLog) encode(b []byte, r *logRecord, order []int) ([]byte, []int) {
	b = append(b, '{')
	b = appendKey(b, "assessment_id", true)
	b = appendString(b, string(r.assessmentID[:]))
	b = appendKey(b, "payment_id", false)
	b = appendString(b, r.paymentID)
	b = appendKey(b, "created", false)
	b = strconv.AppendInt(b, r.created, 10)
	b = appendKey(b, "at_ms", false)
	b = strconv.AppendInt(b, r.at, 10)
	b = appendKey(b, "ruleset_version", false)
	b = strconv.AppendUint(b, r.version, 10)
	b = appendKey(b, "decision", false)
	b = appendString(b, r.action.String())
	b = appendKey(b, "rule_id", false)
	if r.ruleID == "" {
		b = append(b, "null"...)
	} else {
		b = appendString(b, r.ruleID)
	}
	b = appendKey(b, "shadow_matches", false)
	b = append(b, '[')
	for i, id := range r.shadow {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendString(b, id)
	}
	b = append(b, ']')
	b = appendKey(b, "scored", false)
	b = strconv.AppendBool(b, r.scored)
	b = appendKey(b, "risk_score", false)
	b = strconv.AppendInt(b, int64(r.riskScore), 10)
	b = appendKey(b, "probability", false)
	b = appendFloat(b, r.prob)
	b = appendKey(b, "raw_score", false)
	b = appendFloat(b, r.raw)
	b = appendKey(b, "latency_us", false)
	b = strconv.AppendFloat(b, float64(r.latency)/1e3, 'f', 1, 64)
	if r.deadlineMs > 0 {
		b = appendKey(b, "deadline_ms", false)
		b = strconv.AppendInt(b, r.deadlineMs, 10)
	}
	b = appendKey(b, "catalog", false)
	b = appendString(b, l.catalogStamp)
	if l.modelSHA256 != "" {
		b = appendKey(b, "model_sha256", false)
		b = appendString(b, l.modelSHA256)
	}

	// The full feature vector, in catalog order: a number, a string, or
	// null for missing.
	b = appendKey(b, "features", false)
	b = append(b, '[')
	for i, f := range l.cat.Fields() {
		if i > 0 {
			b = append(b, ',')
		}
		if f.Kind == schema.Number {
			b = appendFloat(b, r.num[f.Slot])
		} else if s := r.str[f.Slot]; s == "" {
			b = append(b, "null"...)
		} else {
			b = appendString(b, s)
		}
	}
	b = append(b, ']')

	if r.scored {
		// Saabas contributions in log-odds, largest magnitude first, so a
		// reader sees what drove the score without sorting.
		order = order[:0]
		for i := range r.contrib {
			order = append(order, i)
		}
		slices.SortFunc(order, func(i, j int) int {
			return cmp.Or(cmp.Compare(abs(r.contrib[j]), abs(r.contrib[i])), cmp.Compare(i, j))
		})
		if l.topN > 0 && len(order) > l.topN {
			order = order[:l.topN]
		}
		b = appendKey(b, "bias", false)
		b = appendFloat(b, r.bias)
		b = appendKey(b, "contributions", false)
		b = append(b, '{')
		for k, i := range order {
			b = appendKey(b, l.modelInputs[i], k == 0)
			b = appendFloat(b, r.contrib[i])
		}
		b = append(b, '}')
	}
	b = append(b, '}', '\n')
	return b, order
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// Stats for /metrics.
func (l *DecisionLog) stats() (written, dropped, errs uint64, queued int) {
	if l == nil {
		return 0, 0, 0, 0
	}
	return l.written.Load(), l.dropped.Load(), l.writeErrs.Load(), len(l.ch)
}

// LogEntry is a decoded decision-log line.
type LogEntry struct {
	AssessmentID   string             `json:"assessment_id"`
	PaymentID      string             `json:"payment_id"`
	Created        int64              `json:"created"`
	AtMs           int64              `json:"at_ms"`
	RulesetVersion uint64             `json:"ruleset_version"`
	Decision       string             `json:"decision"`
	RuleID         *string            `json:"rule_id"`
	ShadowMatches  []string           `json:"shadow_matches"`
	Scored         bool               `json:"scored"`
	RiskScore      int                `json:"risk_score"`
	Probability    *float64           `json:"probability"`
	RawScore       *float64           `json:"raw_score"`
	LatencyUs      float64            `json:"latency_us"`
	Catalog        string             `json:"catalog"`
	ModelSHA256    string             `json:"model_sha256"`
	Features       []any              `json:"features"`
	Bias           *float64           `json:"bias"`
	Contributions  map[string]float64 `json:"contributions"`
}

// Row rebuilds the logged feature vector for cat, which must be the catalog
// the log was written with (see Catalog).
func (e *LogEntry) Row(cat *schema.Catalog) (schema.Row, error) {
	fields := cat.Fields()
	if len(e.Features) != len(fields) {
		return schema.Row{}, errFeatureCount(len(e.Features), len(fields))
	}
	row := cat.NewRow()
	for i, f := range fields {
		v := e.Features[i]
		if v == nil {
			continue // NewRow is all missing
		}
		switch f.Kind {
		case schema.Number:
			x, ok := v.(float64)
			if !ok {
				return schema.Row{}, errFeatureType(f.Name, v)
			}
			row.Num[f.Slot] = x
		case schema.String:
			s, ok := v.(string)
			if !ok {
				return schema.Row{}, errFeatureType(f.Name, v)
			}
			row.Str[f.Slot] = s
		}
	}
	return row, nil
}

// ReadLog calls fn for every entry of a decision log, in order.
func ReadLog(r io.Reader, fn func(line int, e *LogEntry) error) error {
	return readJSONL(r, func(line int, b []byte) error {
		var e LogEntry
		if err := unmarshalLine(b, &e); err != nil {
			return errLogLine(line, err)
		}
		return fn(line, &e)
	})
}
