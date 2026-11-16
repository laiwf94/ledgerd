package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
	"github.com/laiwf94/ledgerd/internal/metrics"
	"github.com/laiwf94/ledgerd/internal/wal"
)

const secret = "whsec_test_0123456789abcdefghij"

// receiver is a webhook endpoint that checks every signature and records
// what it accepted. answer decides the status of each attempt; nil accepts
// everything.
type receiver struct {
	t   testing.TB
	srv *httptest.Server

	mu       sync.Mutex
	accepted []Delivery
	attempts map[string]int // by delivery ID
	answer   func(d Delivery, attempt int) int
	bad      []string
}

func newReceiver(t testing.TB) *receiver {
	r := &receiver{t: t, attempts: map[string]int{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	var d Delivery
	err := json.Unmarshal(body, &d)
	r.mu.Lock()
	switch {
	case err != nil:
		r.bad = append(r.bad, "bad body: "+err.Error())
	case req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" || req.Header.Get("User-Agent") != userAgent:
		r.bad = append(r.bad, fmt.Sprintf("bad request %s %q", req.Method, req.Header))
	case req.Header.Get(IDHeader) != d.ID || d.ID != fmt.Sprintf("%s.%d.%d", d.Endpoint, d.Entries[0].Seq, d.Entries[len(d.Entries)-1].Seq):
		r.bad = append(r.bad, fmt.Sprintf("delivery id %q, header %q", d.ID, req.Header.Get(IDHeader)))
	}
	if err := Verify(secret, req.Header.Get(SignatureHeader), body, time.Now(), 0); err != nil {
		r.bad = append(r.bad, "signature: "+err.Error())
	}
	r.attempts[d.ID]++
	n := r.attempts[d.ID]
	answer := r.answer
	r.mu.Unlock()
	code := http.StatusOK
	if answer != nil {
		code = answer(d, n)
	}
	if code >= 200 && code < 300 {
		r.mu.Lock()
		r.accepted = append(r.accepted, d)
		r.mu.Unlock()
	}
	w.WriteHeader(code)
}

func (r *receiver) setAnswer(f func(d Delivery, attempt int) int) {
	r.mu.Lock()
	r.answer = f
	r.mu.Unlock()
}

// seqs returns the sequences of every accepted entry, in arrival order.
func (r *receiver) seqs(endpoint string) []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []uint64
	for _, d := range r.accepted {
		if d.Endpoint == endpoint {
			for _, e := range d.Entries {
				out = append(out, e.Seq)
			}
		}
	}
	return out
}

func (r *receiver) deliveries() []Delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Delivery(nil), r.accepted...)
}

func (r *receiver) check() {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bad) > 0 {
		r.t.Fatalf("receiver saw %d bad requests, first: %s", len(r.bad), r.bad[0])
	}
}

type harness struct {
	t    testing.TB
	l    *ledger.Ledger
	d    *Dispatcher
	opts ledger.Options
	reg  *metrics.Registry
	do   Options
}

func newHarness(t testing.TB, dir string, mutate func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, opts: ledger.Options{Dir: dir, SweepInterval: -1}, reg: metrics.New()}
	h.do = Options{BackoffBase: 5 * time.Millisecond, BackoffMax: 40 * time.Millisecond, MaxAttempts: 4, Timeout: 2 * time.Second, Metrics: h.reg}
	if mutate != nil {
		mutate(&h.do)
	}
	h.open()
	return h
}

func (h *harness) open() {
	h.t.Helper()
	l, err := ledger.Open(h.opts)
	if err != nil {
		h.t.Fatal(err)
	}
	h.l = l
	o := h.do
	o.Ledger = l
	d, err := Start(o)
	if err != nil {
		h.t.Fatal(err)
	}
	h.d = d
	h.t.Cleanup(func() { d.Close(); l.Close() })
}

func (h *harness) restart() {
	h.t.Helper()
	h.d.Close()
	if err := h.l.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.open()
}

