package webhook

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
	"github.com/laiwf94/ledgerd/internal/metrics"
)

func (h *harness) monitor(id, acct string, op ledger.MonitorOperator, v int64) {
	h.t.Helper()
	h.cmd(ledger.Command{CreateMonitor: &ledger.MonitorSpec{ID: id, Account: acct,
		Condition: ledger.Condition{Field: ledger.FieldPosted, Operator: op, Value: v}}})
}

// flip moves one unit into alice and out to bob, so a monitor on alice
// posted >= 1 triggers and clears: two entries, two alerts.
func (h *harness) flip() {
	h.t.Helper()
	h.pay("alice", 1)
	h.cmd(ledger.Command{CreateTransfer: &ledger.TransferSpec{Currency: "USD", Postings: []ledger.PostingSpec{
		{Account: "alice", Direction: ledger.Debit, Amount: 1}, {Account: "bob", Direction: ledger.Credit, Amount: 1}}}})
}

// alerts returns the alert sequences an endpoint accepted, in arrival
// order, and checks each against the ledger's alert feed.
func (r *receiver) alerts(l *ledger.Ledger, endpoint string, replays bool) []uint64 {
	r.t.Helper()
	page, err := l.Alerts(ledger.AlertFilter{}, 0, ledger.MaxListLimit)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []uint64
	for _, d := range r.deliveries() {
		if d.Endpoint != endpoint || d.Replay != replays {
			continue
		}
		for _, e := range d.Entries {
			for _, a := range e.Alerts {
				want := page.Alerts[a.Seq-1]
				if a.EntrySeq != e.Seq || a.Monitor != want.Monitor || a.Event != want.Event || a.EntrySeq != want.EntrySeq ||
					a.Balance != want.Balance || !a.Time.Equal(want.Time) {
					r.t.Fatalf("%s: alert %+v on entry %d, the ledger has %+v", endpoint, a, e.Seq, want)
				}
				out = append(out, a.Seq)
			}
		}
	}
	return out
}

// Endpoints that opt in to alerts get each alert with the entry that raised
// it; positions, dead letters, replays and restarts cover the alerts as they
// cover entries; an endpoint that did not opt in gets none.
func TestAlertsRideWithTheirEntries(t *testing.T) {
	r := newReceiver(t)
	var mu sync.Mutex
	failAlert := uint64(3)
	r.setAnswer(func(d Delivery, attempt int) int {
		mu.Lock()
		defer mu.Unlock()
		if d.Endpoint == "alerts" && !d.Replay {
			for _, e := range d.Entries {
				for _, a := range e.Alerts {
					if a.Seq == failAlert {
						return http.StatusServiceUnavailable
					}
				}
			}
		}
		return http.StatusOK
	})
	h := newHarness(t, t.TempDir(), func(o *Options) { o.MaxAttempts = 2 })
	h.opts.SnapshotEvery = 5 // snapshots carry the subscriptions too
	h.restart()
	h.accounts()
	h.monitor("alice-up", "alice", ledger.OpGreaterOrEqual, 1)
	h.monitor("bob-big", "bob", ledger.OpGreaterOrEqual, 1000) // never crosses
	h.hook(ledger.WebhookSpec{ID: "bob", URL: r.srv.URL, AfterSeq: new(uint64), Account: "bob", Alerts: ledger.AlertsInclude,
		AlertMonitor: "alice-up"})
	h.hook(ledger.WebhookSpec{ID: "plain", URL: r.srv.URL, AfterSeq: new(uint64)})
	for i := 0; i < 4; i++ {
		h.flip()
	}
	h.pay("bob", 3) // entries with no alert
	// Registered once the entries exist, so its deliveries are 1-2, 3-4
	// and so on, not whatever the timing of the commits made them.
	h.hook(ledger.WebhookSpec{ID: "alerts", URL: r.srv.URL, AfterSeq: new(uint64), MaxBatch: 2, Alerts: ledger.AlertsOnly})
	head := h.l.Head()
	eventually(t, "every endpoint at the head", func() bool {
		return h.get("alerts").Cursor == head && h.get("bob").Cursor == head && h.get("plain").Cursor == head
	})
	// Alert 3 rode in the delivery that failed: a dead letter, holding the
	// entries that raised alerts 3 and 4.
	w := h.get("alerts")
	if len(w.DeadLetters) != 1 || w.DeadLetters[0].Entries != 2 {
		t.Fatalf("dead letters %+v", w.DeadLetters)
	}
	if got := r.alerts(h.l, "alerts", false); !equal(got, []uint64{1, 2, 5, 6, 7, 8}) {
		t.Fatalf("alerts received %v", got)
	}
	// Restart, more crossings, then replay the dead letter.
	h.restart()
	h.flip()
	head = h.l.Head()
	// Wait for the new alerts first: a replay that lands while a delivery
	// is in flight makes that delivery's progress refused, so it is sent
	// again, which is at least once but not what this checks.
	eventually(t, "the new alerts", func() bool { return h.get("alerts").Cursor == head })
	mu.Lock()
	failAlert = 0
	mu.Unlock()
	h.cmd(ledger.Command{ReplayWebhook: &ledger.WebhookReplaySpec{ID: "alerts", AllDeadLetters: true}})
	eventually(t, "the replay and the new alerts", func() bool {
		w := h.get("alerts")
		return w.Cursor == head && len(w.DeadLetters) == 0 && h.get("bob").Cursor == head
	})
	if got := r.alerts(h.l, "alerts", false); !equal(got, []uint64{1, 2, 5, 6, 7, 8, 9, 10}) {
		t.Fatalf("alerts received %v", got)
	}
	if got := r.alerts(h.l, "alerts", true); !equal(got, []uint64{3, 4}) {
		t.Fatalf("alerts replayed %v", got)
	}
	// Only the entries that raised alerts reached "alerts".
	for _, d := range r.deliveries() {
		for _, e := range d.Entries {
			if d.Endpoint == "alerts" && len(e.Alerts) == 0 || d.Endpoint == "plain" && e.Alerts != nil {
				t.Fatalf("%s got entry %d with alerts %+v", d.Endpoint, e.Seq, e.Alerts)
			}
		}
	}
	// "bob" got every entry on bob (the alerts clearing are on entries
	// moving alice to bob) and, with them, alice-up's clears; the triggers
	// ride on entries paying alice, which it also gets for their alerts.
	if got := r.alerts(h.l, "bob", false); !equal(got, span(1, 10)) {
		t.Fatalf("bob's alerts %v", got)
	}
	if got := r.seqs("bob"); len(got) != 10+3 {
		t.Fatalf("bob's entries %v", got)
	}
	// After a replay of everything, the alerts arrive again in order.
	h.cmd(ledger.Command{ReplayWebhook: &ledger.WebhookReplaySpec{ID: "alerts", AfterSeq: new(uint64)}})
	eventually(t, "the replayed journal", func() bool { return len(r.alerts(h.l, "alerts", false)) == 8+10 })
	if got := r.alerts(h.l, "alerts", false)[8:]; !equal(got, span(1, 10)) {
		t.Fatalf("after replay %v", got)
	}
	r.check()
	if n := h.reg.Counter("ledgerd_webhook_alerts_delivered_total", "", "").Value(); n == 0 {
		t.Fatal("no alerts counted")
	}
}

