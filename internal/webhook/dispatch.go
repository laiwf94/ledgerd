package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
	"github.com/laiwf94/ledgerd/internal/metrics"
)

// The dispatcher runs one worker per registered endpoint. A worker reads
// the journal feed from the endpoint's position with the endpoint's filter,
// up to MaxBatch entries at a time, and POSTs them as one signed delivery.
// Only once the receiver answers 2xx does it log the new position with a
// webhook_progress command, so every entry reaches every endpoint at least
// once and in sequence order: a crash, a lost response or a refused
// progress command (the endpoint changed meanwhile) means the same entries
// are sent again, never that they are skipped.
//
// A failed attempt is retried after an exponentially growing, jittered
// delay. After MaxAttempts the delivery becomes a dead letter, logged in
// the same command that moves the position past it, and the worker carries
// on with the entries after it. An endpoint holding MaxDeadLetters dead
// letters takes no more: its worker keeps retrying the delivery at the
// longest delay instead, so nothing is dropped. Replayed dead letters are
// redelivered before the worker carries on from its position.
//
// Workers hold no queue. They wait on ledger.EntriesGrown at the head and
// on ledger.WebhooksChanged for changes to their endpoint, so an idle
// endpoint costs a goroutine and nothing else, and the writer never waits
// for a delivery.

// Defaults for Options.
const (
	DefaultTimeout     = 10 * time.Second
	DefaultBackoffBase = time.Second
	DefaultBackoffMax  = time.Hour
	DefaultMaxAttempts = 20
	// skipPersist is how far a filtered endpoint's scan may run ahead of
	// its logged position before the position is logged without a
	// delivery, so a restart does not rescan a long stretch of entries
	// the filter never matches.
	skipPersist = 4096
	// maxResponse is how much of a receiver's response body is read
	// before the connection is reused.
	maxResponse = 64 << 10
	userAgent   = "ledgerd-webhook/1"
)

// Options configures a Dispatcher.
type Options struct {
	Ledger *ledger.Ledger
	// Client sends deliveries. The default follows no redirects, so a 3xx
	// answer is a failed attempt.
	Client *http.Client
	// Timeout bounds one attempt, response included. Default 10s.
	Timeout time.Duration
	// BackoffBase is the delay after the first failed attempt; each further
	// failure doubles it, up to BackoffMax. The delay actually waited is
	// drawn between half and all of it. Defaults 1s and 1h.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// MaxAttempts is how many times a delivery is tried before it becomes
	// a dead letter. Default 20, about nine hours with the default delays.
	MaxAttempts int
	// Clock stamps signatures. Default time.Now.
	Clock   func() time.Time
	Logger  *slog.Logger
	Metrics *metrics.Registry
}

// Delivery is the JSON body of every webhook request.
type Delivery struct {
	// ID is the same for every attempt at the same entries; see IDHeader.
	ID       string `json:"id"`
	Endpoint string `json:"endpoint"`
	// Replay is set on a redelivered dead letter.
	Replay  bool           `json:"replay,omitempty"`
	Entries []ledger.Entry `json:"entries"`
}

// Dispatcher delivers entries to every registered endpoint until Close.
type Dispatcher struct {
	o      Options
	l      *ledger.Ledger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	workers map[string]context.CancelFunc

	mOK, mFailed, mDead, mEntries, mRedelivered *metrics.Counter
	mAttempt                                    *metrics.Histogram
}

