// Package webhook receives Clearinghouse webhook events: it verifies the
// Clearinghouse-Signature header, deduplicates deliveries by event id, and
// decodes the event envelope.
//
// # Signature header
//
// Clearinghouse signs every delivery with
//
//	Clearinghouse-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<raw body>")>
//
// The grammar below is shared with Clearinghouse's Ruby verifier, and the
// test vectors in testdata/signatures.json pin it down for both sides:
//
//   - The header is a comma-separated list of key=value items. Spaces and tabs
//     around an item are ignored. An item without '=' or with an empty key is
//     malformed.
//   - t must appear exactly once and be a canonical non-negative decimal
//     integer (no sign, no leading zeros) that fits in an int64.
//   - v1 must appear at least once. Each value is 64 hex digits (either case).
//     During secret rotation the sender includes one v1 per active secret and
//     the verifier accepts the payload if any of them matches.
//   - Any other key (v0, a future v2, ...) is ignored.
//
// Verification checks, in order: the header parses (else malformed_header),
// some configured secret produces some v1 in the header (else
// no_matching_signature), and t is within the tolerance of the verifier's
// clock in either direction (else timestamp_outside_tolerance). The
// signature is checked before the timestamp, as Stripe does, so that a
// request without a valid signature never learns anything about the clock.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader is the HTTP header that carries the signature.
const SignatureHeader = "Clearinghouse-Signature"

// DefaultTolerance is the replay window used when Verifier.Tolerance is zero.
const DefaultTolerance = 300 * time.Second

// ErrorCode identifies why a signature was rejected. The string values are
// part of the contract with Clearinghouse and appear in the shared test vectors.
type ErrorCode string

const (
	CodeNoMatchingSignature       ErrorCode = "no_matching_signature"
	CodeTimestampOutsideTolerance ErrorCode = "timestamp_outside_tolerance"
	CodeMalformedHeader           ErrorCode = "malformed_header"
)

// VerificationError is returned for every rejected signature. Match it with
// errors.Is against the sentinel values below, or errors.As to read the
// Code and the human-readable Reason.
type VerificationError struct {
	Code   ErrorCode
	Reason string
}

func (e *VerificationError) Error() string {
	if e.Reason == "" {
		return "webhook: " + string(e.Code)
	}
	return "webhook: " + string(e.Code) + ": " + e.Reason
}

// Is reports whether target is a *VerificationError with the same Code, so
// errors.Is(err, ErrMalformedHeader) ignores the Reason.
func (e *VerificationError) Is(target error) bool {
	t, ok := target.(*VerificationError)
	return ok && t.Code == e.Code
}

// Sentinel errors for use with errors.Is.
var (
	ErrNoMatchingSignature       = &VerificationError{Code: CodeNoMatchingSignature}
	ErrTimestampOutsideTolerance = &VerificationError{Code: CodeTimestampOutsideTolerance}
	ErrMalformedHeader           = &VerificationError{Code: CodeMalformedHeader}
)

// Header is a parsed Clearinghouse-Signature header.
type Header struct {
	// Timestamp is the t value, in Unix seconds.
	Timestamp int64
	// Signatures holds the decoded v1 values (32 bytes each), in header order.
	Signatures [][]byte
}

func malformed(format string, args ...any) error {
	return &VerificationError{Code: CodeMalformedHeader, Reason: fmt.Sprintf(format, args...)}
}

// ParseHeader parses a Clearinghouse-Signature header value. Every error it
// returns matches ErrMalformedHeader.
func ParseHeader(value string) (Header, error) {
	var h Header
	sawT := false
	if strings.Trim(value, " \t") == "" {
		return h, malformed("empty header")
	}
	for item := range strings.SplitSeq(value, ",") {
		item = strings.Trim(item, " \t")
		key, val, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			return h, malformed("item %q is not key=value", item)
		}
		switch key {
		case "t":
			if sawT {
				return h, malformed("duplicate t")
			}
			ts, err := parseTimestamp(val)
			if err != nil {
				return h, err
			}
			h.Timestamp, sawT = ts, true
		case "v1":
			if len(val) != 2*sha256.Size {
				return h, malformed("v1 must be %d hex digits, got %d characters", 2*sha256.Size, len(val))
			}
			sig, err := hex.DecodeString(val)
			if err != nil {
				return h, malformed("v1 is not hex")
			}
			h.Signatures = append(h.Signatures, sig)
		default:
			// Unknown schemes are ignored so the sender can add new ones
			// without breaking old verifiers.
		}
	}
	if !sawT {
		return h, malformed("missing t")
	}
	if len(h.Signatures) == 0 {
		return h, malformed("missing v1")
	}
	return h, nil
}

