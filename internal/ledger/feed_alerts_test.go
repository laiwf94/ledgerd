package ledger

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"
)

func withAlertYield(t testing.TB, n int) {
	old := alertYield
	alertYield = n
	t.Cleanup(func() { alertYield = old })
}

// walkFeedItems follows the feed from after to the head with pages of limit,
// checks the paging rules on every page and returns the entries with their
// alerts.
func walkFeedItems(t testing.TB, l *Ledger, f EntryFilter, after uint64, limit int) []FeedEntry {
	t.Helper()
	var out []FeedEntry
	for pages := 0; ; pages++ {
		if pages > maxPages {
			t.Fatalf("feed %+v never ended", f)
		}
		p, err := l.Entries(f, after, limit)
		if err != nil {
			t.Fatalf("feed %+v after %d: %v", f, after, err)
		}
		if p.Next < after || p.Next > p.Head || len(p.Entries) > limit {
			t.Fatalf("feed %+v after %d: next %d, head %d, %d entries", f, after, p.Next, p.Head, len(p.Entries))
		}
		if (f.Alerts == AlertsOff) != (p.Alerts == nil) || p.Alerts != nil && len(p.Alerts) != len(p.Entries) {
			t.Fatalf("feed %+v: %d entries, %d alert lists (nil %v)", f, len(p.Entries), len(p.Alerts), p.Alerts == nil)
		}
		for i, en := range p.Entries {
			if en.Seq <= after || en.Seq > p.Next {
				t.Fatalf("feed %+v after %d: entry %d on a page that ends at %d", f, after, en.Seq, p.Next)
			}
			out = append(out, p.Item(i))
		}
		if !p.HasMore() {
			return out
		}
		after = p.Next
	}
}

// allAlerts reads the whole alert feed.
func (e *env) allAlerts() []BalanceAlert {
	e.t.Helper()
	var out []BalanceAlert
	var after uint64
	for {
		p, err := e.l.Alerts(AlertFilter{}, after, MaxListLimit)
		if err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p.Alerts...)
		if !p.HasMore() {
			return out
		}
		after = p.Next
	}
}

// scanJournalAlerts is the feed's answer with alerts, worked out by brute
// force from the whole journal and the whole alert feed.
func scanJournalAlerts(e *env, f EntryFilter, after uint64, all []BalanceAlert) []FeedEntry {
	base := EntryFilter{Account: f.Account, Kinds: f.Kinds, Currency: f.Currency}
	matched := map[uint64]bool{}
	for _, en := range scanJournal(e, base, after) {
		matched[en.Seq] = true
	}
	var out []FeedEntry
	for seq := after + 1; seq <= e.head(); seq++ {
		var raised []BalanceAlert
		for _, a := range all {
			if a.EntrySeq == seq && (f.AlertMonitor == "" || a.Monitor == f.AlertMonitor) && (f.AlertAccount == "" || a.Account == f.AlertAccount) {
				raised = append(raised, a)
			}
		}
		var ok bool
		switch f.Alerts {
		case AlertsOff:
			ok, raised = matched[seq], nil
		case AlertsInclude:
			ok = matched[seq] || len(raised) > 0
		case AlertsOnly:
			ok = matched[seq] && len(raised) > 0
		}
		if ok {
			out = append(out, FeedEntry{Entry: e.entry(seq), Alerts: raised})
		}
	}
	return out
}

func sameItems(t *testing.T, what string, got, want []FeedEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d entries %v, want %d %v", what, len(got), itemSeqs(got), len(want), itemSeqs(want))
	}
	for i := range got {
		if !reflect.DeepEqual(got[i].Entry, want[i].Entry) {
			t.Fatalf("%s: entry %d is %+v, want %+v", what, i, got[i].Entry, want[i].Entry)
		}
		if len(got[i].Alerts) != len(want[i].Alerts) || len(got[i].Alerts) > 0 && !reflect.DeepEqual(got[i].Alerts, want[i].Alerts) {
			t.Fatalf("%s: entry %d carries alerts %+v, want %+v", what, got[i].Seq, got[i].Alerts, want[i].Alerts)
		}
	}
}

func itemSeqs(items []FeedEntry) []uint64 {
	out := make([]uint64, len(items))
	for i := range items {
		out[i] = items[i].Seq
	}
	return out
}

func alertSeqsOf(items []FeedEntry) []uint64 {
	var out []uint64
	for _, it := range items {
		for _, a := range it.Alerts {
			out = append(out, a.Seq)
		}
	}
	return out
}

