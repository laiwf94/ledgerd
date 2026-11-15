// Package webhook delivers journal entries to registered HTTP endpoints,
// signed with HMAC-SHA256, retried with exponential backoff, with each
// endpoint's position kept in the ledger's log.
package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Headers on every delivery.
const (
	// SignatureHeader carries "t=<unix seconds>,v1=<hex HMAC-SHA256>". The
	// MAC is over the timestamp, a full stop and the raw body, keyed with
	// the endpoint's secret, so a receiver can reject both a forged body
	// and an old delivery played back later.
	SignatureHeader = "Ledgerd-Signature"
	// IDHeader names the delivery: the endpoint, then the first and last
	// sequence it carries. A retry of the same entries has the same ID.
	IDHeader = "Ledgerd-Webhook-Id"
	// DefaultTolerance is how far a signature's timestamp may be from the
	// receiver's clock in Verify.
	DefaultTolerance = 5 * time.Minute
	secretPrefix     = "whsec_"
)

// NewSecret returns a random endpoint secret: "whsec_" and 32 random bytes
// in unpadded base64url.
func NewSecret() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return secretPrefix + base64.RawURLEncoding.EncodeToString(b[:])
}

// Sign returns the signature header value for body sent at t.
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac(secret, ts, body))
}

func mac(secret, ts string, body []byte) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	m.Write([]byte{'.'})
	m.Write(body)
	return m.Sum(nil)
}

// Errors from Verify.
var (
	ErrNoSignature  = errors.New("webhook: the signature header is missing or malformed")
	ErrBadSignature = errors.New("webhook: no signature matches the body")
	ErrTimestamp    = errors.New("webhook: the signature's timestamp is outside the tolerance")
)

// Verify checks a delivery's signature header against its raw body and
// the endpoint's secret: some v1 signature must be the HMAC of the
// timestamp and body, compared in constant time, and the timestamp must be
// within tolerance of now (DefaultTolerance when zero). A header may carry
// several v1 values, so a receiver keeps working while a secret is rotated.
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	var ts string
	var sigs [][]byte
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return ErrNoSignature
		}
		switch k {
		case "t":
			if ts != "" {
				return ErrNoSignature
			}
			ts = v
		case "v1":
			b, err := hex.DecodeString(v)
			if err != nil || len(b) != sha256.Size {
				return ErrNoSignature
			}
			sigs = append(sigs, b)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 || len(ts) > 19 {
		return ErrNoSignature
	}
	want := mac(secret, ts, body)
	ok := false
	for _, s := range sigs {
		if hmac.Equal(s, want) {
			ok = true
		}
	}
	if !ok {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return ErrTimestamp
	}
	return nil
}
