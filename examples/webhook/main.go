// Command webhook is an example ledgerd webhook receiver. It verifies every
// delivery's signature, applies each journal entry exactly once and keeps
// working across its own crashes and the server's.
//
// Delivery from ledgerd is at least once and in order per endpoint: the
// server moves an endpoint's position only after a 2xx, so a crash on
// either side, a lost answer or a timeout means the same entries come
// again. A receiver turns that into exactly once by recording what it has
// applied durably before answering, and skipping what it already has.
//
// This one appends each new entry, as one JSON line, to a file (the same
// NDJSON the journal feed and export use), fsyncs it and only then answers
// 200. On start it reads the file back to learn which sequences it holds,
// and cuts off a last line that a crash left half written. A real service
// would apply the entry and record its sequence in one database
// transaction instead; the shape is the same.
//
// Usage:
//
//	webhook -addr 127.0.0.1:9090 -secret whsec_... -out entries.ndjson
//
// The secret may also come from LEDGERD_WEBHOOK_SECRET. -secret may be
// given more than once while a secret is being rotated; a delivery signed
// with any of them is accepted.
//
// Answers: 200 once the delivery's new entries are on disk (duplicates
// included, so the server moves on), 401 for a missing, malformed, wrong
// or stale signature, 400 for a body that is not a delivery, 413 for one
// over -max-body, and 500 when the file cannot be written, so the server
// retries. Only the standard library is used, so the verification code can
// be copied into any Go service.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Headers ledgerd sends with every delivery.
const (
	signatureHeader = "Ledgerd-Signature"
	idHeader        = "Ledgerd-Webhook-Id"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "webhook:", err)
		os.Exit(1)
	}
}

type secrets []string

func (s *secrets) String() string     { return fmt.Sprint(len(*s), " secrets") }
func (s *secrets) Set(v string) error { *s = append(*s, v); return nil }

// run serves until ctx ends, then shuts down cleanly.
func run(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("webhook", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:9090", "address to listen on")
	path := fs.String("path", "/hook", "URL path deliveries are POSTed to")
	out := fs.String("out", "entries.ndjson", "file the applied entries are appended to, one JSON line each")
	tolerance := fs.Duration("tolerance", 5*time.Minute, "how far a signature's timestamp may be from this clock")
	maxBody := fs.Int64("max-body", 64<<20, "largest delivery accepted, in bytes")
	var keys secrets
	fs.Var(&keys, "secret", "the endpoint's secret (repeat while rotating; default $LEDGERD_WEBHOOK_SECRET)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(keys) == 0 {
		if s := os.Getenv("LEDGERD_WEBHOOK_SECRET"); s != "" {
			keys = append(keys, s)
		}
	}
	if len(keys) == 0 {
		return errors.New("a secret is required: -secret or LEDGERD_WEBHOOK_SECRET")
	}
	log := slog.New(slog.NewJSONHandler(stdout, nil))

	r, err := openReceiver(*out, keys, *tolerance, *maxBody, time.Now, log)
	if err != nil {
		return err
	}
	defer r.Close()
	log.Info("recovered", "file", *out, "entries", r.Len(), "highest_seq", r.Highest(), "truncated_bytes", r.truncated)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("POST "+*path, r)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String(), "path", *path)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	log.Info("stopped", "entries", r.Len(), "highest_seq", r.Highest())
	return nil
}

// Errors from verify.
var (
	errNoSignature  = errors.New("the signature header is missing or malformed")
	errBadSignature = errors.New("no signature matches the body")
	errStale        = errors.New("the signature's timestamp is outside the tolerance")
)

// verify checks a Ledgerd-Signature header, "t=<unix seconds>,v1=<hex>",
// against the raw body: some v1 value must equal HMAC-SHA256 under one of
// the secrets of the timestamp, a full stop and the body, compared in
// constant time, and the timestamp must be within tolerance of now, so a
// captured delivery cannot be played back later.
func verify(keys []string, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts string
	var sigs [][]byte
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return errNoSignature
		}
		switch k {
		case "t":
			if ts != "" {
				return errNoSignature
			}
			ts = v
		case "v1":
			b, err := hex.DecodeString(v)
			if err != nil || len(b) != sha256.Size {
				return errNoSignature
			}
			sigs = append(sigs, b)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 || len(ts) > 19 {
		return errNoSignature
	}
	match := false
	for _, key := range keys {
		m := hmac.New(sha256.New, []byte(key))
		m.Write([]byte(ts))
		m.Write([]byte{'.'})
		m.Write(body)
		want := m.Sum(nil)
		for _, s := range sigs {
			if hmac.Equal(s, want) {
				match = true
			}
		}
	}
	if !match {
		return errBadSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return errStale
	}
	return nil
}

// delivery is the body of a webhook request.
type delivery struct {
	ID       string            `json:"id"`
	Endpoint string            `json:"endpoint"`
	Replay   bool              `json:"replay"`
	Entries  []json.RawMessage `json:"entries"`
}

// receiver applies deliveries to an append-only file.
type receiver struct {
	keys      []string
	tolerance time.Duration
	maxBody   int64
	now       func() time.Time
	log       *slog.Logger

	mu        sync.Mutex // serializes appends, so the file and seen agree
	f         *os.File
	seen      map[uint64]struct{}
	highest   uint64
	truncated int64 // bytes of a half-written last line cut off at open
}

