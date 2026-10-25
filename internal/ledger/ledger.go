// Package ledger is a double-entry ledger: accounts with balances, balanced
// journal entries, two-phase transfers, reversals, idempotent commands and a
// durable command log with group commit.
//
// All state changes go through one writer goroutine, in the style of
// TigerBeetle's single-threaded state machine. The writer takes every command
// that is waiting, applies them in order to a transaction on top of the
// committed state, writes all their log records with one fsync, and only then
// makes the results visible and replies. Readers never see a change that is
// not yet on disk, and many concurrent transfers share one disk flush.
package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/laiwf94/ledgerd/internal/metrics"
	"github.com/laiwf94/ledgerd/internal/wal"
)

// Defaults for Options.
const (
	DefaultIdempotencyTTL = 24 * time.Hour
	DefaultMaxBatch       = 1024
	DefaultSweepInterval  = time.Second
	logName               = "ledger.wal"
)

// ErrClosed is returned for commands sent after Close.
var ErrClosed = &Error{Code: CodeUnavailable, Message: "the ledger is shutting down"}

// Options configures a Ledger.
type Options struct {
	// Dir holds the log. Empty keeps everything in memory, which is only
	// useful for tests and demos.
	Dir string
	// Clock returns the current time. It decides when pending transfers
	// expire and when idempotency keys are forgotten. Default time.Now.
	Clock func() time.Time
	// IdempotencyTTL is how long a key's result is kept. Default 24h, as
	// Stripe documents for its keys.
	IdempotencyTTL time.Duration
	// MaxBatch caps how many commands share one fsync. 1 turns group commit
	// off (the benchmark baseline). Default 1024.
	MaxBatch int
	// SweepInterval is how often due reservations are expired in the
	// background. Commands also expire due reservations before they run.
	// Negative disables the background sweep. Default 1s.
	SweepInterval time.Duration
	// NewID generates transfer IDs. Default "tr_" plus 24 random hex digits.
	NewID func() string
	// WrapFile wraps the log file; tests use it to inject I/O failures.
	WrapFile func(wal.File) wal.File
	Logger   *slog.Logger
	Metrics  *metrics.Registry
}

// Ledger is an open ledger. It is safe for concurrent use.
type Ledger struct {
	opts Options
	log  *wal.Log

	mu sync.RWMutex // guards st for readers; the writer holds it only to commit
	st *state

	queue   chan *request
	quit    chan struct{}
	done    chan struct{}
	closeMu sync.RWMutex
	closed  bool
	fatal   atomic.Pointer[error]

	idemMu   sync.Mutex
	idem     map[string]*idemEntry
	idemFIFO []idemAt
	inflight map[string]*flight

	mCommitted, mRejected, mBatches, mRecords *metrics.Counter
	mCommit                                   *metrics.Histogram
}

type request struct {
	ctx  context.Context
	cmd  *Command // nil asks for an expiry sweep only
	idem *Idem
	resp chan response
}

type response struct {
	out Outcome
	err error
}

type idemEntry struct {
	hash [32]byte
	out  Outcome
	err  *Error
	at   time.Time
}

type idemAt struct {
	id string
	at time.Time
}

type flight struct {
	hash [32]byte
	done chan struct{}
}

