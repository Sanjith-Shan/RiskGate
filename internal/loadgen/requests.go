package loadgen

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Request is one assess request body loaded from a JSONL file.
//
// The file has one JSON object per line:
//
//	{"payment_id": "...", "created": ..., "amount": ..., "currency": "...", "risk_fields": {...}}
//
// The line is sent as the body. RiskGate updates velocity state on every
// assess, so replaying the same payment_id many times is not the same
// workload as distinct payments; Body can make each send's id unique.
type Request struct {
	raw []byte
	// For unique ids: the body is prefix + idOpen + "_lg<seq>" + `"` + suffix,
	// where idOpen is the JSON-quoted original id without its closing quote.
	prefix, idOpen, suffix []byte
}

// placeholder stands in for the payment_id value while the line is
// re-encoded, so its position can be found without a streaming parser.
const placeholder = `"__loadgen_payment_id_placeholder__"`

// ParseRequest prepares one JSONL line.
func ParseRequest(line []byte) (Request, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return Request{}, err
	}
	var id string
	if raw, ok := fields["payment_id"]; ok {
		if err := json.Unmarshal(raw, &id); err != nil {
			return Request{}, fmt.Errorf("payment_id is not a string: %w", err)
		}
	}
	fields["payment_id"] = json.RawMessage(placeholder)
	// Re-encoding sorts keys; the JSON is equivalent, and only rewritten
	// bodies use this form.
	encoded, err := json.Marshal(fields)
	if err != nil {
		return Request{}, err
	}
	at := bytes.Index(encoded, []byte(placeholder))
	if at < 0 || bytes.Count(encoded, []byte(placeholder)) != 1 {
		return Request{}, errors.New("line contains the payment_id placeholder text")
	}
	quoted, err := json.Marshal(id)
	if err != nil {
		return Request{}, err
	}
	return Request{
		raw:    bytes.Clone(line),
		prefix: encoded[:at],
		idOpen: quoted[:len(quoted)-1],
		suffix: encoded[at+len(placeholder):],
	}, nil
}

// Body returns the request body for send number seq. With unique set, the
// payment_id becomes "<original>_lg<seq>"; otherwise the line is sent
// byte for byte.
func (r Request) Body(seq int64, unique bool) []byte {
	if !unique {
		return r.raw
	}
	b := make([]byte, 0, len(r.prefix)+len(r.idOpen)+len(r.suffix)+24)
	b = append(b, r.prefix...)
	b = append(b, r.idOpen...)
	b = append(b, "_lg"...)
	b = strconv.AppendInt(b, seq, 10)
	b = append(b, '"')
	return append(b, r.suffix...)
}

// ReadRequests reads a JSONL file of assess requests. Blank lines are
// skipped; any other invalid line is an error naming its line number.
func ReadRequests(r io.Reader) ([]Request, error) {
	var reqs []Request
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		req, err := ParseRequest(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		reqs = append(reqs, req)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(reqs) == 0 {
		return nil, errors.New("no requests in input")
	}
	return reqs, nil
}
