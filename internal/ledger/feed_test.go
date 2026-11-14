package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func withFeedBudget(t testing.TB, n int) {
	old := feedScanBudget
	feedScanBudget = func(int) int { return n }
	t.Cleanup(func() { feedScanBudget = old })
}

// walkFeed follows the feed from after to the head with pages of limit and
// checks the paging rules on every page.
func walkFeed(t testing.TB, l *Ledger, f EntryFilter, after uint64, limit int) []Entry {
	t.Helper()
	var out []Entry
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
		for _, en := range p.Entries {
			if en.Seq <= after || en.Seq > p.Next {
				t.Fatalf("feed %+v after %d: entry %d on a page that ends at %d", f, after, en.Seq, p.Next)
			}
		}
		out = append(out, p.Entries...)
		if !p.HasMore() {
			return out
		}
		after = p.Next
	}
}

// scanJournal is the feed's answer worked out by brute force.
func scanJournal(e *env, f EntryFilter, after uint64) []Entry {
	var out []Entry
	for seq := after + 1; seq <= e.head(); seq++ {
		en := e.entry(seq)
		if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, en.Kind) {
			continue
		}
		if f.Account != "" && !slices.ContainsFunc(en.Lines, func(ln Line) bool { return ln.Account == f.Account }) {
			continue
		}
		if f.Currency != "" {
			tr, err := e.l.Transfer(en.TransferID)
			if err != nil {
				e.t.Fatal(err)
			}
			if !slices.Contains(tr.currencies(), f.Currency) {
				continue
			}
		}
		out = append(out, en)
	}
	return out
}

func seqs(es []Entry) []uint64 {
	out := make([]uint64, len(es))
	for i := range es {
		out[i] = es[i].Seq
	}
	return out
}

// feedWorkload writes every kind of entry, in USD and as USD to EUR
// conversions, and returns the conversion hold it extended.
func feedWorkload(e *env) {
	e.fxSetup()
	e.do(move(dr("alice", 300), cr("bob", 300)))
	h := e.do(hold(600, dr("alice", 200), cr("merchant", 200))).Transfer.ID
	e.do(increaseCmd(h, 50))
	e.do(extendCmd(h, 900))
	e.do(postCmd(h, 120))
	v := e.do(hold(600, dr("bob", 100), cr("merchant", 100))).Transfer.ID
	e.do(voidCmd(v))
	p := e.do(move(dr("alice", 1000), cr("merchant", 1000))).Transfer.ID
	e.do(reverseCmd(p, 400))
	e.do(usdToEUR("0.9234", 10000, 9234))
	fx := e.do(convertHold("0.9234", 60, dr("alice", 10000), cr("fx_usd", 10000), eur(dr("fx_eur", 9234)), eur(cr("alice_eur", 9234)))).Transfer.ID
	e.do(extendCmd(fx, 120))
	e.do(hold(30, dr("alice", 5), cr("bob", 5)))
	e.clock.Advance(31 * time.Second)
	if n, err := e.l.ExpireDue(context.Background()); err != nil || n != 1 {
		e.t.Fatalf("expired %d: %v", n, err)
	}
}