// Open recovers the ledger from its log (if Dir is set) and starts the writer.
func Open(opts Options) (*Ledger, error) {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.IdempotencyTTL <= 0 {
		opts.IdempotencyTTL = DefaultIdempotencyTTL
	}
	if opts.MaxBatch <= 0 {
		opts.MaxBatch = DefaultMaxBatch
	}
	if opts.SweepInterval == 0 {
		opts.SweepInterval = DefaultSweepInterval
	}
	if opts.NewID == nil {
		opts.NewID = randomID
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if opts.Metrics == nil {
		opts.Metrics = metrics.New()
	}
	l := &Ledger{opts: opts, st: newState(), queue: make(chan *request, 4*opts.MaxBatch),
		quit: make(chan struct{}), done: make(chan struct{}),
		idem: map[string]*idemEntry{}, inflight: map[string]*flight{}}

	if opts.Dir != "" {
		first := true
		lg, err := wal.Open(filepath.Join(opts.Dir, logName), wal.Options{WrapFile: opts.WrapFile}, func(p []byte) error {
			err := l.replay(p, first)
			first = false
			return err
		})
		if err != nil {
			return nil, err
		}
		l.log = lg
		if err := l.prepareLog(); err != nil {
			lg.Close()
			return nil, err
		}
	}
	l.pruneIdem()
	if v := l.CheckInvariants(); len(v) > 0 {
		if l.log != nil {
			l.log.Close()
		}
		return nil, fmt.Errorf("%w: recovered state breaks %d invariants, first: %s", wal.ErrCorrupt, len(v), v[0])
	}
	l.registerMetrics()
	go l.run()
	return l, nil
}

// prepareLog writes the header into a new log, or compacts an existing one:
// expired idempotency records are dropped. Entries are history and are
// always kept.
func (l *Ledger) prepareLog() error {
	if l.log.Size() == 0 {
		p, _ := encodeRecord(record{Type: recHeader, Version: logVersion})
		return l.log.Append(p)
	}
	cutoff := l.now().Add(-l.opts.IdempotencyTTL)
	err := l.log.Compact(func(p []byte) ([]byte, bool, error) {
		r, err := decodeRecord(p)
		if err != nil {
			return nil, false, err
		}
		if r.Idem == nil || r.Time.After(cutoff) {
			return p, true, nil
		}
		if r.Type == recReject {
			return nil, false, nil
		}
		r.Idem = nil
		out, err := encodeRecord(r)
		return out, true, err
	})
	if err != nil && !errors.Is(err, wal.ErrBroken) {
		// The old log is intact; running on it is safe, only larger.
		l.opts.Logger.Warn("log compaction failed; continuing with the uncompacted log", "error", err.Error())
		return nil
	}
	return err
}

// replay applies one log record during recovery.
func (l *Ledger) replay(p []byte, first bool) error {
	r, err := decodeRecord(p)
	if err != nil {
		return err
	}
	if first != (r.Type == recHeader) {
		return errors.New("the log must start with exactly one header")
	}
	var idem *Idem
	if r.Idem != nil {
		if idem, err = r.Idem.idem(); err != nil {
			return err
		}
	}
	switch r.Type {
	case recHeader:
		if r.Version != logVersion {
			return fmt.Errorf("log format version %d is not supported", r.Version)
		}
		return nil
	case recReject:
		if idem == nil || r.Err == nil {
			return errors.New("reject record without key or error")
		}
		l.storeIdem(idem, &idemEntry{hash: idem.Hash, err: r.Err, at: r.Time})
		return nil
	case recCmd, recExpire:
		t := newTxn(l.st, r.Time)
		var out Outcome
		var aerr *Error
		if r.Type == recExpire {
			out, aerr = t.release(r.ID, KindExpirePending, StateExpired)
		} else {
			if r.Cmd == nil {
				return errors.New("command record without a command")
			}
			if verr := r.Cmd.Validate(); verr != nil {
				return verr
			}
			out, aerr = t.apply(r.Cmd, r.ID)
		}
		if aerr != nil {
			return fmt.Errorf("replaying %s record: %v", r.Type, aerr)
		}
		var seq uint64
		if len(t.entries) > 0 {
			seq = t.entries[len(t.entries)-1].Seq
		}
		if seq != r.Seq {
			return fmt.Errorf("record produced entry %d, log says %d", seq, r.Seq)
		}
		t.commit()
		if idem != nil {
			l.storeIdem(idem, &idemEntry{hash: idem.Hash, out: out, at: r.Time})
		}
		return nil
	}
	return fmt.Errorf("unknown record type %q", r.Type)
}

func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "tr_" + hex.EncodeToString(b[:])
}

func (l *Ledger) now() time.Time { return l.opts.Clock().UTC().Round(0) }

func (l *Ledger) registerMetrics() {
	m := l.opts.Metrics
	l.mCommitted = m.Counter("ledgerd_commands_total", `result="committed"`, "Commands executed by the writer.")
	l.mRejected = m.Counter("ledgerd_commands_total", `result="rejected"`, "Commands executed by the writer.")
	l.mBatches = m.Counter("ledgerd_log_batches_total", "", "Group commits: batches written with one fsync.")
	l.mRecords = m.Counter("ledgerd_log_records_total", "", "Records written to the log.")
	l.mCommit = m.Histogram("ledgerd_commit_duration_seconds", "", "Time to write and fsync one batch.")
	m.Gauge("ledgerd_journal_head", "Sequence number of the newest journal entry.", func() float64 { return float64(l.Head()) })
	m.Gauge("ledgerd_pending_transfers", "Open pending transfers.", func() float64 { return float64(l.Stats().OpenPending) })
}

