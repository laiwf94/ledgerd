package webhook

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	secret := NewSecret()
	if !strings.HasPrefix(secret, "whsec_") || len(secret) != len("whsec_")+43 || secret == NewSecret() {
		t.Fatalf("secret %q", secret)
	}
	now := time.Unix(1_900_000_000, 0)
	body := []byte(`{"id":"w.1.1","endpoint":"w","entries":[]}`)
	h := Sign(secret, now, body)
	if !strings.HasPrefix(h, "t=1900000000,v1=") || len(h) != len("t=1900000000,v1=")+64 {
		t.Fatalf("header %q", h)
	}
	if err := Verify(secret, h, body, now.Add(time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	// The same body at another time has another signature.
	if Sign(secret, now.Add(time.Second), body) == h {
		t.Fatal("the timestamp is not signed")
	}
	cases := []struct {
		name, secret, header string
		body                 []byte
		now                  time.Time
		want                 error
	}{
		{"tampered body", secret, h, []byte(`{"id":"w.1.2","endpoint":"w","entries":[]}`), now, ErrBadSignature},
		{"wrong secret", NewSecret(), h, body, now, ErrBadSignature},
		{"moved timestamp", secret, strings.Replace(h, "t=1900000000", "t=1900000001", 1), body, now, ErrBadSignature},
		{"too old", secret, h, body, now.Add(DefaultTolerance + time.Second), ErrTimestamp},
		{"from the future", secret, h, body, now.Add(-DefaultTolerance - time.Second), ErrTimestamp},
		{"empty", secret, "", body, now, ErrNoSignature},
		{"no timestamp", secret, h[strings.Index(h, "v1="):], body, now, ErrNoSignature},
		{"no signature", secret, "t=1900000000", body, now, ErrNoSignature},
		{"two timestamps", secret, "t=1," + h, body, now, ErrNoSignature},
		{"bad hex", secret, "t=1900000000,v1=zz", body, now, ErrNoSignature},
		{"short mac", secret, "t=1900000000,v1=abcd", body, now, ErrNoSignature},
		{"bad timestamp", secret, "t=soon," + h[strings.Index(h, "v1="):], body, now, ErrNoSignature},
		{"no equals", secret, h + ",junk", body, now, ErrNoSignature},
	}
	for _, c := range cases {
		if err := Verify(c.secret, c.header, c.body, c.now, 0); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// During a rotation the header carries one signature per secret, and
	// unknown schemes are ignored.
	old := NewSecret()
	rotated := Sign(old, now, body) + "," + strings.TrimPrefix(Sign(secret, now, body), "t=1900000000,") + ",v0=legacy"
	for _, s := range []string{old, secret} {
		if err := Verify(s, rotated, body, now, 0); err != nil {
			t.Fatalf("rotation: %v", err)
		}
	}
	if err := Verify(secret, h, body, now.Add(2*time.Hour), 3*time.Hour); err != nil {
		t.Fatalf("custom tolerance: %v", err)
	}
}
