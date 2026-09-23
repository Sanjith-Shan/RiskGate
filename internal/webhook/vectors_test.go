package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updateVectors = flag.Bool("update", false, "regenerate testdata/signatures.json")

// vector is one entry of the signature test-vector file shared with
// Clearinghouse's Ruby verifier. Both implementations must agree on every
// entry, so the format is language-neutral: strings, integers, booleans.
type vector struct {
	Name      string   `json:"name"`
	Secrets   []string `json:"secrets"`
	Payload   string   `json:"payload"`
	Header    string   `json:"header"`
	Now       int64    `json:"now"`
	Tolerance int64    `json:"tolerance"`
	Valid     bool     `json:"valid"`
	Error     *string  `json:"error"`
}

const vectorFile = "testdata/signatures.json"

// Vector files written by Clearinghouse. The testdata copy is refreshed by
// scripts/sync_signature_vectors.sh and is what CI sees; the sibling
// checkout is checked too when it exists on this machine.
func clearinghouseVectorFiles() []string {
	files := []string{"testdata/clearinghouse_signatures.json"}
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, "Documents", "Clearinghouse", "spec", "fixtures", "signatures.json"))
	}
	return files
}

func TestSignatureVectors(t *testing.T) {
	runVectorFile(t, vectorFile)
}

func TestClearinghouseSignatureVectors(t *testing.T) {
	found := false
	for _, path := range clearinghouseVectorFiles() {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		found = true
		t.Run(path, func(t *testing.T) { runVectorFile(t, path) })
	}
	if !found {
		t.Skip("no Clearinghouse vector file; run scripts/sync_signature_vectors.sh")
	}
}

func runVectorFile(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if len(vs) == 0 {
		t.Fatalf("%s: no vectors", path)
	}
	for _, v := range vs {
		t.Run(v.Name, func(t *testing.T) {
			if v.Valid != (v.Error == nil) {
				t.Fatalf("inconsistent vector: valid=%v error=%v", v.Valid, v.Error)
			}
			if v.Tolerance <= 0 {
				// Clearinghouse reads 0 as "exactly now"; Verifier reads a
				// zero Tolerance as the default. No shared vector uses 0.
				t.Fatalf("tolerance %d is not supported by this runner", v.Tolerance)
			}
			verifier := NewVerifier(v.Secrets...)
			verifier.Tolerance = time.Duration(v.Tolerance) * time.Second
			err := verifier.VerifyAt(v.Header, []byte(v.Payload), time.Unix(v.Now, 0))
			switch {
			case v.Valid && err != nil:
				t.Fatalf("want valid, got %v", err)
			case !v.Valid && err == nil:
				t.Fatalf("want %s, got valid", *v.Error)
			case !v.Valid && string(Code(err)) != *v.Error:
				t.Fatalf("want %s, got %v", *v.Error, err)
			}
		})
	}
}

// TestVectorFileUpToDate regenerates the vectors and compares them with the
// committed file, so the file can never drift from the code that made it.
// Run `go test ./internal/webhook -run TestVectorFileUpToDate -update` to
// rewrite it, then scripts/check_signature_vectors.sh to cross-check it.
func TestVectorFileUpToDate(t *testing.T) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(generateVectors()); err != nil {
		t.Fatal(err)
	}
	if *updateVectors {
		if err := os.WriteFile(vectorFile, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, buf.Bytes()) {
		t.Fatalf("%s is stale; rerun with -update", vectorFile)
	}
}