// Do validates and executes one command. With idem set, it follows Stripe's
// idempotency-key semantics: the first request with a key executes, and its
// result (success or error) is returned to every later request with the same
// key and the same request hash. A different request with the same key is
// rejected with CodeKeyReused. A request that arrives while the first is
// still executing waits for it, or gets CodeInProgress if ctx ends first.
func (l *Ledger) Do(ctx context.Context, cmd Command, idem *Idem) (Outcome, error) {
	if err := cmd.Validate(); err != nil {
		return Outcome{}, err
	}
	if idem == nil {
		return l.submit(ctx, &cmd, nil)
	}
	if err := idem.validate(); err != nil {
		return Outcome{}, err
	}
	id := idem.id()
	for {
		l.idemMu.Lock()
		if e, ok := l.idem[id]; ok && l.now().Before(e.at.Add(l.opts.IdempotencyTTL)) {
			l.idemMu.Unlock()
			if e.hash != idem.Hash {
				return Outcome{}, errf(CodeKeyReused, "this idempotency key was already used for a different request")
			}
			out := e.out
			out.Replayed = true
			if e.err != nil {
				return out, e.err
			}
			return out, nil
		}
		if f, ok := l.inflight[id]; ok {
			l.idemMu.Unlock()
			if f.hash != idem.Hash {
				return Outcome{}, errf(CodeKeyReused, "this idempotency key is in use by a different request")
			}
			select {
			case <-f.done:
				continue
			case <-ctx.Done():
				return Outcome{}, errf(CodeInProgress, "a request with this idempotency key is still in progress; retry later")
			}
		}
		l.inflight[id] = &flight{hash: idem.Hash, done: make(chan struct{})}
		l.idemMu.Unlock()
		return l.submit(ctx, &cmd, idem)
	}
}

// ExpireDue releases every pending transfer whose timeout has passed and
// returns how many it released. The background sweep calls the same code.
func (l *Ledger) ExpireDue(ctx context.Context) (int, error) {
	out, err := l.submit(ctx, nil, nil)
	return out.Expired, err
}

func (l *Ledger) submit(ctx context.Context, cmd *Command, idem *Idem) (Outcome, error) {
	req := &request{ctx: ctx, cmd: cmd, idem: idem, resp: make(chan response, 1)}
	if err := l.enqueue(req); err != nil {
		l.finishFlight(idem, nil)
		return Outcome{}, err
	}
	// From here the writer owns the request, including the idempotency
	// flight: it completes even if this caller stops waiting.
	select {
	case r := <-req.resp:
		return r.out, r.err
	case <-ctx.Done():
		return Outcome{}, ctx.Err()
	}
}

func (l *Ledger) enqueue(req *request) error {
	l.closeMu.RLock()
	defer l.closeMu.RUnlock()
	if l.closed {
		return ErrClosed
	}
	if err := l.unavailable(); err != nil {
		return err
	}
	select {
	case l.queue <- req:
		return nil
	case <-req.ctx.Done():
		return req.ctx.Err()
	}
}

// unavailable returns the error that stopped the writer, if any.
func (l *Ledger) unavailable() error {
	if p := l.fatal.Load(); p != nil {
		return &Error{Code: CodeUnavailable, Message: "the ledger stopped accepting writes after a storage error; restart to recover"}
	}
	return nil
}

// Err returns the storage error that stopped the writer, or nil.
func (l *Ledger) Err() error {
	if p := l.fatal.Load(); p != nil {
		return *p
	}
	return nil
}

func (l *Ledger) halt(err error) {
	if l.fatal.CompareAndSwap(nil, &err) {
		l.opts.Logger.Error("ledger stopped accepting writes", "error", err.Error())
	}
}

func (l *Ledger) storeIdem(i *Idem, e *idemEntry) {
	id := i.id()
	l.idem[id] = e
	l.idemFIFO = append(l.idemFIFO, idemAt{id: id, at: e.at})
}

// finishFlight records a result (when there is one) and wakes requests
// waiting on the same key.
func (l *Ledger) finishFlight(i *Idem, e *idemEntry) {
	if i == nil {
		return
	}
	l.idemMu.Lock()
	defer l.idemMu.Unlock()
	if e != nil {
		l.storeIdem(i, e)
	}
	id := i.id()
	if f, ok := l.inflight[id]; ok {
		delete(l.inflight, id)
		close(f.done)
	}
}

// pruneIdem forgets keys older than the retention window. Keys arrive in
// time order, so the oldest are at the front.
func (l *Ledger) pruneIdem() {
	l.idemMu.Lock()
	defer l.idemMu.Unlock()
	cutoff := l.now().Add(-l.opts.IdempotencyTTL)
	n := 0
	for n < len(l.idemFIFO) && !l.idemFIFO[n].at.After(cutoff) {
		q := l.idemFIFO[n]
		if e, ok := l.idem[q.id]; ok && e.at.Equal(q.at) {
			delete(l.idem, q.id)
		}
		n++
	}
	l.idemFIFO = append(l.idemFIFO[:0:0], l.idemFIFO[n:]...)
}