func TestEntriesCarryAlerts(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(monCmd("alice-low", "alice", FieldAvailable, OpLessThan, 100)) // holds at once: alice is empty
	e.do(monCmd("bob-high", "bob", FieldPosted, OpGreaterOrEqual, 500))
	e.do(move(dr("bank", 1000), cr("alice", 1000)))             // 1: alice-low clears
	e.do(move(dr("alice", 950), cr("bob", 950)))                // 2: alice-low triggers, bob-high triggers
	e.do(move(dr("bank", 10), cr("merchant", 10)))              // 3: no alert
	e.do(move(dr("bob", 900), cr("alice", 900)))                // 4: bob-high clears, alice-low clears
	h := e.do(hold(600, dr("alice", 880), cr("merchant", 880))) // 5: alice-low triggers on available
	e.do(voidCmd(h.Transfer.ID))                                // 6: alice-low clears
	if e.head() != 6 || e.l.AlertHead() != 7 {
		t.Fatalf("head %d, %d alerts", e.head(), e.l.AlertHead())
	}

	// Without alerts the feed is unchanged and carries none.
	p, err := e.l.Entries(EntryFilter{}, 0, 100)
	if err != nil || len(p.Entries) != 6 || p.Alerts != nil || p.AlertCount() != 0 {
		t.Fatalf("plain feed %+v %v", p, err)
	}
	if fe := p.Item(2); fe.Alerts != nil {
		t.Fatalf("plain item %+v", fe)
	}

	items := walkFeedItems(t, e.l, EntryFilter{Alerts: AlertsInclude}, 0, 100)
	if got := itemSeqs(items); !slices.Equal(got, []uint64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("include: %v", got)
	}
	if got := alertSeqsOf(items); !slices.Equal(got, []uint64{1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("include alerts %v", got)
	}
	if a := items[1].Alerts; len(a) != 2 || a[0].Monitor != "alice-low" || a[0].Event != AlertTriggered || a[1].Monitor != "bob-high" ||
		a[1].Balance.Posted != 950 || a[1].EntrySeq != 2 || a[1].TransferID != items[1].TransferID {
		t.Fatalf("entry 2 alerts %+v", a)
	}
	if len(items[2].Alerts) != 0 {
		t.Fatalf("entry 3 alerts %+v", items[2].Alerts)
	}

	// Only entries that raised an alert, then only bob's.
	only := walkFeedItems(t, e.l, EntryFilter{Alerts: AlertsOnly}, 0, 100)
	if got := itemSeqs(only); !slices.Equal(got, []uint64{1, 2, 4, 5, 6}) {
		t.Fatalf("only: %v", got)
	}
	bob := walkFeedItems(t, e.l, EntryFilter{Alerts: AlertsOnly, AlertAccount: "bob"}, 0, 100)
	if got, a := itemSeqs(bob), alertSeqsOf(bob); !slices.Equal(got, []uint64{2, 4}) || !slices.Equal(a, []uint64{3, 4}) {
		t.Fatalf("bob only: %v alerts %v", got, a)
	}
	mon := walkFeedItems(t, e.l, EntryFilter{Alerts: AlertsOnly, AlertMonitor: "alice-low", Kinds: []EntryKind{KindPending, KindVoidPending}}, 0, 100)
	if got, a := itemSeqs(mon), alertSeqsOf(mon); !slices.Equal(got, []uint64{5, 6}) || !slices.Equal(a, []uint64{6, 7}) {
		t.Fatalf("alice-low holds: %v alerts %v", got, a)
	}

	// Include adds the entries that raised a wanted alert to the ones the
	// entry filter matches: merchant's entries, plus bob's crossings.
	inc := walkFeedItems(t, e.l, EntryFilter{Account: "merchant", Alerts: AlertsInclude, AlertMonitor: "bob-high"}, 0, 100)
	if got, a := itemSeqs(inc), alertSeqsOf(inc); !slices.Equal(got, []uint64{2, 3, 4, 5, 6}) || !slices.Equal(a, []uint64{3, 4}) {
		t.Fatalf("merchant + bob-high: %v alerts %v", got, a)
	}
	// An account that does not exist matches no entry, but its alerts can
	// still be included.
	ghost := walkFeedItems(t, e.l, EntryFilter{Account: "ghost", Alerts: AlertsInclude, AlertAccount: "bob"}, 0, 100)
	if got := itemSeqs(ghost); !slices.Equal(got, []uint64{2, 4}) {
		t.Fatalf("ghost + bob: %v", got)
	}
	if p, err := e.l.Entries(EntryFilter{Account: "ghost", Alerts: AlertsOnly}, 0, 10); err != nil || len(p.Entries) != 0 || p.Next != 6 {
		t.Fatalf("ghost only: %+v %v", p, err)
	}

	// Resuming from every position gives the rest exactly once.
	for after := uint64(0); after <= 6; after++ {
		got := walkFeedItems(t, e.l, EntryFilter{Alerts: AlertsOnly}, after, 1)
		want := scanJournalAlerts(e, EntryFilter{Alerts: AlertsOnly}, after, e.allAlerts())
		sameItems(t, fmt.Sprintf("only after %d", after), got, want)
	}

	// The scan budget and the alert walk's yield bound a page; Next says how
	// far it got.
	withFeedBudget(t, 1)
	withAlertYield(t, 1)
	p, _ = e.l.Entries(EntryFilter{Alerts: AlertsOnly, AlertMonitor: "nobody"}, 0, 10)
	// The walk hands over entry 2 after missing alert 1 at entry 1 and
	// alert 2 at entry 2; the budget of one stops the page there.
	if len(p.Entries) != 0 || p.Next != 2 || !p.HasMore() {
		t.Fatalf("budgeted page %+v", p)
	}
	if got := walkFeedItems(t, e.l, EntryFilter{Alerts: AlertsOnly, AlertMonitor: "nobody"}, 0, 10); len(got) != 0 {
		t.Fatal(got)
	}

	for _, bad := range []EntryFilter{
		{Alerts: "some"},
		{AlertMonitor: "alice-low"},
		{AlertAccount: "alice"},
		{Alerts: AlertsOnly, AlertMonitor: "a b"},
		{Alerts: AlertsInclude, AlertAccount: "a b"},
	} {
		if _, err := e.l.Entries(bad, 0, 10); codeOf(err) != CodeInvalid {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
}

// The feed with alerts, against brute force, over a random workload with
// monitors created and deleted along the way, for every mode and a spread of
// filters, page sizes, scan budgets and alert-walk yields.
func TestEntriesWithAlertsMatchBruteForce(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			e := newEnv(t, false)
			accts := []string{"a", "b", "c", "d"}
			e.do(openAcct("src", "USD", Debit))
			for _, a := range accts {
				e.do(openAcct(a, "USD", Credit))
			}
			monitors := map[string]bool{}
			var holds []string
			for i := 0; i < 400; i++ {
				switch k := rng.Intn(10); {
				case k == 0:
					id := fmt.Sprintf("m%d", rng.Intn(12))
					if monitors[id] {
						e.do(delMonCmd(id))
						delete(monitors, id)
						continue
					}
					fields := []BalanceField{FieldPosted, FieldPending, FieldAvailable}
					ops := []MonitorOperator{OpLessThan, OpGreaterOrEqual, OpEquals, OpNotEquals}
					e.do(monCmd(id, accts[rng.Intn(len(accts))], fields[rng.Intn(3)], ops[rng.Intn(4)], int64(rng.Intn(4)*50)))
					monitors[id] = true
				case k < 4 && len(holds) > 0:
					j := rng.Intn(len(holds))
					if rng.Intn(2) == 0 {
						e.do(voidCmd(holds[j]))
					} else {
						e.do(postCmd(holds[j], 0))
					}
					holds = slices.Delete(holds, j, j+1)
				default:
					from, to := accts[rng.Intn(len(accts))], accts[rng.Intn(len(accts))]
					if from == to {
						continue
					}
					amt := int64(1 + rng.Intn(120))
					if bal := e.account(from).Balance.Available; bal < amt {
						e.do(move(dr("src", amt), cr(from, amt)))
					}
					if rng.Intn(4) == 0 {
						holds = append(holds, e.do(hold(600, dr(from, amt), cr(to, amt))).Transfer.ID)
					} else {
						e.do(move(dr(from, amt), cr(to, amt)))
					}
				}
			}
			e.invariants()
			all := e.allAlerts()
			if len(all) < 30 {
				t.Fatalf("only %d alerts: the workload should cross often", len(all))
			}
			var filters []EntryFilter
			for _, mode := range []AlertMode{AlertsOff, AlertsInclude, AlertsOnly} {
				filters = append(filters, EntryFilter{Alerts: mode}, EntryFilter{Account: "b", Alerts: mode},
					EntryFilter{Kinds: []EntryKind{KindPending}, Alerts: mode})
				if mode != AlertsOff {
					filters = append(filters, EntryFilter{Alerts: mode, AlertMonitor: "m3"}, EntryFilter{Alerts: mode, AlertAccount: "c"},
						EntryFilter{Account: "a", Alerts: mode, AlertAccount: "d"}, EntryFilter{Account: "src", Alerts: mode, AlertMonitor: "m1"},
						EntryFilter{Account: "zz", Alerts: mode, AlertAccount: "a"})
				}
			}
			for _, f := range filters {
				for _, c := range []struct{ limit, budget, yield int }{{100, 0, 0}, {1, 0, 0}, {3, 2, 1}, {7, 5, 3}, {2, 1, 2}} {
					if c.budget > 0 {
						withFeedBudget(t, c.budget)
						withAlertYield(t, c.yield)
					}
					after := uint64(rng.Int63n(int64(e.head()) + 1))
					if c.limit == 100 {
						after = 0
					}
					got := walkFeedItems(t, e.l, f, after, c.limit)
					sameItems(t, fmt.Sprintf("%+v %+v after %d", f, c, after), got, scanJournalAlerts(e, f, after, all))
				}
				withFeedBudget(t, minFeedScan)
				withAlertYield(t, 1024)
			}
		})
	}
}