// openReceiver opens or creates the file and reads back which sequences
// it holds. A last line without its newline, or one that is not an entry,
// was cut short by a crash before its fsync, so it was never answered 200:
// it is removed and the server will send it again.
func openReceiver(path string, keys []string, tolerance time.Duration, maxBody int64, now func() time.Time, log *slog.Logger) (*receiver, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	r := &receiver{keys: keys, tolerance: tolerance, maxBody: maxBody, now: now, log: log, f: f, seen: map[uint64]struct{}{}}
	good, err := r.load()
	if err != nil {
		f.Close()
		return nil, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, err
	}
	if size > good {
		if err := f.Truncate(good); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
		r.truncated = size - good
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return r, nil
}

// load reads every complete line and returns the offset after the last
// good one. A bad line before the end is corruption, not a torn write, and
// is an error rather than something to cut away.
func (r *receiver) load() (int64, error) {
	if _, err := r.f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	br := bufio.NewReaderSize(r.f, 1<<16)
	var off int64
	for line := 1; ; line++ {
		b, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return off, nil // b, if any, is a torn last line
		}
		if err != nil {
			return 0, err
		}
		seq, perr := entrySeq(b)
		if perr != nil {
			if _, err := br.Peek(1); errors.Is(err, io.EOF) {
				return off, nil // a torn last line that happens to end in a newline
			}
			return 0, fmt.Errorf("%s line %d: %v", r.f.Name(), line, perr)
		}
		if _, dup := r.seen[seq]; dup {
			return 0, fmt.Errorf("%s line %d: entry %d appears twice", r.f.Name(), line, seq)
		}
		r.seen[seq] = struct{}{}
		r.highest = max(r.highest, seq)
		off += int64(len(b))
	}
}

func entrySeq(b []byte) (uint64, error) {
	var e struct {
		Seq *uint64 `json:"seq"`
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return 0, err
	}
	if e.Seq == nil || *e.Seq == 0 {
		return 0, errors.New("an entry without a sequence")
	}
	return *e.Seq, nil
}

// Len is how many distinct entries have been applied.
func (r *receiver) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

// Highest is the highest sequence applied.
func (r *receiver) Highest() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.highest
}

func (r *receiver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, r.maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			r.refuse(w, http.StatusRequestEntityTooLarge, "", err)
		}
		return // otherwise the sender gave up mid-body
	}
	// Verify before parsing anything: an unsigned body is not looked at.
	if err := verify(r.keys, req.Header.Get(signatureHeader), body, r.now(), r.tolerance); err != nil {
		r.refuse(w, http.StatusUnauthorized, req.Header.Get(idHeader), err)
		return
	}
	var d delivery
	if err := json.Unmarshal(body, &d); err != nil {
		r.refuse(w, http.StatusBadRequest, req.Header.Get(idHeader), err)
		return
	}
	if d.ID == "" || d.ID != req.Header.Get(idHeader) {
		r.refuse(w, http.StatusBadRequest, req.Header.Get(idHeader), fmt.Errorf("delivery %q arrived under ID header %q", d.ID, req.Header.Get(idHeader)))
		return
	}
	applied, dups, err := r.apply(d.Entries)
	if err != nil {
		var bad badEntry
		if errors.As(err, &bad) {
			r.refuse(w, http.StatusBadRequest, d.ID, err)
			return
		}
		r.log.Error("write failed", "delivery", d.ID, "error", err.Error())
		http.Error(w, "could not record the entries", http.StatusInternalServerError)
		return
	}
	r.log.Info("delivery", "delivery", d.ID, "endpoint", d.Endpoint, "replay", d.Replay,
		"entries", len(d.Entries), "applied", applied, "duplicates", dups)
	w.WriteHeader(http.StatusOK)
}

type badEntry struct{ error }

func (r *receiver) refuse(w http.ResponseWriter, status int, id string, err error) {
	r.log.Warn("refused", "status", status, "delivery", id, "error", err.Error())
	http.Error(w, http.StatusText(status), status)
}

// apply appends the entries not yet held, as one write followed by an
// fsync, and only then counts them as held. Within one endpoint entries
// arrive in sequence order, so skipping those at or below the highest
// applied would do; a set also covers a replayed dead letter, which comes
// after later entries and may hold entries this receiver already applied
// when its answer was lost.
func (r *receiver) apply(entries []json.RawMessage) (applied, dups int, err error) {
	seqs := make([]uint64, len(entries))
	for i, e := range entries {
		if seqs[i], err = entrySeq(e); err != nil {
			return 0, 0, badEntry{fmt.Errorf("entry %d: %v", i, err)}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var buf bytes.Buffer
	var fresh []uint64
	inBatch := map[uint64]struct{}{}
	for i, e := range entries {
		_, held := r.seen[seqs[i]]
		_, twice := inBatch[seqs[i]]
		if held || twice {
			dups++
			continue
		}
		inBatch[seqs[i]] = struct{}{}
		if err := json.Compact(&buf, e); err != nil {
			return 0, 0, badEntry{fmt.Errorf("entry %d: %v", seqs[i], err)}
		}
		buf.WriteByte('\n')
		fresh = append(fresh, seqs[i])
	}
	if len(fresh) == 0 {
		return 0, dups, nil
	}
	start, err := r.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, 0, err
	}
	if _, err := r.f.Write(buf.Bytes()); err != nil {
		r.f.Truncate(start) // leave no partial line behind a 500
		r.f.Seek(start, io.SeekStart)
		return 0, 0, err
	}
	if err := r.f.Sync(); err != nil {
		// What reached the disk is unknown, and a retry would append the
		// entries again. Stop: a restart reads back what the file holds.
		r.log.Error("fsync failed; exiting so a restart reads back the file", "error", err.Error())
		os.Exit(1)
	}
	for _, s := range fresh {
		r.seen[s] = struct{}{}
		r.highest = max(r.highest, s)
	}
	return len(fresh), dups, nil
}
