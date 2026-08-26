package webhook

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	now := time.Unix(1767225600, 0)
	body := []byte(`{"id":"evt_1"}`)
	header := Sign(body, now, []byte("old"), []byte("new"))
	if got := strings.Count(header, "v1="); got != 2 {
		t.Fatalf("Sign with two secrets produced %d v1 entries: %s", got, header)
	}
	for _, secret := range []string{"old", "new"} {
		v := NewVerifier(secret)
		if err := v.VerifyAt(header, body, now); err != nil {
			t.Errorf("secret %q: %v", secret, err)
		}
	}
}

func TestVerifyUsesInjectedClock(t *testing.T) {
	signedAt := time.Unix(1_000_000, 0)
	body := []byte("x")
	header := Sign(body, signedAt, []byte("s"))

	v := NewVerifier("s")
	v.Now = func() time.Time { return signedAt.Add(299 * time.Second) }
	if err := v.Verify(header, body); err != nil {
		t.Fatalf("inside tolerance: %v", err)
	}
	v.Now = func() time.Time { return signedAt.Add(301 * time.Second) }
	if err := v.Verify(header, body); !errors.Is(err, ErrTimestampOutsideTolerance) {
		t.Fatalf("outside tolerance: got %v", err)
	}
}

func TestVerifyWallClock(t *testing.T) {
	body := []byte("x")
	if err := NewVerifier("s").Verify(Sign(body, time.Now(), []byte("s")), body); err != nil {
		t.Fatal(err)
	}
}

func TestVerifierWithoutSecretsFailsClosed(t *testing.T) {
	body := []byte("x")
	now := time.Unix(1_000_000, 0)
	err := (&Verifier{}).VerifyAt(Sign(body, now, []byte("s")), body, now)
	if !errors.Is(err, ErrNoMatchingSignature) {
		t.Fatalf("got %v, want no_matching_signature", err)
	}
}

func TestErrorsIsMatchesByCode(t *testing.T) {
	err := error(&VerificationError{Code: CodeMalformedHeader, Reason: "missing t"})
	if !errors.Is(err, ErrMalformedHeader) {
		t.Error("errors.Is should match by code")
	}
	if errors.Is(err, ErrNoMatchingSignature) {
		t.Error("errors.Is matched a different code")
	}
	if got := err.Error(); got != "webhook: malformed_header: missing t" {
		t.Errorf("Error() = %q", got)
	}
	if got := ErrMalformedHeader.Error(); got != "webhook: malformed_header" {
		t.Errorf("Error() = %q", got)
	}
	if Code(errors.New("other")) != "" {
		t.Error("Code of a foreign error should be empty")
	}
}

func TestParseHeader(t *testing.T) {
	sig := strings.Repeat("ab", 32)
	h, err := ParseHeader("t=42, v0=zz ,v1=" + sig + ",v1=" + strings.ToUpper(sig))
	if err != nil {
		t.Fatal(err)
	}
	if h.Timestamp != 42 || len(h.Signatures) != 2 || len(h.Signatures[0]) != 32 {
		t.Fatalf("parsed %+v", h)
	}
	if h.Signatures[0][0] != 0xab || h.Signatures[1][0] != 0xab {
		t.Fatal("hex decoded wrong")
	}
	if _, err := ParseHeader("t=9223372036854775807,v1=" + sig); err != nil {
		t.Fatalf("max int64 rejected: %v", err)
	}
	if _, err := ParseHeader("t=9223372036854775808,v1=" + sig); !errors.Is(err, ErrMalformedHeader) {
		t.Fatalf("max int64 + 1: %v", err)
	}
	if _, err := ParseHeader("t=0,v1=" + sig); err != nil {
		t.Fatalf("t=0 is canonical: %v", err)
	}
}

func TestTimestampFarFromNowDoesNotOverflow(t *testing.T) {
	body := []byte("x")
	ts := time.Unix(1<<62, 0)
	header := Sign(body, ts, []byte("s"))
	err := NewVerifier("s").VerifyAt(header, body, time.Unix(0, 0))
	if !errors.Is(err, ErrTimestampOutsideTolerance) {
		t.Fatalf("got %v", err)
	}
}

func BenchmarkVerify(b *testing.B) {
	body := []byte(strings.Repeat("x", 2048))
	now := time.Unix(1767225600, 0)
	header := Sign(body, now, []byte("old"), []byte("new"))
	v := NewVerifier("new")
	b.ReportAllocs()
	for b.Loop() {
		if err := v.VerifyAt(header, body, now); err != nil {
			b.Fatal(err)
		}
	}
}