func TestEntriesFeed(t *testing.T) {
	e := newEnv(t, false)
	if p, err := e.l.Entries(EntryFilter{}, 0, 10); err != nil || len(p.Entries) != 0 || p.Next != 0 || p.Head != 0 || p.HasMore() {
		t.Fatalf("empty ledger: %+v, %v", p, err)
	}
	feedWorkload(e)
	head := e.head()
	seen := map[EntryKind]int{}
	for seq := uint64(1); seq <= head; seq++ {
		seen[e.entry(seq).Kind]++
	}
	if len(seen) != MaxFeedKinds {
		t.Fatalf("the workload wrote entry kinds %v, want all %d", seen, MaxFeedKinds)
	}

	// The whole journal, one page at a time and in pages of every size,
	// exactly as the journal holds it.
	for _, limit := range []int{1, 2, 3, 7, int(head), MaxListLimit} {
		got := walkFeed(t, e.l, EntryFilter{}, 0, limit)
		if want := scanJournal(e, EntryFilter{}, 0); !reflect.DeepEqual(got, want) {
			t.Fatalf("limit %d: feed %v, journal %v", limit, seqs(got), seqs(want))
		}
	}
	// Resuming from any sequence returns exactly what follows it.
	for after := uint64(0); after <= head; after++ {
		got := walkFeed(t, e.l, EntryFilter{}, after, 4)
		if len(got) != int(head-after) || len(got) > 0 && got[0].Seq != after+1 {
			t.Fatalf("after %d: %v", after, seqs(got))
		}
	}
	p, err := e.l.Entries(EntryFilter{}, head, 10)
	if err != nil || len(p.Entries) != 0 || p.Next != head || p.HasMore() {
		t.Fatalf("at the head: %+v, %v", p, err)
	}

	filters := []EntryFilter{
		{Account: "alice"}, {Account: "alice_eur"}, {Account: "merchant"}, {Account: "bank"}, {Account: "nobody"},
		{Currency: "USD"}, {Currency: "EUR"}, {Currency: "JPY"},
		{Kinds: []EntryKind{KindExtendPending}}, {Kinds: []EntryKind{KindPostPending, KindVoidPending, KindExpirePending}},
		{Kinds: []EntryKind{KindPosted}, Account: "bob"}, {Kinds: []EntryKind{KindExtendPending}, Currency: "EUR"},
		{Account: "alice", Currency: "EUR"}, {Account: "fx_eur", Kinds: []EntryKind{KindPending}},
	}
	for _, f := range filters {
		for _, after := range []uint64{0, 9, head - 3} {
			want := scanJournal(e, f, after)
			for _, limit := range []int{1, 2, 100} {
				if got := walkFeed(t, e.l, f, after, limit); !reflect.DeepEqual(got, want) {
					t.Fatalf("%+v after %d limit %d: feed %v, journal %v", f, after, limit, seqs(got), seqs(want))
				}
			}
		}
	}
	// The conversion hold's extension has no lines; it is in both of the
	// transfer's currencies.
	if got := walkFeed(t, e.l, EntryFilter{Kinds: []EntryKind{KindExtendPending}, Currency: "EUR"}, 0, 10); len(got) != 1 {
		t.Fatalf("EUR extensions: %v", seqs(got))
	}
	if got := walkFeed(t, e.l, EntryFilter{Kinds: []EntryKind{KindExtendPending}, Account: "alice"}, 0, 10); len(got) != 0 {
		t.Fatalf("an extension, which has no lines, matched an account: %v", seqs(got))
	}
	// A page that fills stops at its last entry; a page that runs out of
	// matches before the head still reports the head, so a filtered
	// consumer never rescans entries it has passed.
	all := scanJournal(e, EntryFilter{Account: "alice_eur"}, 0)
	p, _ = e.l.Entries(EntryFilter{Account: "alice_eur"}, 0, 1)
	if len(p.Entries) != 1 || p.Next != all[0].Seq || !p.HasMore() {
		t.Fatalf("first alice_eur page: %+v", p)
	}
	p, _ = e.l.Entries(EntryFilter{Account: "alice_eur"}, all[len(all)-1].Seq-1, 5)
	if len(p.Entries) != 1 || p.Next != head || p.HasMore() {
		t.Fatalf("last alice_eur page: %+v (head %d)", p, head)
	}
	p, _ = e.l.Entries(EntryFilter{Account: "nobody"}, 3, 5)
	if len(p.Entries) != 0 || p.Next != head || p.HasMore() {
		t.Fatalf("an account that does not exist: %+v", p)
	}

	bad := []struct {
		f     EntryFilter
		after uint64
		limit int
	}{
		{EntryFilter{}, 0, 0}, {EntryFilter{}, 0, MaxListLimit + 1}, {EntryFilter{}, head + 1, 10},
		{EntryFilter{Account: "-x"}, 0, 10}, {EntryFilter{Currency: "usd"}, 0, 10}, {EntryFilter{Currency: "XAU"}, 0, 10},
		{EntryFilter{Kinds: []EntryKind{"refund"}}, 0, 10}, {EntryFilter{Kinds: make([]EntryKind, MaxFeedKinds+1)}, 0, 10},
	}
	for _, b := range bad {
		var le *Error
		if _, err := e.l.Entries(b.f, b.after, b.limit); !errors.As(err, &le) || le.Code != CodeInvalid {
			t.Errorf("Entries(%+v, %d, %d): %v, want invalid_argument", b.f, b.after, b.limit, err)
		}
	}
}

func TestParseEntryKinds(t *testing.T) {
	got, err := ParseEntryKinds("posted,extend_pending,reversal")
	if err != nil || !reflect.DeepEqual(got, []EntryKind{KindPosted, KindExtendPending, KindReversal}) {
		t.Fatalf("%v, %v", got, err)
	}
	for _, s := range []string{"", "posted,", ",posted", "posted,posted", "Posted", "refund",
		"posted,pending,post_pending,void_pending,expire_pending,reversal,increase_pending,extend_pending,posted"} {
		if k, err := ParseEntryKinds(s); err == nil {
			t.Errorf("ParseEntryKinds(%q) = %v", s, k)
		}
	}
}