// Start starts delivering to the ledger's endpoints.
func Start(o Options) (*Dispatcher, error) {
	if o.Ledger == nil {
		return nil, errors.New("webhook: a ledger is required")
	}
	if o.Client == nil {
		o.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = DefaultBackoffBase
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = DefaultBackoffMax
	}
	o.BackoffMax = max(o.BackoffMax, o.BackoffBase)
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if o.Metrics == nil {
		o.Metrics = metrics.New()
	}
	d := &Dispatcher{o: o, l: o.Ledger, workers: map[string]context.CancelFunc{}}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	m := o.Metrics
	d.mOK = m.Counter("ledgerd_webhook_attempts_total", `result="delivered"`, "Webhook delivery attempts.")
	d.mFailed = m.Counter("ledgerd_webhook_attempts_total", `result="failed"`, "Webhook delivery attempts.")
	d.mDead = m.Counter("ledgerd_webhook_dead_letters_total", "", "Webhook deliveries given up on after every attempt.")
	d.mRedelivered = m.Counter("ledgerd_webhook_redeliveries_total", "", "Dead letters delivered by a replay.")
	d.mEntries = m.Counter("ledgerd_webhook_entries_delivered_total", "", "Journal entries in accepted webhook deliveries.")
	d.mAttempt = m.Histogram("ledgerd_webhook_attempt_duration_seconds", "", "Time for one webhook delivery attempt.")
	m.Gauge("ledgerd_webhook_endpoints", "Registered webhook endpoints.", func() float64 { return float64(len(d.l.Webhooks())) })
	m.Gauge("ledgerd_webhook_backlog_entries", "Journal entries after each endpoint's position, summed over endpoints.", func() float64 {
		head := d.l.Head()
		var n uint64
		for _, h := range d.l.Webhooks() {
			n += head - min(h.Cursor, head)
		}
		return float64(n)
	})
	d.wg.Add(1)
	go d.run()
	return d, nil
}

// Close stops every worker and waits for them. A delivery in flight is
// abandoned; its entries are sent again after the next start.
func (d *Dispatcher) Close() {
	d.cancel()
	d.wg.Wait()
}

// run starts a worker for each new endpoint and stops the workers of
// deleted ones.
func (d *Dispatcher) run() {
	defer d.wg.Done()
	for {
		changed := d.l.WebhooksChanged()
		live := map[string]bool{}
		for _, h := range d.l.Webhooks() {
			live[h.ID] = true
		}
		d.mu.Lock()
		for id, stop := range d.workers {
			if !live[id] {
				stop()
				delete(d.workers, id)
			}
		}
		for id := range live {
			if _, ok := d.workers[id]; !ok {
				ctx, stop := context.WithCancel(d.ctx)
				d.workers[id] = stop
				w := &worker{d: d, id: id, ctx: ctx}
				d.wg.Add(1)
				go func() {
					defer d.wg.Done()
					w.run()
				}()
			}
		}
		d.mu.Unlock()
		select {
		case <-changed:
		case <-d.ctx.Done():
			return
		}
	}
}

type worker struct {
	d   *Dispatcher
	id  string
	ctx context.Context
}

// run delivers to one endpoint until its context ends.
func (w *worker) run() {
	l := w.d.l
	var version, scanned uint64
	for w.ctx.Err() == nil {
		if err := l.Err(); err != nil {
			// The ledger stopped taking writes after a storage error and
			// stays stopped until a restart, so no position can be logged.
			// Delivering now would only send the same entries again and
			// again; after the restart they are sent once more.
			w.d.o.Logger.Warn("webhook: the ledger stopped taking writes; deliveries resume after a restart", "webhook", w.id, "error", err.Error())
			<-w.ctx.Done()
			return
		}
		changed := l.WebhooksChanged()
		grown := l.EntriesGrown()
		h, err := l.Webhook(w.id)
		if err != nil {
			// Deleted; the dispatcher stops this worker, or starts over
			// with the endpoint created again under the same ID.
			w.wait(changed, nil)
			continue
		}
		if h.Version != version {
			version, scanned = h.Version, h.Cursor
		}
		if dl := replaying(h); dl != nil {
			w.redeliver(h, dl)
			continue
		}
		page, err := l.Entries(h.Filter(), scanned, h.MaxBatch)
		if err != nil {
			w.d.o.Logger.Error("webhook: reading the journal failed", "webhook", h.ID, "error", err.Error())
			w.sleep(w.d.o.BackoffBase, h.Version)
			continue
		}
		if len(page.Entries) == 0 {
			scanned = page.Next
			if scanned-h.Cursor >= skipPersist {
				w.progress(&ledger.WebhookProgressSpec{ID: h.ID, Version: h.Version, Through: scanned})
				continue
			}
			if !page.HasMore() {
				w.wait(changed, grown)
			}
			continue
		}
		last := page.Entries[len(page.Entries)-1].Seq
		attempts, ok, derr := w.deliver(h, page.Entries, false)
		switch {
		case ok:
			w.progress(&ledger.WebhookProgressSpec{ID: h.ID, Version: h.Version, Through: page.Next})
		case derr != nil:
			w.d.mDead.Inc()
			w.d.o.Logger.Warn("webhook: delivery failed every attempt; kept as a dead letter", "webhook", h.ID,
				"after_seq", scanned, "through_seq", last, "attempts", attempts, "error", derr.Error())
			w.progress(&ledger.WebhookProgressSpec{ID: h.ID, Version: h.Version, Through: page.Next,
				Dead: &ledger.DeadLetterSpec{AfterSeq: scanned, ThroughSeq: last, Entries: len(page.Entries), Attempts: attempts, Error: errText(derr)}})
		}
	}
}

