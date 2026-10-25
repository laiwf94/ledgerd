package ledger

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The log is a command log. Each accepted command is stored with the time it
// ran and the IDs it produced; replaying a record runs the same command
// through the same code at the same time, so the rebuilt state is identical.
// Expiries are their own records, so recovery never consults the clock.
//
// Commands that were rejected are stored only when they carried an
// idempotency key, so a retry gets the original error back even after a
// restart.

const (
	recHeader = "header"
	recCmd    = "cmd"
	recExpire = "expire"
	recReject = "reject"

	logVersion = 1
)

type record struct {
	Type    string     `json:"t"`
	Version int        `json:"v,omitempty"`
	Time    time.Time  `json:"time,omitzero"`
	Seq     uint64     `json:"seq,omitempty"` // entry the record produced, if any
	ID      string     `json:"id,omitempty"`  // transfer created or expired
	Cmd     *Command   `json:"cmd,omitempty"`
	Idem    *idemField `json:"idem,omitempty"`
	Err     *Error     `json:"err,omitempty"`
}

type idemField struct {
	Scope string `json:"scope,omitempty"`
	Key   string `json:"key"`
	Hash  string `json:"hash"`
}

func toIdemField(i *Idem) *idemField {
	if i == nil {
		return nil
	}
	return &idemField{Scope: i.Scope, Key: i.Key, Hash: hex.EncodeToString(i.Hash[:])}
}

func (f *idemField) idem() (*Idem, error) {
	i := &Idem{Scope: f.Scope, Key: f.Key}
	h, err := hex.DecodeString(f.Hash)
	if err != nil || len(h) != len(i.Hash) {
		return nil, errors.New("bad idempotency hash")
	}
	copy(i.Hash[:], h)
	if err := i.validate(); err != nil {
		return nil, err
	}
	return i, nil
}

func encodeRecord(r record) ([]byte, error) { return json.Marshal(r) }

func decodeRecord(p []byte) (record, error) {
	var r record
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("bad record: %v", err)
	}
	if dec.More() {
		return r, errors.New("bad record: trailing data")
	}
	return r, nil
}