func (h *harness) cmd(c ledger.Command) ledger.Outcome {
	h.t.Helper()
	out, err := h.l.Do(context.Background(), c, nil)
	if err != nil {
		h.t.Fatalf("%+v: %v", c, err)
	}
	return out
}

func (h *harness) accounts() {
	h.t.Helper()
	for _, a := range []ledger.AccountSpec{{ID: "bank", Currency: "USD", NormalBalance: ledger.Debit},
		{ID: "alice", Currency: "USD", NormalBalance: ledger.Credit}, {ID: "bob", Currency: "USD", NormalBalance: ledger.Credit}} {
		h.cmd(ledger.Command{CreateAccount: &a})
	}
}

// pay makes n transfers from bank to the account and returns their entry
// sequences.
func (h *harness) pay(to string, n int) []uint64 {
	h.t.Helper()
	var seqs []uint64
	for i := 0; i < n; i++ {
		out := h.cmd(ledger.Command{CreateTransfer: &ledger.TransferSpec{Currency: "USD", Postings: []ledger.PostingSpec{
			{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: to, Direction: ledger.Credit, Amount: 1}}}})
		seqs = append(seqs, out.Transfer.Entries[0].Seq)
	}
	return seqs
}

func (h *harness) hook(spec ledger.WebhookSpec) *ledger.Webhook {
	h.t.Helper()
	if spec.Secret == "" {
		spec.Secret = secret
	}
	return h.cmd(ledger.Command{CreateWebhook: &spec}).Webhook
}