// run is the writer loop.
func (l *Ledger) run() {
	defer close(l.done)
	var tick <-chan time.Time
	if l.opts.SweepInterval > 0 {
		t := time.NewTicker(l.opts.SweepInterval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case req := <-l.queue:
			l.process(l.drain(req))
		case <-tick:
			l.pruneIdem()
			if l.dueExpiries() {
				l.process(nil)
			}
		case <-l.quit:
			// Close stopped new submissions; finish what was queued.
			for {
				select {
				case req := <-l.queue:
					l.process(l.drain(req))
				default:
					return
				}
			}
		}
	}
}

// drain collects every request already waiting, up to MaxBatch. This is the
// whole group-commit mechanism: while one batch is being fsynced, the next
// one queues up behind it.
func (l *Ledger) drain(first *request) []*request {
	batch := []*request{first}
	for len(batch) < l.opts.MaxBatch {
		select {
		case req := <-l.queue:
			batch = append(batch, req)
		default:
			return batch
		}
	}
	return batch
}

func (l *Ledger) dueExpiries() bool {
	h := l.st.expiries // only the writer touches the heap
	return len(h) > 0 && !h[0].at.After(l.now())
}

type result struct {
	out  Outcome
	err  error
	idem *idemEntry
}

// process applies a batch, makes it durable with one fsync, publishes it to
// readers and replies.
func (l *Ledger) process(batch []*request) {
	results := make([]result, len(batch))
	reply := func() {
		for i, req := range batch {
			l.finishFlight(req.idem, results[i].idem)
			req.resp <- response{out: results[i].out, err: results[i].err}
		}
	}
	failAll := func(err error) {
		for i := range results {
			results[i] = result{err: err}
		}
		reply()
	}
	if err := l.unavailable(); err != nil {
		failAll(err)
		return
	}
	now := l.now()
	t := newTxn(l.st, now)
	var recs [][]byte
	expired, err := t.sweep()
	if err != nil {
		l.halt(err)
		failAll(l.unavailable())
		return
	}
	for _, tr := range expired {
		p, _ := encodeRecord(record{Type: recExpire, Time: now, Seq: tr.Entries[len(tr.Entries)-1].Seq, ID: tr.ID})
		recs = append(recs, p)
	}
	committed, rejected := 0, 0
	for i, req := range batch {
		if req.cmd == nil {
			results[i].out = Outcome{Expired: len(expired)}
			continue
		}
		if err := req.ctx.Err(); err != nil {
			results[i].err = err // never executed; a retry runs it fresh
			continue
		}
		var id string
		if req.cmd.CreateTransfer != nil || req.cmd.Reverse != nil {
			id = l.opts.NewID()
		}
		before := t.nextSeq()
		out, aerr := t.apply(req.cmd, id)
		rec := record{Time: now, Idem: toIdemField(req.idem)}
		if aerr != nil {
			rejected++
			results[i].err = aerr
			if req.idem == nil {
				continue
			}
			rec.Type, rec.Err = recReject, aerr
		} else {
			committed++
			results[i].out = out
			rec.Type, rec.Cmd, rec.ID = recCmd, req.cmd, id
			if t.nextSeq() != before {
				rec.Seq = t.nextSeq() - 1
			}
		}
		if req.idem != nil {
			results[i].idem = &idemEntry{hash: req.idem.Hash, out: out, err: aerr, at: now}
		}
		p, err := encodeRecord(rec)
		if err != nil {
			l.halt(fmt.Errorf("encoding a log record: %w", err))
			failAll(l.unavailable())
			return
		}
		recs = append(recs, p)
	}
	if len(recs) > 0 && l.log != nil {
		start := time.Now()
		if err := l.log.Append(recs...); err != nil {
			l.halt(err)
			failAll(l.unavailable())
			return
		}
		l.mCommit.Observe(time.Since(start))
		l.mBatches.Inc()
		l.mRecords.Add(uint64(len(recs)))
	}
	l.mu.Lock()
	t.commit()
	l.mu.Unlock()
	l.mCommitted.Add(uint64(committed))
	l.mRejected.Add(uint64(rejected))
	reply()
}

// Close stops accepting commands, finishes the queued ones and closes the log.
func (l *Ledger) Close() error {
	l.closeMu.Lock()
	if l.closed {
		l.closeMu.Unlock()
		return nil
	}
	l.closed = true
	l.closeMu.Unlock()
	close(l.quit)
	<-l.done
	if l.log != nil {
		return l.log.Close()
	}
	return nil
}