// parseTimestamp accepts only the canonical decimal form, so the string the
// sender signed and the integer the verifier checks cannot disagree.
func parseTimestamp(s string) (int64, error) {
	if s == "" {
		return 0, malformed("empty t")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, malformed("t is not a non-negative integer")
		}
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, malformed("t has a leading zero")
	}
	ts, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, malformed("t out of range")
	}
	return ts, nil
}

// Verifier checks Clearinghouse-Signature headers. The zero value is not
// usable: at least one secret is required. A Verifier is safe for
// concurrent use as long as its fields are not modified.
type Verifier struct {
	// Secrets are the signing secrets currently accepted. Configure two
	// during a rotation. Each secret's bytes are the HMAC key as-is.
	Secrets [][]byte
	// Tolerance bounds |now - t|. Zero means DefaultTolerance.
	Tolerance time.Duration
	// Now returns the current time. Nil means time.Now. Replays driven by a
	// simulated clock set this.
	Now func() time.Time
}

// NewVerifier returns a Verifier with the default tolerance and wall clock.
func NewVerifier(secrets ...string) *Verifier {
	v := &Verifier{}
	for _, s := range secrets {
		v.Secrets = append(v.Secrets, []byte(s))
	}
	return v
}

// Verify checks header against the raw request body. It returns nil or a
// *VerificationError.
func (v *Verifier) Verify(header string, payload []byte) error {
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	return v.VerifyAt(header, payload, now())
}

// VerifyAt is Verify with an explicit clock reading.
func (v *Verifier) VerifyAt(header string, payload []byte, now time.Time) error {
	if len(v.Secrets) == 0 {
		// A misconfigured verifier must fail closed, never accept.
		return &VerificationError{Code: CodeNoMatchingSignature, Reason: "no secrets configured"}
	}
	h, err := ParseHeader(header)
	if err != nil {
		return err
	}
	if !v.matches(h, payload) {
		return &VerificationError{Code: CodeNoMatchingSignature}
	}
	tolerance := v.Tolerance
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	// Compare in whole seconds: t has one-second resolution, and the shared
	// vectors define "now" in Unix seconds.
	skew := now.Unix() - h.Timestamp
	if skew < 0 {
		skew = -skew
	}
	if skew > int64(tolerance/time.Second) {
		return &VerificationError{
			Code:   CodeTimestampOutsideTolerance,
			Reason: fmt.Sprintf("t=%d is %ds from now, tolerance %s", h.Timestamp, now.Unix()-h.Timestamp, tolerance),
		}
	}
	return nil
}

// matches computes one MAC per secret and compares it, in constant time,
// against every v1 in the header. Work is secrets*signatures comparisons of
// 32 bytes, so a header stuffed with v1 values costs no extra HMACs.
func (v *Verifier) matches(h Header, payload []byte) bool {
	for _, secret := range v.Secrets {
		want := computeMAC(secret, h.Timestamp, payload)
		for _, got := range h.Signatures {
			if hmac.Equal(want, got) {
				return true
			}
		}
	}
	return false
}

func computeMAC(secret []byte, timestamp int64, payload []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(strconv.AppendInt(nil, timestamp, 10))
	mac.Write([]byte{'.'})
	mac.Write(payload)
	return mac.Sum(nil)
}

// Sign returns a Clearinghouse-Signature header value for payload at time t,
// with one v1 entry per secret (two during a rotation). It is what
// Clearinghouse sends, and RiskGate uses it in tests and tooling.
func Sign(payload []byte, t time.Time, secrets ...[]byte) string {
	ts := t.Unix()
	var b strings.Builder
	b.WriteString("t=")
	b.WriteString(strconv.FormatInt(ts, 10))
	for _, s := range secrets {
		b.WriteString(",v1=")
		b.WriteString(hex.EncodeToString(computeMAC(s, ts, payload)))
	}
	return b.String()
}

// Code returns the ErrorCode carried by err, or "" if err is not a
// *VerificationError.
func Code(err error) ErrorCode {
	var ve *VerificationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}