// replaying returns the oldest dead letter marked for redelivery.
func replaying(h *ledger.Webhook) *ledger.DeadLetter {
	for i := range h.DeadLetters {
		if h.DeadLetters[i].Replaying {
			return &h.DeadLetters[i]
		}
	}
	return nil
}

// redeliver sends a dead letter's entries again: the entries matching the
// endpoint's filter in its range, which the journal still holds unchanged.
func (w *worker) redeliver(h *ledger.Webhook, dl *ledger.DeadLetter) {
	var entries []ledger.Entry
	after := dl.AfterSeq
	for after < dl.ThroughSeq {
		page, err := w.d.l.Entries(h.Filter(), after, h.MaxBatch)
		if err != nil {
			w.d.o.Logger.Error("webhook: reading the journal failed", "webhook", h.ID, "error", err.Error())
			w.sleep(w.d.o.BackoffBase, h.Version)
			return
		}
		for _, e := range page.Entries {
			if e.Seq <= dl.ThroughSeq {
				entries = append(entries, e)
			}
		}
		after = page.Next
	}
	if len(entries) == 0 {
		w.progress(&ledger.WebhookProgressSpec{ID: h.ID, Version: h.Version, Redelivered: dl.ID})
		return
	}
	attempts, ok, err := w.deliver(h, entries, true)
	switch {
	case ok:
		w.d.mRedelivered.Inc()
		w.progress(&ledger.WebhookProgressSpec{ID: h.ID, Version: h.Version, Redelivered: dl.ID})
	case err != nil:
		w.d.o.Logger.Warn("webhook: replayed dead letter failed again", "webhook", h.ID, "dead_letter", dl.ID, "error", err.Error())
		w.progress(&ledger.WebhookProgressSpec{ID: h.ID, Version: h.Version,
			Failed: &ledger.RedeliveryFailed{DeadLetter: dl.ID, Attempts: attempts, Error: errText(err)}})
	}
}

// progress logs what a delivery achieved. A refusal because the endpoint
// changed meanwhile is expected: the worker reads it again and carries on
// from there. Any other failure (the ledger stopped taking writes) waits
// and lets the delivery be repeated.
func (w *worker) progress(p *ledger.WebhookProgressSpec) {
	ctx, cancel := context.WithTimeout(w.ctx, w.d.o.Timeout)
	defer cancel()
	_, err := w.d.l.Do(ctx, ledger.Command{WebhookProgress: p}, nil)
	var le *ledger.Error
	switch {
	case err == nil:
	case errors.As(err, &le) && (le.Code == ledger.CodePrecondition || le.Code == ledger.CodeNotFound):
	case errors.As(err, &le) && le.Code == ledger.CodeLimitExceeded:
		// The dead letter list is full: keep the entries and try them again
		// at the longest delay.
		w.d.o.Logger.Warn("webhook: dead letter list is full; retrying instead", "webhook", p.ID)
		w.sleep(w.d.o.BackoffMax, p.Version)
	default:
		if w.ctx.Err() == nil && w.d.l.Err() == nil {
			w.d.o.Logger.Warn("webhook: logging delivery progress failed; the entries will be sent again", "webhook", p.ID, "error", err.Error())
			w.sleep(w.d.o.BackoffBase, p.Version)
		}
	}
}

