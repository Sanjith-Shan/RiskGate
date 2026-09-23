package webhook

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"
)

// FuzzParseHeader feeds arbitrary header values to the parser. It must never
// panic, every error must be malformed_header, and every success must have
// a timestamp and only 32-byte signatures.
func FuzzParseHeader(f *testing.F) {
	sig := strings.Repeat("0f", 32)
	for _, seed := range []string{
		"", " ", ",", "=", "t=", "v1=", "t=1,v1=" + sig, "t=1,v1=" + sig + ",v1=" + sig,
		"t=01,v1=" + sig, "t=-1,v1=" + sig, "t=1,t=1,v1=" + sig, "t=1,v0=x,v1=" + sig,
		"t=99999999999999999999,v1=" + sig, " t=1 ,\tv1=" + sig, "t=1,v1=" + sig + ",",
		"t=1,v1=" + sig[:63], "t=1,v1=zz", "a=b=c", "t=1;v1=" + sig, "\x00t=1,v1=" + sig,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		h, err := ParseHeader(value)
		if err != nil {
			if !errors.Is(err, ErrMalformedHeader) {
				t.Fatalf("ParseHeader(%q) returned %v, want malformed_header", value, err)
			}
			return
		}
		if h.Timestamp < 0 {
			t.Fatalf("negative timestamp from %q", value)
		}
		if len(h.Signatures) == 0 {
			t.Fatalf("accepted %q with no signatures", value)
		}
		for _, s := range h.Signatures {
			if len(s) != sha256.Size {
				t.Fatalf("signature of %d bytes from %q", len(s), value)
			}
		}
	})
}

// FuzzVerify checks the sign/verify contract on arbitrary bodies and
// secrets: a body verifies under the secret that signed it, and not after a
// one-byte change.
func FuzzVerify(f *testing.F) {
	f.Add([]byte(`{"id":"evt_1"}`), "whsec_a", int64(1767225600))
	f.Add([]byte{}, "k", int64(0))
	f.Add([]byte("caf\xc3\xa9"), "\xff", int64(1<<40))
	f.Fuzz(func(t *testing.T, body []byte, secret string, unix int64) {
		if unix < 0 || unix > 1<<40 {
			return // Sign only produces canonical non-negative timestamps.
		}
		now := time.Unix(unix, 0)
		v := NewVerifier(secret)
		header := Sign(body, now, []byte(secret))
		if err := v.VerifyAt(header, body, now); err != nil {
			t.Fatalf("signed body rejected: %v", err)
		}
		tampered := append([]byte(nil), body...)
		if len(tampered) == 0 {
			tampered = append(tampered, 'x')
		} else {
			tampered[0] ^= 0x01
		}
		if err := v.VerifyAt(header, tampered, now); !errors.Is(err, ErrNoMatchingSignature) {
			t.Fatalf("tampered body: got %v, want no_matching_signature", err)
		}
	})
}