// A filter that rarely matches stops after its scan budget with a short
// page, and the walk still returns every match exactly once.
func TestEntriesScanBudget(t *testing.T) {
	withFeedBudget(t, 5)
	e := newEnv(t, false)
	feedWorkload(e)
	head := e.head()
	f := EntryFilter{Kinds: []EntryKind{KindExtendPending}}
	// Entry 5 is the first extension; 6 to 10 are not extensions.
	if en := e.entry(5); en.Kind != KindExtendPending {
		t.Fatalf("entry 5 is %s", en.Kind)
	}
	p, err := e.l.Entries(f, 5, 100)
	if err != nil || len(p.Entries) != 0 || p.Next != 10 || !p.HasMore() {
		t.Fatalf("page after 5: %+v, %v", p, err)
	}
	pages := 0
	var got []Entry
	for after := uint64(0); ; pages++ {
		p, err := e.l.Entries(f, after, 100)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, p.Entries...)
		if !p.HasMore() {
			break
		}
		after = p.Next
	}
	if want := scanJournal(e, f, 0); !reflect.DeepEqual(got, want) || pages < int(head/5)-1 {
		t.Fatalf("%d pages: %v, want %v", pages, seqs(got), seqs(want))
	}
	// An account's walk counts its own rows against the budget.
	acct := walkFeed(t, e.l, EntryFilter{Account: "alice"}, 0, 100)
	if want := scanJournal(e, EntryFilter{Account: "alice"}, 0); !reflect.DeepEqual(acct, want) {
		t.Fatalf("alice: %v, want %v", seqs(acct), seqs(want))
	}
}

// The feed holds no lock while it scans: at every entry it examines, the
// test commits more transfers, which must not wait for it. The page is the
// journal as of the head it noted, and the next page picks up what was
// committed meanwhile.
func TestEntriesDoNotBlockCommits(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	for i := 0; i < 20; i++ {
		e.do(move(dr("bank", 10), cr("alice", 10)))
	}
	head := e.head()
	committed := 0
	e.l.feedHook = func(uint64) {
		done := make(chan error, 1)
		go func() {
			_, err := e.l.Do(context.Background(), move(dr("bank", 1), cr("bob", 1)), nil)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
			committed++
		case <-time.After(10 * time.Second):
			t.Fatal("a commit waited for the feed")
		}
	}
	p, err := e.l.Entries(EntryFilter{}, 0, MaxListLimit)
	e.l.feedHook = nil
	if err != nil {
		t.Fatal(err)
	}
	if committed != int(head) || p.Head != head || p.Next != head || len(p.Entries) != int(head) {
		t.Fatalf("%d commits during the scan; page %d entries, next %d, head %d (was %d)", committed, len(p.Entries), p.Next, p.Head, head)
	}
	for i := range p.Entries {
		if !reflect.DeepEqual(p.Entries[i], e.entry(uint64(i+1))) {
			t.Fatalf("entry %d changed under the feed", i+1)
		}
	}
	rest, err := e.l.Entries(EntryFilter{Account: "bob"}, p.Next, MaxListLimit)
	if err != nil || len(rest.Entries) != committed || rest.Head != head+uint64(committed) {
		t.Fatalf("next page: %d entries, head %d: %v", len(rest.Entries), rest.Head, err)
	}
}