// deliver sends entries until an attempt succeeds or MaxAttempts have
// failed. It returns the number of attempts, whether the receiver accepted
// them, and the last error. With neither ok nor an error, the delivery
// was abandoned: the worker is stopping or the endpoint changed.
func (w *worker) deliver(h *ledger.Webhook, entries []ledger.Entry, replay bool) (int, bool, error) {
	id := fmt.Sprintf("%s.%d.%d", h.ID, entries[0].Seq, entries[len(entries)-1].Seq)
	body, err := json.Marshal(Delivery{ID: id, Endpoint: h.ID, Replay: replay, Entries: entries})
	if err != nil {
		return 1, false, err
	}
	for attempt := 1; ; attempt++ {
		err := w.attempt(h, id, body)
		if err == nil {
			w.d.mOK.Inc()
			w.d.mEntries.Add(uint64(len(entries)))
			return attempt, true, nil
		}
		if w.ctx.Err() != nil {
			return attempt, false, nil
		}
		w.d.mFailed.Inc()
		w.d.o.Logger.Debug("webhook: attempt failed", "webhook", h.ID, "delivery", id, "attempt", attempt, "error", err.Error())
		if attempt >= w.d.o.MaxAttempts {
			return attempt, false, err
		}
		if !w.sleep(w.d.backoff(attempt), h.Version) {
			return attempt, false, nil
		}
	}
}

// attempt makes one signed POST.
func (w *worker) attempt(h *ledger.Webhook, id string, body []byte) error {
	ctx, cancel := context.WithTimeout(w.ctx, w.d.o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(IDHeader, id)
	req.Header.Set(SignatureHeader, Sign(h.Secret, w.d.o.Clock(), body))
	start := time.Now()
	resp, err := w.d.o.Client.Do(req)
	w.d.mAttempt.Observe(time.Since(start))
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the endpoint answered %d", resp.StatusCode)
	}
	return nil
}

// backoff is the delay after the given failed attempt: BackoffBase doubled
// for each attempt before it, capped at BackoffMax, with the lower half
// jittered so endpoints that failed together do not retry together.
func (d *Dispatcher) backoff(attempt int) time.Duration {
	delay := d.o.BackoffMax
	if attempt-1 < 62 {
		if b := d.o.BackoffBase << (attempt - 1); b > 0 && b < delay {
			delay = b
		}
	}
	half := delay / 2
	return delay - half + rand.N(half+1)
}

// sleep waits for delay. It returns false early if the worker stops or the
// endpoint changes from version (it was deleted, or replayed elsewhere).
func (w *worker) sleep(delay time.Duration, version uint64) bool {
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		changed := w.d.l.WebhooksChanged()
		if h, err := w.d.l.Webhook(w.id); err != nil || h.Version != version {
			return false
		}
		select {
		case <-t.C:
			return true
		case <-w.ctx.Done():
			return false
		case <-changed:
		}
	}
}

// wait blocks until the endpoints change, entries are appended (when grown
// is set) or the worker stops.
func (w *worker) wait(changed, grown <-chan struct{}) {
	select {
	case <-changed:
	case <-grown:
	case <-w.ctx.Done():
	}
}

// errText is an error as a dead letter keeps it: printable and short.
func errText(err error) string {
	b := []rune(err.Error())
	out := make([]rune, 0, min(len(b), ledger.MaxWebhookError))
	n := 0
	for _, r := range b {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			r = ' '
		}
		if n+len(string(r)) > ledger.MaxWebhookError {
			break
		}
		n += len(string(r))
		out = append(out, r)
	}
	return string(out)
}