func TestWebhookAlertSubscription(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	spec := &WebhookSpec{ID: "alerts", URL: "https://hooks.example.test/a", Secret: testSecret, Alerts: AlertsOnly, AlertAccount: "alice"}
	h := e.do(Command{CreateWebhook: spec}).Webhook
	f := h.Filter()
	if h.Alerts != AlertsOnly || h.AlertAccount != "alice" || f.Alerts != AlertsOnly || f.AlertAccount != "alice" || f.AlertMonitor != "" {
		t.Fatalf("%+v %+v", h, f)
	}
	for _, bad := range []WebhookSpec{
		{ID: "x", URL: "https://h.example.test/", Secret: testSecret, Alerts: "every"},
		{ID: "x", URL: "https://h.example.test/", Secret: testSecret, AlertMonitor: "m"},
		{ID: "x", URL: "https://h.example.test/", Secret: testSecret, Alerts: AlertsInclude, AlertAccount: "a b"},
	} {
		e.fail(Command{CreateWebhook: &bad}, CodeInvalid)
	}
}

// The alert subscription of a webhook endpoint survives a snapshot, and a
// version 9 snapshot, written before endpoints had one, still loads.
func TestWebhookAlertSnapshot(t *testing.T) {
	dir, clock := writeSmallSnapshot(t, snapVersion)
	fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
	full := openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "small", fromSnap, full)
	for _, l := range []*Ledger{fromSnap, full} {
		h, err := l.Webhook("reserve-alerts")
		if err != nil || h.Alerts != AlertsInclude || h.AlertMonitor != "reserve-low" || h.Account != "reserve" {
			t.Fatalf("%+v %v", h, err)
		}
		out, err := l.Do(context.Background(), Command{CreateWebhook: smallAlertHook}, key("k11", "alert-hook", "alert-hook"))
		if err != nil || !out.Replayed || out.Webhook.Alerts != AlertsInclude {
			t.Fatalf("replayed create: %v %+v", err, out.Webhook)
		}
		p, err := l.Entries(h.Filter(), 0, 100)
		if err != nil || p.AlertCount() == 0 {
			t.Fatalf("%+v %v", p, err)
		}
		if v := allChecks(l); len(v) > 0 {
			t.Fatal(v)
		}
	}

	old := snapWriteVersion
	snapWriteVersion = 9
	t.Cleanup(func() { snapWriteVersion = old })
	dir, clock = writeSmallSnapshot(t, 9)
	snapWriteVersion = old
	fromSnap = openAt(t, copyDir(t, dir, true), clock, nil)
	full = openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "v9", fromSnap, full)
	for _, h := range fromSnap.Webhooks() {
		if h.Alerts != AlertsOff || h.AlertMonitor != "" || h.AlertAccount != "" {
			t.Fatalf("%+v", h)
		}
	}
}

var smallAlertHook = &WebhookSpec{ID: "reserve-alerts", URL: "https://hooks.example.test/reserve", Secret: testSecret, Account: "reserve",
	Alerts: AlertsInclude, AlertMonitor: "reserve-low", AfterSeq: ptr(uint64(0))}

// writeSmallAlertHooks adds a keyed endpoint subscribed to alerts to
// writeSmallSnapshot's ledger.
func writeSmallAlertHooks(t *testing.T, e *env) {
	t.Helper()
	if _, err := e.l.Do(context.Background(), Command{CreateWebhook: smallAlertHook}, key("k11", "alert-hook", "alert-hook")); err != nil {
		t.Fatal(err)
	}
}