func generateVectors() []vector {
	const (
		t0      int64 = 1767225600 // 2026-01-01T00:00:00Z
		primary       = "whsec_riskgate_primary_2f9c1d"
		rotated       = "whsec_riskgate_rotated_8a4b7e"
		other         = "whsec_not_the_right_secret"
	)
	payload := `{"id":"evt_1QbX7d2eZvKYlo2C","object":"event","type":"charge.dispute.created","created":1767225600,"api_version":"2026-01-01","data":{"object":{"id":"dp_1QbX7d2eZvKYlo2C","object":"dispute","payment_intent":"pi_3QbX7c2eZvKYlo2C","amount":4999,"currency":"usd","reason":"fraudulent","status":"needs_response"}}}`
	unicodePayload := `{"id":"evt_unicode","object":"event","type":"payment_intent.succeeded","created":1767225600,"api_version":"2026-01-01","data":{"object":{"id":"pi_unicode","amount":1250,"amount_received":1250,"currency":"eur","status":"succeeded","description":"Café Zürich ☕ — 東京 🍣","note":"line\nbreak\ttab \"quoted\" \\ backslash"}}}`

	sig := func(secret string, ts int64, body string) string {
		h := Sign([]byte(body), time.Unix(ts, 0), []byte(secret))
		_, v1, _ := strings.Cut(h, ",v1=")
		return v1
	}
	good := sig(primary, t0, payload)
	ts := "t=1767225600"
	code := func(c ErrorCode) *string { s := string(c); return &s }
	ok := (*string)(nil)

	type spec struct {
		name    string
		secrets []string
		payload string
		header  string
		now     int64
		tol     int64
		err     *string
	}
	specs := []spec{
		// Accepted.
		{"valid single v1", []string{primary}, payload, ts + ",v1=" + good, t0, 300, ok},
		{"valid with clock slightly ahead", []string{primary}, payload, ts + ",v1=" + good, t0 + 42, 300, ok},
		{"valid with clock slightly behind", []string{primary}, payload, ts + ",v1=" + good, t0 - 42, 300, ok},
		{"rotation: two v1, verifier has new secret", []string{rotated}, payload, ts + ",v1=" + good + ",v1=" + sig(rotated, t0, payload), t0, 300, ok},
		{"rotation: two v1, verifier has old secret", []string{primary}, payload, ts + ",v1=" + good + ",v1=" + sig(rotated, t0, payload), t0, 300, ok},
		{"rotation: three v1, only last matches", []string{rotated}, payload, ts + ",v1=" + sig(other, t0, payload) + ",v1=" + good + ",v1=" + sig(rotated, t0, payload), t0, 300, ok},
		{"two secrets configured, header signed with first", []string{primary, rotated}, payload, ts + ",v1=" + good, t0, 300, ok},
		{"two secrets configured, header signed with second", []string{primary, rotated}, payload, ts + ",v1=" + sig(rotated, t0, payload), t0, 300, ok},
		{"v1 before t", []string{primary}, payload, "v1=" + good + "," + ts, t0, 300, ok},
		{"v0 scheme ignored", []string{primary}, payload, ts + ",v0=" + strings.Repeat("0", 64) + ",v1=" + good, t0, 300, ok},
		{"unknown future scheme ignored even if not hex", []string{primary}, payload, ts + ",v1=" + good + ",v2=not-hex-at-all", t0, 300, ok},
		{"a junk v1 next to a good one is just a non-match", []string{primary}, payload, ts + ",v1=" + good + ",v1=xyz", t0, 300, ok},
		{"leading-zero t signed as sent", []string{primary}, payload, "t=01767225600,v1=" + hmacHex(primary, "01767225600."+payload), t0, 300, ok},
		{"spaces around items", []string{primary}, payload, " t=1767225600 , v1=" + good + " ", t0, 300, ok},
		{"tab around items", []string{primary}, payload, "t=1767225600,\tv1=" + good, t0, 300, ok},
		{"duplicate identical v1", []string{primary}, payload, ts + ",v1=" + good + ",v1=" + good, t0, 300, ok},
		{"unicode body", []string{primary}, unicodePayload, ts + ",v1=" + sig(primary, t0, unicodePayload), t0, 300, ok},
		{"unicode secret", []string{"whsec_clé_🔑"}, payload, ts + ",v1=" + sig("whsec_clé_🔑", t0, payload), t0, 300, ok},
		{"empty body", []string{primary}, "", ts + ",v1=" + sig(primary, t0, ""), t0, 300, ok},
		{"boundary: exactly tolerance in the past", []string{primary}, payload, ts + ",v1=" + good, t0 + 300, 300, ok},
		{"boundary: exactly tolerance in the future", []string{primary}, payload, ts + ",v1=" + good, t0 - 300, 300, ok},
		{"custom tolerance: exactly 10s", []string{primary}, payload, ts + ",v1=" + good, t0 + 10, 10, ok},

		// Signature does not match.
		{"wrong secret", []string{other}, payload, ts + ",v1=" + good, t0, 300, code(CodeNoMatchingSignature)},
		{"tampered body: amount changed", []string{primary}, strings.Replace(payload, `"amount":4999`, `"amount":4990`, 1), ts + ",v1=" + good, t0, 300, code(CodeNoMatchingSignature)},
		{"tampered body: trailing newline added", []string{primary}, payload + "\n", ts + ",v1=" + good, t0, 300, code(CodeNoMatchingSignature)},
		{"tampered timestamp: t moved, signature for original t", []string{primary}, payload, "t=1767225601,v1=" + good, t0, 300, code(CodeNoMatchingSignature)},
		{"unicode body normalized differently (NFD)", []string{primary}, strings.Replace(unicodePayload, "Café", "Café", 1), ts + ",v1=" + sig(primary, t0, unicodePayload), t0, 300, code(CodeNoMatchingSignature)},
		{"empty body against signature of {}", []string{primary}, "", ts + ",v1=" + sig(primary, t0, "{}"), t0, 300, code(CodeNoMatchingSignature)},
		{"signature over body only, no timestamp prefix", []string{primary}, payload, ts + ",v1=" + hmacHex(primary, payload), t0, 300, code(CodeNoMatchingSignature)},
		{"rotation: two v1, neither secret configured", []string{other}, payload, ts + ",v1=" + good + ",v1=" + sig(rotated, t0, payload), t0, 300, code(CodeNoMatchingSignature)},
		{"secret is case sensitive", []string{strings.ToUpper(primary)}, payload, ts + ",v1=" + good, t0, 300, code(CodeNoMatchingSignature)},
		{"valid only under v0, which is ignored", []string{primary}, payload, ts + ",v0=" + good + ",v1=" + sig(other, t0, payload), t0, 300, code(CodeNoMatchingSignature)},
		{"old timestamp and wrong secret: signature checked first", []string{other}, payload, ts + ",v1=" + good, t0 + 3600, 300, code(CodeNoMatchingSignature)},

		// Timestamp outside tolerance.
		{"old timestamp", []string{primary}, payload, ts + ",v1=" + good, t0 + 3600, 300, code(CodeTimestampOutsideTolerance)},
		{"future timestamp", []string{primary}, payload, ts + ",v1=" + good, t0 - 3600, 300, code(CodeTimestampOutsideTolerance)},
		{"boundary: one second past tolerance in the past", []string{primary}, payload, ts + ",v1=" + good, t0 + 301, 300, code(CodeTimestampOutsideTolerance)},
		{"boundary: one second past tolerance in the future", []string{primary}, payload, ts + ",v1=" + good, t0 - 301, 300, code(CodeTimestampOutsideTolerance)},
		{"custom tolerance: 11s past 10s", []string{primary}, payload, ts + ",v1=" + good, t0 + 11, 10, code(CodeTimestampOutsideTolerance)},
		{"replay of a day-old delivery", []string{primary}, payload, ts + ",v1=" + good, t0 + 86400, 300, code(CodeTimestampOutsideTolerance)},

		// Malformed header.
		{"empty header", []string{primary}, payload, "", t0, 300, code(CodeMalformedHeader)},
		{"whitespace-only header", []string{primary}, payload, "  \t ", t0, 300, code(CodeMalformedHeader)},
		{"missing t", []string{primary}, payload, "v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"missing v1", []string{primary}, payload, ts, t0, 300, code(CodeMalformedHeader)},
		{"only v0", []string{primary}, payload, ts + ",v0=" + good, t0, 300, code(CodeMalformedHeader)},
		{"v1 key is case sensitive", []string{primary}, payload, ts + ",V1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"t key is case sensitive", []string{primary}, payload, "T=1767225600,v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"v1 not hex", []string{primary}, payload, ts + ",v1=" + strings.Repeat("zz", 32), t0, 300, code(CodeNoMatchingSignature)},
		{"v1 truncated", []string{primary}, payload, ts + ",v1=" + good[:63], t0, 300, code(CodeNoMatchingSignature)},
		{"v1 too long", []string{primary}, payload, ts + ",v1=" + good + "00", t0, 300, code(CodeNoMatchingSignature)},
		{"v1 containing equals sign", []string{primary}, payload, ts + ",v1=" + good + "=", t0, 300, code(CodeNoMatchingSignature)},
		{"uppercase hex does not match", []string{primary}, payload, ts + ",v1=" + strings.ToUpper(good), t0, 300, code(CodeNoMatchingSignature)},
		{"leading-zero t with signature over canonical t", []string{primary}, payload, "t=01767225600,v1=" + good, t0, 300, code(CodeNoMatchingSignature)},
		{"v1 empty", []string{primary}, payload, ts + ",v1=", t0, 300, code(CodeMalformedHeader)},
		{"empty v1 next to a valid one", []string{primary}, payload, ts + ",v1=" + good + ",v1=", t0, 300, code(CodeMalformedHeader)},
		{"duplicate t, different values", []string{primary}, payload, ts + ",t=1767225601,v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"duplicate t, same value", []string{primary}, payload, ts + "," + ts + ",v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"t empty", []string{primary}, payload, "t=,v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"t not a number", []string{primary}, payload, "t=yesterday,v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"t negative", []string{primary}, payload, "t=-1767225600,v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"t with plus sign", []string{primary}, payload, "t=+1767225600,v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"t fractional", []string{primary}, payload, "t=1767225600.5,v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"t in milliseconds does not match a seconds signature", []string{primary}, payload, "t=1767225600000,v1=" + good, t0, 300, code(CodeNoMatchingSignature)},
		{"t overflows int64", []string{primary}, payload, "t=99999999999999999999,v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"t with 16 digits", []string{primary}, payload, "t=1000000000000000,v1=" + hmacHex(primary, "1000000000000000."+payload), t0, 300, code(CodeMalformedHeader)},
		{"t with 15 digits parses, then fails tolerance", []string{primary}, payload, "t=999999999999999,v1=" + hmacHex(primary, "999999999999999."+payload), t0, 300, code(CodeTimestampOutsideTolerance)},
		{"empty element", []string{primary}, payload, ts + ",,v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"t containing equals sign", []string{primary}, payload, "t=1767225600=1,v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"space around equals", []string{primary}, payload, "t = 1767225600,v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"item without equals", []string{primary}, payload, ts + ",v1=" + good + ",garbage", t0, 300, code(CodeMalformedHeader)},
		{"empty key", []string{primary}, payload, ts + ",=abc,v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"trailing comma", []string{primary}, payload, ts + ",v1=" + good + ",", t0, 300, code(CodeMalformedHeader)},
		{"semicolon separator", []string{primary}, payload, ts + ";v1=" + good, t0, 300, code(CodeMalformedHeader)},
		{"malformed wins over wrong secret", []string{other}, payload, "v1=" + good, t0, 300, code(CodeMalformedHeader)},
	}

	out := make([]vector, len(specs))
	for i, s := range specs {
		out[i] = vector{
			Name: s.name, Secrets: s.secrets, Payload: s.payload, Header: s.header,
			Now: s.now, Tolerance: s.tol, Valid: s.err == nil, Error: s.err,
		}
	}
	return out
}

// hmacHex is lowercase hex HMAC-SHA256 of msg, computed without Sign so
// vectors can sign arbitrary text (a non-canonical t, or the body alone).
func hmacHex(secret, msg string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}