func (h *harness) get(id string) *ledger.Webhook {
	h.t.Helper()
	w, err := h.l.Webhook(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return w
}

// eventually waits up to 20 seconds for cond.
func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func span(from, to uint64) []uint64 {
	var out []uint64
	for s := from; s <= to; s++ {
		out = append(out, s)
	}
	return out
}

func equal(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDeliversEveryEntryInOrderSigned(t *testing.T) {
	r := newReceiver(t)
	h := newHarness(t, t.TempDir(), nil)
	h.accounts()
	h.pay("alice", 5)
	h.hook(ledger.WebhookSpec{ID: "all", URL: r.srv.URL + "/all", AfterSeq: new(uint64), MaxBatch: 3})
	h.hook(ledger.WebhookSpec{ID: "new", URL: r.srv.URL + "/new"}) // from the head: only what comes next
	// Writers keep committing while deliveries run.
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := h.l.Do(context.Background(), ledger.Command{CreateTransfer: &ledger.TransferSpec{Currency: "USD", Postings: []ledger.PostingSpec{
					{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "bob", Direction: ledger.Credit, Amount: 1}}}}, nil); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	head := h.l.Head()
	eventually(t, "every entry at both endpoints", func() bool {
		return h.get("all").Cursor == head && h.get("new").Cursor == head
	})
	r.check()
	if got := r.seqs("all"); !equal(got, span(1, head)) {
		t.Fatalf("all received %v, want 1..%d once each in order", got, head)
	}
	if got := r.seqs("new"); !equal(got, span(6, head)) {
		t.Fatalf("new received %v, want 6..%d", got, head)
	}
	for _, d := range r.deliveries() {
		if d.Endpoint == "all" && len(d.Entries) > 3 || d.Replay {
			t.Fatalf("delivery %s has %d entries, replay %v", d.ID, len(d.Entries), d.Replay)
		}
		for _, e := range d.Entries {
			if len(e.Lines) != 2 || e.Kind != ledger.KindPosted || e.TransferID == "" {
				t.Fatalf("entry %+v", e)
			}
		}
	}
	if n := h.reg.Counter("ledgerd_webhook_entries_delivered_total", "", "").Value(); n != uint64(2*head-5) {
		t.Fatalf("entries delivered metric %d", n)
	}
}

func TestFilteredEndpointSkipsWhatItDoesNotWant(t *testing.T) {
	r := newReceiver(t)
	h := newHarness(t, t.TempDir(), nil)
	h.accounts()
	h.hook(ledger.WebhookSpec{ID: "alice", URL: r.srv.URL, Account: "alice", AfterSeq: new(uint64)})
	want := h.pay("alice", 3)
	// A batch of entries the filter never matches, more than skipPersist.
	batch := ledger.BatchSpec{}
	for i := 0; i < skipPersist+500; i++ {
		batch.Transfers = append(batch.Transfers, ledger.TransferSpec{Currency: "USD", Postings: []ledger.PostingSpec{
			{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "bob", Direction: ledger.Credit, Amount: 1}}})
	}
	h.cmd(ledger.Command{CreateBatch: &batch})
	want = append(want, h.pay("alice", 2)...)
	head := h.l.Head()
	eventually(t, "the filtered endpoint at the head", func() bool { return h.get("alice").Cursor == head })
	r.check()
	if got := r.seqs("alice"); !equal(got, want) {
		t.Fatalf("received %v, want %v", got, want)
	}
	// Nothing at the head matches: the position still follows the head,
	// logged once the scan is skipPersist ahead.
	h.cmd(ledger.Command{CreateBatch: &batch})
	head = h.l.Head()
	eventually(t, "the position logged past entries that do not match", func() bool { return head-h.get("alice").Cursor < skipPersist })
	if got := r.seqs("alice"); !equal(got, want) {
		t.Fatalf("received %v after unmatched entries, want %v", got, want)
	}
}

func TestRetriesWithBackoffUntilAccepted(t *testing.T) {
	r := newReceiver(t)
	r.setAnswer(func(d Delivery, attempt int) int {
		if attempt <= 3 {
			return []int{http.StatusInternalServerError, http.StatusFound, http.StatusTooManyRequests}[attempt-1]
		}
		return http.StatusNoContent
	})
	h := newHarness(t, "", func(o *Options) { o.MaxAttempts = 10 })
	h.accounts()
	h.hook(ledger.WebhookSpec{ID: "w", URL: r.srv.URL, AfterSeq: new(uint64)})
	start := time.Now()
	seqs := h.pay("alice", 1)
	eventually(t, "the delivery accepted", func() bool { return h.get("w").Cursor == seqs[0] })
	// Three failures wait at least 2.5 + 5 + 10 ms (half of 5, 10 and 20 ms).
	if d := time.Since(start); d < 17*time.Millisecond {
		t.Fatalf("three retries took only %s", d)
	}
	r.check()
	r.mu.Lock()
	attempts := r.attempts["w.1.1"]
	r.mu.Unlock()
	if attempts != 4 || len(r.deliveries()) != 1 || len(h.get("w").DeadLetters) != 0 {
		t.Fatalf("%d attempts, %d accepted", attempts, len(r.deliveries()))
	}
	if n := h.reg.Counter("ledgerd_webhook_attempts_total", `result="failed"`, "").Value(); n != 3 {
		t.Fatalf("failed attempts metric %d", n)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	d := &Dispatcher{o: Options{BackoffBase: time.Second, BackoffMax: time.Minute}}
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 6: 32 * time.Second, 7: time.Minute, 70: time.Minute, 1 << 30: time.Minute} {
		for i := 0; i < 100; i++ {
			if got := d.backoff(attempt); got < want/2 || got > want {
				t.Fatalf("attempt %d: %s, want %s to %s", attempt, got, want/2, want)
			}
		}
	}
}

func TestTimeoutsAndUnreachableEndpointsAreFailures(t *testing.T) {
	r := newReceiver(t)
	release := make(chan struct{})
	var once sync.Once
	r.setAnswer(func(d Delivery, attempt int) int {
		if attempt == 1 {
			// Answers long after the dispatcher stopped waiting.
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	h := newHarness(t, "", func(o *Options) { o.Timeout = 50 * time.Millisecond; o.MaxAttempts = 3 })
	h.accounts()
	h.hook(ledger.WebhookSpec{ID: "slow", URL: r.srv.URL, AfterSeq: new(uint64)})
	h.hook(ledger.WebhookSpec{ID: "gone", URL: deadURL, AfterSeq: new(uint64)})
	h.pay("alice", 1)
	eventually(t, "the slow endpoint after a timed-out attempt", func() bool { return h.get("slow").Cursor == 1 })
	once.Do(func() { close(release) })
	eventually(t, "a dead letter for the unreachable endpoint", func() bool { return len(h.get("gone").DeadLetters) == 1 })
	dl := h.get("gone").DeadLetters[0]
	if dl.Attempts != 3 || !strings.Contains(dl.Error, "connect") || dl.AfterSeq != 0 || dl.ThroughSeq != 1 || dl.Entries != 1 || h.get("gone").Cursor != 1 {
		t.Fatalf("dead letter %+v", dl)
	}
	if len(r.deliveries()) != 1 {
		t.Fatalf("%d deliveries accepted", len(r.deliveries()))
	}
}

func TestDeadLettersAndReplay(t *testing.T) {
	r := newReceiver(t)
	var down sync.Mutex
	broken := map[uint64]bool{}
	r.setAnswer(func(d Delivery, attempt int) int {
		down.Lock()
		defer down.Unlock()
		for _, e := range d.Entries {
			if broken[e.Seq] {
				return http.StatusServiceUnavailable
			}
		}
		return http.StatusOK
	})
	setBroken := func(seqs ...uint64) {
		down.Lock()
		clear(broken)
		for _, s := range seqs {
			broken[s] = true
		}
		down.Unlock()
	}
	h := newHarness(t, t.TempDir(), func(o *Options) { o.MaxAttempts = 2 })
	h.accounts()
	h.hook(ledger.WebhookSpec{ID: "w", URL: r.srv.URL, AfterSeq: new(uint64), MaxBatch: 2})
	setBroken(3)
	h.cmd(ledger.Command{CreateBatch: &ledger.BatchSpec{Transfers: []ledger.TransferSpec{
		{Currency: "USD", Postings: []ledger.PostingSpec{{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "alice", Direction: ledger.Credit, Amount: 1}}},
		{Currency: "USD", Postings: []ledger.PostingSpec{{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "alice", Direction: ledger.Credit, Amount: 1}}},
		{Currency: "USD", Postings: []ledger.PostingSpec{{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "alice", Direction: ledger.Credit, Amount: 1}}},
		{Currency: "USD", Postings: []ledger.PostingSpec{{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "alice", Direction: ledger.Credit, Amount: 1}}},
		{Currency: "USD", Postings: []ledger.PostingSpec{{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "alice", Direction: ledger.Credit, Amount: 1}}},
	}}})
	// Deliveries are 1-2, 3-4 (fails twice: a dead letter) and 5.
	eventually(t, "the endpoint past its dead letter", func() bool { return h.get("w").Cursor == 5 })
	w := h.get("w")
	if len(w.DeadLetters) != 1 {
		t.Fatalf("dead letters %+v", w.DeadLetters)
	}
	if dl := w.DeadLetters[0]; dl.ID != 1 || dl.AfterSeq != 2 || dl.ThroughSeq != 4 || dl.Entries != 2 || dl.Attempts != 2 ||
		dl.Error != "the endpoint answered 503" || dl.Replaying {
		t.Fatalf("dead letter %+v", dl)
	}
	if got := r.seqs("w"); !equal(got, []uint64{1, 2, 5}) {
		t.Fatalf("received %v", got)
	}
	// The dead letter survives a restart, and a replay that fails again
	// adds its attempts and stops.
	h.restart()
	h.cmd(ledger.Command{ReplayWebhook: &ledger.WebhookReplaySpec{ID: "w", AllDeadLetters: true}})
	eventually(t, "the failed replay recorded", func() bool { d := h.get("w").DeadLetters; return len(d) == 1 && !d[0].Replaying })
	if dl := h.get("w").DeadLetters[0]; dl.Attempts != 4 {
		t.Fatalf("after a failed replay %+v", dl)
	}
	// Fixed, the replay is accepted and the dead letter removed, flagged as
	// a replay and with the original delivery's ID.
	setBroken()
	h.cmd(ledger.Command{ReplayWebhook: &ledger.WebhookReplaySpec{ID: "w", DeadLetters: []uint64{1}}})
	eventually(t, "the dead letter redelivered", func() bool { return len(h.get("w").DeadLetters) == 0 })
	ds := r.deliveries()
	if last := ds[len(ds)-1]; !last.Replay || last.ID != "w.3.4" || len(last.Entries) != 2 {
		t.Fatalf("replayed delivery %+v", last)
	}
	// Everything after a position, again.
	h.cmd(ledger.Command{ReplayWebhook: &ledger.WebhookReplaySpec{ID: "w", AfterSeq: new(uint64)}})
	eventually(t, "the replayed journal", func() bool { return len(r.seqs("w")) == 3+2+5 })
	if got := r.seqs("w"); !equal(got, []uint64{1, 2, 5, 3, 4, 1, 2, 3, 4, 5}) {
		t.Fatalf("received %v", got)
	}
	r.check()
	if n := h.reg.Counter("ledgerd_webhook_redeliveries_total", "", "").Value(); n != 1 {
		t.Fatalf("redeliveries metric %d", n)
	}
}

// A replay made while a delivery is in flight wins: the delivery's progress
// is refused and the endpoint carries on from the replayed position.
func TestReplayDuringADeliveryWins(t *testing.T) {
	r := newReceiver(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r.setAnswer(func(d Delivery, attempt int) int {
		if d.Entries[0].Seq == 4 && attempt == 1 {
			close(entered)
			<-release
		}
		return http.StatusOK
	})
	h := newHarness(t, "", nil)
	h.accounts()
	h.hook(ledger.WebhookSpec{ID: "w", URL: r.srv.URL, AfterSeq: new(uint64), MaxBatch: 3})
	h.cmd(ledger.Command{CreateBatch: &ledger.BatchSpec{Transfers: []ledger.TransferSpec{
		{Currency: "USD", Postings: []ledger.PostingSpec{{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "alice", Direction: ledger.Credit, Amount: 1}}},
		{Currency: "USD", Postings: []ledger.PostingSpec{{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "alice", Direction: ledger.Credit, Amount: 1}}},
		{Currency: "USD", Postings: []ledger.PostingSpec{{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "alice", Direction: ledger.Credit, Amount: 1}}},
		{Currency: "USD", Postings: []ledger.PostingSpec{{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "alice", Direction: ledger.Credit, Amount: 1}}},
	}}})
	<-entered
	h.cmd(ledger.Command{ReplayWebhook: &ledger.WebhookReplaySpec{ID: "w", AfterSeq: new(uint64)}})
	close(release)
	eventually(t, "the replay delivered", func() bool { return len(r.seqs("w")) == 4+4 })
	if got := r.seqs("w"); !equal(got, []uint64{1, 2, 3, 4, 1, 2, 3, 4}) {
		t.Fatalf("received %v", got)
	}
	eventually(t, "the position at the head", func() bool { return h.get("w").Cursor == 4 })
}

// Every entry acknowledged by the ledger reaches the endpoint across
// restarts while the endpoint is down, and what was delivered before a
// restart is not delivered again.
func TestDeliverySurvivesRestarts(t *testing.T) {
	r := newReceiver(t)
	var mu sync.Mutex
	up := true
	r.setAnswer(func(Delivery, int) int {
		mu.Lock()
		defer mu.Unlock()
		if up {
			return http.StatusOK
		}
		return http.StatusBadGateway
	})
	setUp := func(v bool) { mu.Lock(); up = v; mu.Unlock() }
	h := newHarness(t, t.TempDir(), func(o *Options) { o.MaxAttempts = 1000 })
	h.opts.SnapshotEvery = 7 // snapshots carry the positions too
	h.restart()
	h.accounts()
	h.hook(ledger.WebhookSpec{ID: "w", URL: r.srv.URL, AfterSeq: new(uint64), MaxBatch: 4})
	h.pay("alice", 10)
	eventually(t, "the first entries", func() bool { return h.get("w").Cursor == 10 })
	setUp(false)
	h.pay("alice", 10)
	h.restart()
	if c := h.get("w").Cursor; c != 10 {
		t.Fatalf("position %d after a restart with the endpoint down", c)
	}
	h.pay("alice", 5)
	h.restart()
	setUp(true)
	eventually(t, "every entry", func() bool { return h.get("w").Cursor == 25 })
	h.restart()
	h.pay("alice", 1)
	eventually(t, "the entry after the last restart", func() bool { return h.get("w").Cursor == 26 })
	if got := r.seqs("w"); !equal(got, span(1, 26)) {
		t.Fatalf("received %v", got)
	}
	r.check()
}

func TestDeleteStopsDeliveryAndRecreateStartsOver(t *testing.T) {
	r := newReceiver(t)
	h := newHarness(t, "", nil)
	h.accounts()
	h.hook(ledger.WebhookSpec{ID: "w", URL: r.srv.URL + "/one", AfterSeq: new(uint64)})
	h.pay("alice", 2)
	eventually(t, "delivery", func() bool { return h.get("w").Cursor == 2 })
	h.cmd(ledger.Command{DeleteWebhook: &ledger.WebhookRef{ID: "w"}})
	h.pay("alice", 2)
	eventually(t, "the worker gone", func() bool {
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		return len(h.d.workers) == 0
	})
	time.Sleep(20 * time.Millisecond)
	if n := len(r.seqs("w")); n != 2 {
		t.Fatalf("%d entries delivered after the endpoint was deleted", n)
	}
	h.hook(ledger.WebhookSpec{ID: "w", URL: r.srv.URL + "/two", AfterSeq: new(uint64)})
	eventually(t, "the new endpoint from the start", func() bool { return h.get("w").Cursor == 4 })
	if got := r.seqs("w"); !equal(got, []uint64{1, 2, 1, 2, 3, 4}) {
		t.Fatalf("received %v", got)
	}
}

// With MaxDeadLetters dead letters an endpoint takes no more: its worker
// retries at the longest delay and moves on once the receiver recovers.
func TestFullDeadLetterListHoldsTheEntries(t *testing.T) {
	if testing.Short() {
		t.Skip("delivers a thousand dead letters")
	}
	r := newReceiver(t)
	var mu sync.Mutex
	up := false
	r.setAnswer(func(Delivery, int) int {
		mu.Lock()
		defer mu.Unlock()
		if up {
			return http.StatusOK
		}
		return http.StatusInternalServerError
	})
	h := newHarness(t, "", func(o *Options) { o.MaxAttempts = 1; o.BackoffMax = 30 * time.Millisecond })
	h.accounts()
	batch := ledger.BatchSpec{}
	for i := 0; i < ledger.MaxDeadLetters+3; i++ {
		batch.Transfers = append(batch.Transfers, ledger.TransferSpec{Currency: "USD", Postings: []ledger.PostingSpec{
			{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "alice", Direction: ledger.Credit, Amount: 1}}})
	}
	h.cmd(ledger.Command{CreateBatch: &batch})
	h.hook(ledger.WebhookSpec{ID: "w", URL: r.srv.URL, AfterSeq: new(uint64), MaxBatch: 1})
	eventually(t, "a full dead letter list", func() bool { return len(h.get("w").DeadLetters) == ledger.MaxDeadLetters })
	time.Sleep(100 * time.Millisecond)
	if w := h.get("w"); w.Cursor != ledger.MaxDeadLetters || len(w.DeadLetters) != ledger.MaxDeadLetters {
		t.Fatalf("position %d with %d dead letters", w.Cursor, len(w.DeadLetters))
	}
	mu.Lock()
	up = true
	mu.Unlock()
	eventually(t, "the held entries delivered", func() bool { return h.get("w").Cursor == ledger.MaxDeadLetters+3 })
	if got := r.seqs("w"); !equal(got, span(ledger.MaxDeadLetters+1, ledger.MaxDeadLetters+3)) {
		t.Fatalf("received %v", got)
	}
}

func TestCloseAbandonsADeliveryInFlight(t *testing.T) {
	r := newReceiver(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.setAnswer(func(Delivery, int) int {
		once.Do(func() { close(entered) })
		<-release
		return http.StatusOK
	})
	t.Cleanup(func() { close(release) })
	h := newHarness(t, "", func(o *Options) { o.Timeout = 30 * time.Second })
	h.accounts()
	h.hook(ledger.WebhookSpec{ID: "w", URL: r.srv.URL, AfterSeq: new(uint64)})
	h.pay("alice", 1)
	<-entered
	start := time.Now()
	h.d.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %s", d)
	}
	if c := h.get("w").Cursor; c != 0 {
		t.Fatalf("an abandoned delivery moved the position to %d", c)
	}
}

func TestStartNeedsALedger(t *testing.T) {
	if _, err := Start(Options{}); err == nil {
		t.Fatal("started without a ledger")
	}
}

func TestErrText(t *testing.T) {
	long := strings.Repeat("é", ledger.MaxWebhookError)
	if got := errText(fmt.Errorf("a\nb\x00%s", long)); len(got) > ledger.MaxWebhookError || strings.ContainsAny(got, "\n\x00") || !strings.HasPrefix(got, "a b ") {
		t.Fatalf("%q", got)
	}
}

// failWrites fails every write and fsync while on is set, so nothing
// written meanwhile reaches the file.
type failWrites struct {
	wal.File
	on *atomic.Bool
}

func (f failWrites) Write(p []byte) (int, error) {
	if f.on.Load() {
		return 0, errors.New("input/output error")
	}
	return f.File.Write(p)
}

func (f failWrites) Sync() error {
	if f.on.Load() {
		return errors.New("input/output error")
	}
	return f.File.Sync()
}

// When the receiver accepted a delivery but the ledger could not log the
// new position, the entries are sent again after a restart: at least once,
// never lost.
func TestDeliveryIsRepeatedWhenProgressIsNotLogged(t *testing.T) {
	r := newReceiver(t)
	var failing atomic.Bool
	r.setAnswer(func(d Delivery, attempt int) int {
		failing.Store(true) // the disk fails before the position is logged
		return http.StatusOK
	})
	dir := t.TempDir()
	h := newHarness(t, dir, nil)
	h.d.Close()
	h.l.Close()
	h.opts.WrapFile = func(f wal.File) wal.File { return failWrites{File: f, on: &failing} }
	h.open()
	h.accounts()
	h.pay("alice", 3) // before the endpoint exists, so no write can fail
	h.hook(ledger.WebhookSpec{ID: "w", URL: r.srv.URL, AfterSeq: new(uint64)})
	eventually(t, "the ledger to stop taking writes", func() bool { return h.l.Err() != nil })
	// However long the ledger stays stopped, the worker does not send the
	// entries again: it could not log their position either. Many times
	// the backoff passes.
	time.Sleep(40 * h.do.BackoffBase)
	if got := r.seqs("w"); !equal(got, []uint64{1, 2, 3}) {
		t.Fatalf("received %v while the ledger was stopped", got)
	}
	r.setAnswer(nil)
	failing.Store(false)
	h.restart()
	eventually(t, "the entries delivered again", func() bool { return h.get("w").Cursor == 3 })
	if got := r.seqs("w"); !equal(got, []uint64{1, 2, 3, 1, 2, 3}) {
		t.Fatalf("received %v", got)
	}
}
