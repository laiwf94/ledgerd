package api

// Listing cursors are opaque to clients. A cursor names a position in one
// listing and is bound to the filters it was issued for, so it cannot be
// replayed against a different query, and it is signed with HMAC-SHA256, so
// a client cannot forge or edit one. The format can change between versions
// without breaking clients that treat it as opaque.
//
// Layout before base64url encoding (no padding):
//
//	version   1 byte  (cursorVersion)
//	kind      1 byte  (which listing)
//	filter    8 bytes (SHA-256 prefix of the canonical filter string)
//	position  the rest, up to maxCursorPosition bytes
//	mac       16 bytes (HMAC-SHA256 prefix over everything before it)

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
)

const (
	cursorVersion     = 1
	cursorFilterBytes = 8
	cursorMACBytes    = 16
	maxCursorPosition = 64
	// maxCursorLength bounds the encoded cursor accepted in a query string.
	maxCursorLength = (2+cursorFilterBytes+maxCursorPosition+cursorMACBytes)*4/3 + 4
)

// cursorKind names the listing a cursor belongs to.
type cursorKind byte

const (
	cursorAccounts cursorKind = 1
)

var errBadCursor = errors.New("api: bad cursor")

// cursorCodec signs and checks cursors with one secret.
type cursorCodec struct{ key []byte }

// newCursorCodec derives the signing key. With an explicit secret it is
// used; otherwise the key is derived from every API key, so cursors survive
// restarts without more configuration and stop working when the key set
// changes, which only makes clients restart their listing.
func newCursorCodec(secret string, apiKeys []string) cursorCodec {
	h := sha256.New()
	if secret != "" {
		h.Write([]byte("ledgerd cursor secret v1\x00"))
		h.Write([]byte(secret))
	} else {
		keys := append([]string(nil), apiKeys...)
		sort.Strings(keys)
		h.Write([]byte("ledgerd cursor from api keys v1\x00"))
		for _, k := range keys {
			h.Write([]byte(k))
			h.Write([]byte{0})
		}
	}
	return cursorCodec{key: h.Sum(nil)}
}

func (c cursorCodec) mac(b []byte) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write(b)
	return m.Sum(nil)[:cursorMACBytes]
}

func filterDigest(filter string) []byte {
	sum := sha256.Sum256([]byte(filter))
	return sum[:cursorFilterBytes]
}

// encode returns the cursor for position in the listing kind with filter.
func (c cursorCodec) encode(kind cursorKind, filter string, position []byte) string {
	if len(position) > maxCursorPosition {
		panic("api: cursor position too long")
	}
	b := make([]byte, 0, 2+cursorFilterBytes+len(position)+cursorMACBytes)
	b = append(b, cursorVersion, byte(kind))
	b = append(b, filterDigest(filter)...)
	b = append(b, position...)
	b = append(b, c.mac(b)...)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decode checks a cursor and returns its position. It fails for anything not
// issued by encode with the same key, kind and filter.
func (c cursorCodec) decode(kind cursorKind, filter, cursor string) ([]byte, error) {
	if len(cursor) > maxCursorLength {
		return nil, errBadCursor
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || len(b) < 2+cursorFilterBytes+cursorMACBytes {
		return nil, errBadCursor
	}
	body, sig := b[:len(b)-cursorMACBytes], b[len(b)-cursorMACBytes:]
	if !hmac.Equal(sig, c.mac(body)) {
		return nil, errBadCursor
	}
	if body[0] != cursorVersion || cursorKind(body[1]) != kind ||
		!hmac.Equal(body[2:2+cursorFilterBytes], filterDigest(filter)) {
		return nil, errBadCursor
	}
	pos := body[2+cursorFilterBytes:]
	if len(pos) > maxCursorPosition {
		return nil, errBadCursor
	}
	return pos, nil
}

// canonicalFilter writes name=value pairs in sorted order with each part
// length-prefixed, so different filters never share a string.
func canonicalFilter(pairs map[string]string) string {
	names := make([]string, 0, len(pairs))
	for n := range pairs {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		writeLenPrefixed(&b, n)
		writeLenPrefixed(&b, pairs[n])
	}
	return b.String()
}

func writeLenPrefixed(b *strings.Builder, s string) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	b.Write(n[:])
	b.WriteString(s)
}