// A consumer follows the feed while clients commit concurrently: it must
// see every entry exactly once and in order, and a filtered consumer must
// see exactly the matching entries, whatever page sizes they use.
func TestEntriesFollowConcurrentWrites(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 1_000_000), cr("alice", 1_000_000)))
	const writers, each = 4, 300
	var wg sync.WaitGroup
	var stop atomic.Bool
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w)))
			for i := 0; i < each; i++ {
				var c Command
				switch r.Intn(3) {
				case 0:
					c = move(dr("alice", 1), cr("bob", 1))
				case 1:
					c = hold(600, dr("alice", 2), cr("merchant", 2))
				default:
					c = move(dr("bank", 3), cr("equity", 3))
				}
				if _, err := e.l.Do(context.Background(), c, nil); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	follow := func(f EntryFilter, seed int64) []Entry {
		r := rand.New(rand.NewSource(seed))
		var out []Entry
		var after uint64
		for {
			last := stop.Load()
			p, err := e.l.Entries(f, after, []int{1, 3, 50, MaxListLimit}[r.Intn(4)])
			if err != nil {
				t.Error(err)
				return nil
			}
			out = append(out, p.Entries...)
			after = p.Next
			if last && !p.HasMore() {
				return out
			}
		}
	}
	var all, bob []Entry
	var fw sync.WaitGroup
	fw.Add(2)
	go func() { defer fw.Done(); all = follow(EntryFilter{}, 1) }()
	go func() { defer fw.Done(); bob = follow(EntryFilter{Account: "bob"}, 2) }()
	wg.Wait()
	stop.Store(true)
	fw.Wait()
	head := e.head()
	if head != uint64(1+writers*each) {
		t.Fatalf("head %d", head)
	}
	if want := scanJournal(e, EntryFilter{}, 0); !reflect.DeepEqual(all, want) {
		t.Fatalf("followed %d entries, journal has %d", len(all), len(want))
	}
	if want := scanJournal(e, EntryFilter{Account: "bob"}, 0); !reflect.DeepEqual(bob, want) {
		t.Fatalf("followed %d bob entries, journal has %d", len(bob), len(want))
	}
}

// The feed serves the whole journal after a restart, also once the log
// segments that held its first entries have been dropped behind snapshots.
func TestEntriesAfterSegmentsDropped(t *testing.T) {
	e := newEnv(t, true)
	e.opts.SegmentBytes, e.opts.SnapshotEvery = 1024, 10
	e.restart()
	feedWorkload(e)
	for i := 0; i < 60; i++ {
		e.do(move(dr("alice", 1), cr("bob", 1)))
	}
	waitSnapshots(e.l)
	before := walkFeed(t, e.l, EntryFilter{}, 0, 7)
	beforeEUR := walkFeed(t, e.l, EntryFilter{Currency: "EUR"}, 0, 2)
	e.restart()
	if e.l.log.FirstIndex() == 1 {
		t.Fatal("no segment was dropped")
	}
	if after := walkFeed(t, e.l, EntryFilter{}, 0, 7); !reflect.DeepEqual(after, before) || len(after) != int(e.head()) {
		t.Fatalf("after the restart the feed has %d entries, before %d", len(after), len(before))
	}
	if after := walkFeed(t, e.l, EntryFilter{Currency: "EUR"}, 0, 2); !reflect.DeepEqual(after, beforeEUR) {
		t.Fatalf("EUR entries %v, before %v", seqs(after), seqs(beforeEUR))
	}
}

// BenchmarkEntries reads feed pages of 100 from a ledger of 200,000
// entries among 1,000 accounts: the whole feed, one account's entries, and
// a kind that is one entry in a hundred.
func BenchmarkEntries(b *testing.B) {
	l, err := Open(Options{SweepInterval: -1})
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	const accounts, entries = 1000, 200_000
	for i := 0; i < accounts; i++ {
		if _, err := l.Do(ctx, openAcct(fmt.Sprintf("a%04d", i), "USD", Credit), nil); err != nil {
			b.Fatal(err)
		}
	}
	r := rand.New(rand.NewSource(1))
	batch := &BatchSpec{}
	for i := 0; i < entries; i++ {
		x, y := r.Intn(accounts), r.Intn(accounts-1)
		if y >= x {
			y++
		}
		spec := TransferSpec{Currency: "USD", Postings: []PostingSpec{dr(fmt.Sprintf("a%04d", x), 1), cr(fmt.Sprintf("a%04d", y), 1)}}
		if i%100 == 0 {
			spec.Pending, spec.TimeoutSeconds = true, 3600
		}
		batch.Transfers = append(batch.Transfers, spec)
		if len(batch.Transfers) == MaxBatchTransfers || i == entries-1 {
			if _, err := l.Do(ctx, Command{CreateBatch: batch}, nil); err != nil {
				b.Fatal(err)
			}
			batch = &BatchSpec{}
		}
	}
	for _, bc := range []struct {
		name string
		f    EntryFilter
	}{
		{"all", EntryFilter{}},
		{"account", EntryFilter{Account: "a0500"}},
		{"rare-kind", EntryFilter{Kinds: []EntryKind{KindPending}}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			var after uint64
			n := 0
			for b.Loop() {
				p, err := l.Entries(bc.f, after, 100)
				if err != nil {
					b.Fatal(err)
				}
				n += len(p.Entries)
				after = p.Next
				if !p.HasMore() {
					after = 0
				}
			}
			b.ReportMetric(float64(n)/float64(b.N), "entries/op")
		})
	}
}