// BenchmarkAlertFanout measures the delivery of balance alerts to 1,000
// webhook endpoints subscribed to one account's alerts: each op commits one
// entry that crosses a monitor and waits until every endpoint has accepted
// the alert over HTTP and logged its position. Receiver, dispatcher and
// ledger share the machine, so this is the whole cost of a delivery:
// reading the feed, encoding, signing, the HTTP round trip and the logged
// position.
func BenchmarkAlertFanout(b *testing.B) {
	for _, n := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("subscribers=%d", n), func(b *testing.B) {
			var accepted atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				accepted.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			b.Cleanup(srv.Close)
			l, err := ledger.Open(ledger.Options{SweepInterval: -1})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { l.Close() })
			client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: n + 16, MaxConnsPerHost: 0}}
			d, err := Start(Options{Ledger: l, Client: client, Metrics: metrics.New()})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(d.Close)
			h := &harness{t: b, l: l}
			h.accounts()
			h.monitor("alice-up", "alice", ledger.OpGreaterOrEqual, 1)
			for i := 0; i < n; i++ {
				h.hook(ledger.WebhookSpec{ID: fmt.Sprintf("s%04d", i), URL: srv.URL, Alerts: ledger.AlertsOnly, AlertAccount: "alice"})
			}
			var before runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			ctx := context.Background()
			out := ledger.Command{CreateTransfer: &ledger.TransferSpec{Currency: "USD", Postings: []ledger.PostingSpec{
				{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "alice", Direction: ledger.Credit, Amount: 1}}}}
			back := ledger.Command{CreateTransfer: &ledger.TransferSpec{Currency: "USD", Postings: []ledger.PostingSpec{
				{Account: "alice", Direction: ledger.Debit, Amount: 1}, {Account: "bank", Direction: ledger.Credit, Amount: 1}}}}
			var lat, commit []time.Duration
			b.ResetTimer()
			start := time.Now()
			for i := 0; i < b.N; i++ {
				c := out
				if i%2 == 1 {
					c = back
				}
				t0 := time.Now()
				if _, err := l.Do(ctx, c, nil); err != nil {
					b.Fatal(err)
				}
				commit = append(commit, time.Since(t0))
				want := int64(i+1) * int64(n)
				for accepted.Load() < want {
					time.Sleep(20 * time.Microsecond)
				}
				lat = append(lat, time.Since(t0))
			}
			elapsed := time.Since(start)
			b.StopTimer()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			b.ReportMetric(float64(n*b.N)/elapsed.Seconds(), "deliveries/s")
			b.ReportMetric(float64(elapsed.Nanoseconds())/float64(n*b.N), "ns/delivery")
			b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc)/float64(n*b.N), "B/delivery")
			b.ReportMetric(float64(after.Mallocs-before.Mallocs)/float64(n*b.N), "allocs/delivery")
			slices.Sort(lat)
			b.ReportMetric(float64(lat[len(lat)/2].Microseconds()), "µs-p50-all-delivered")
			b.ReportMetric(float64(lat[len(lat)*99/100].Microseconds()), "µs-p99-all-delivered")
			// The crossing's own commit, which shares the writer with the
			// progress commands of the deliveries before it.
			slices.Sort(commit)
			b.ReportMetric(float64(commit[len(commit)/2].Microseconds()), "µs-p50-commit")
		})
	}
}
