package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// walkTransfers pages through a transfer listing to the end and returns the
// IDs it listed, checking the page invariants on the way.
func walkTransfers(t testing.TB, l *Ledger, f TransferFilter, desc bool, limit int) []string {
	t.Helper()
	ids, err := walkTransfersErr(l, f, desc, limit, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// walkTransfersErr is walkTransfers for goroutines. Each page must hold at
// most limit transfers, all matching f, in strictly increasing (or, with
// desc, decreasing) creation order, and each as the transfer was at the
// page's AsOfSeq: created by then and not changed since (checked against
// changedAt, the head after which a transfer's newest version was
// committed, when it is given).
func walkTransfersErr(l *Ledger, f TransferFilter, desc bool, limit int, check func(TransferPage) error) ([]string, error) {
	var out []string
	var from, asOf uint64
	for pages := 0; ; pages++ {
		if pages > 100_000 {
			return nil, errors.New("listing never ended")
		}
		p, err := l.ListTransfers(f, from, desc, limit)
		if err != nil {
			return nil, err
		}
		if len(p.Transfers) > limit {
			return nil, fmt.Errorf("page holds %d transfers, limit %d", len(p.Transfers), limit)
		}
		if p.AsOfSeq < asOf {
			return nil, fmt.Errorf("page as of %d after a page as of %d", p.AsOfSeq, asOf)
		}
		asOf = p.AsOfSeq
		for _, tr := range p.Transfers {
			seq := createdAt(tr)
			if from != 0 && (!desc && seq <= from || desc && seq >= from) {
				return nil, fmt.Errorf("filter %+v desc %v: %s (seq %d) listed after position %d", f, desc, tr.ID, seq, from)
			}
			if seq > p.AsOfSeq || tr.Entries[len(tr.Entries)-1].Seq > p.AsOfSeq {
				return nil, fmt.Errorf("%s has entries after the page's as-of %d", tr.ID, p.AsOfSeq)
			}
			if !refMatches(f, tr) {
				return nil, fmt.Errorf("filter %+v: %s (%s %s %q %v) does not match", f, tr.ID, tr.State, tr.Currency, tr.Reference, tr.Metadata)
			}
			from = seq
			out = append(out, tr.ID)
		}
		if check != nil {
			if err := check(p); err != nil {
				return nil, err
			}
		}
		if p.Next == 0 {
			return out, nil
		}
		if !desc && p.Next < from || desc && from != 0 && p.Next > from {
			return nil, fmt.Errorf("next position %d goes back from %d", p.Next, from)
		}
		from = p.Next
	}
}

// refMatches is the filter written out independently of
// TransferFilter.matches.
func refMatches(f TransferFilter, tr *Transfer) bool {
	ok := (f.State == "" || f.State == tr.State) &&
		(f.Currency == "" || f.Currency == tr.Currency) &&
		(f.Reference == "" || f.Reference == tr.Reference)
	if f.Account != "" {
		on := false
		for _, p := range tr.Postings {
			on = on || p.Account == f.Account
		}
		ok = ok && on
	}
	for k, v := range f.Metadata {
		got, has := tr.Metadata[k]
		ok = ok && has && got == v
	}
	return ok
}

// bruteTransfers lists, by a scan of every transfer, the IDs matching f in
// creation order.
func bruteTransfers(l *Ledger, f TransferFilter, desc bool) []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var all []*Transfer
	for _, tr := range l.st.transfers {
		if refMatches(f, tr) {
			all = append(all, tr)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Entries[0].Seq < all[j].Entries[0].Seq })
	if desc {
		slices.Reverse(all)
	}
	out := make([]string, len(all))
	for i, tr := range all {
		out[i] = tr.ID
	}
	return out
}

// transferFilters covers each filter alone, combinations, and values no
// transfer has. indexWorkload's accounts and labels are what they name.
var transferFilters = []TransferFilter{
	{},
	{State: StatePending},
	{State: StatePosted},
	{State: StateVoided},
	{State: StateExpired},
	{Currency: "EUR"},
	{Currency: "GBP"},
	{Account: "usd-00"},
	{Account: "jpy-11"},
	{Account: "nobody"},
	{Reference: "inv-3"},
	{Reference: "refund-2"},
	{Reference: "open-hold"},
	{Reference: "never-used"},
	{Metadata: map[string]string{"channel": "web"}},
	{Metadata: map[string]string{"order": "7", "channel": "web"}},
	{Metadata: map[string]string{"order": "99"}},
	{Account: "eur-04", State: StatePosted},
	{Currency: "USD", Reference: "inv-5", State: StatePosted},
	{Account: "usd-06", Metadata: map[string]string{"channel": "web"}, State: StateVoided},
	{Currency: "USD", Account: "eur-01"},
}

func TestListTransfersMatchesAScan(t *testing.T) {
	e := newEnv(t, true)
	e.opts.SnapshotEvery = 97
	e.restart()
	indexWorkload(t, e, 11, 1500)

	check := func(stage string) {
		t.Helper()
		nonEmpty := 0
		for _, f := range transferFilters {
			for _, desc := range []bool{false, true} {
				want := bruteTransfers(e.l, f, desc)
				if len(want) > 0 {
					nonEmpty++
				}
				for _, limit := range []int{1, 3, 50, MaxListLimit} {
					if got := walkTransfers(t, e.l, f, desc, limit); !slices.Equal(got, want) {
						t.Fatalf("%s: filter %+v desc %v limit %d: got %d transfers, want %d", stage, f, desc, limit, len(got), len(want))
					}
				}
			}
		}
		if nonEmpty < 2*(len(transferFilters)-7) {
			t.Fatalf("%s: only %d of the listings found anything", stage, nonEmpty)
		}
		// A page's transfers are the stored versions as of its AsOfSeq.
		p, err := e.l.ListTransfers(TransferFilter{}, 0, true, 10)
		if err != nil {
			t.Fatal(err)
		}
		if p.AsOfSeq != e.l.Head() || len(p.Transfers) != 10 || p.Next == 0 {
			t.Fatalf("%s: page as of %d (head %d), %d transfers, next %d", stage, p.AsOfSeq, e.l.Head(), len(p.Transfers), p.Next)
		}
		for _, tr := range p.Transfers {
			one, err := e.l.Transfer(tr.ID)
			if err != nil || one != tr {
				t.Fatalf("%s: %s listed differs from a read: %v", stage, tr.ID, err)
			}
		}
	}
	check("live")
	e.restart() // snapshot plus log
	check("after restart")
	e.invariants()
}

// TestListTransfersByReference is the statement-free lookup: finding the
// transfers that carry an external reference reads only that reference's
// index set, however many other transfers the accounts have.
func TestListTransfersByReference(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 1_000_000), cr("alice", 1_000_000)))
	var want []string
	for i := range 3000 {
		c := move(dr("alice", 1), cr("merchant", 1))
		if i%1000 == 7 {
			c.CreateTransfer.Reference = "payout-42"
		}
		if out := e.do(c); c.CreateTransfer.Reference != "" {
			want = append(want, out.Transfer.ID)
		}
	}
	rev := e.do(Command{Reverse: &ReverseSpec{TransferID: want[1], Amounts: Amounts{Amount: 1}, Reference: "payout-42"}})
	want = append(want, rev.Transfer.ID)

	p, err := e.l.ListTransfers(TransferFilter{Reference: "payout-42"}, 0, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tr := range p.Transfers {
		got = append(got, tr.ID)
	}
	if !slices.Equal(got, want) || p.Next != 0 {
		t.Fatalf("by reference: got %v next %d, want %v", got, p.Next, want)
	}
	if got := e.l.examined(TransferFilter{Reference: "payout-42", Account: "alice"}, 0, false); got != len(want) {
		t.Fatalf("lookup examined %d transfers, want %d", got, len(want))
	}
	if got := walkTransfers(t, e.l, TransferFilter{Reference: "payout-42", State: StatePosted, Account: "merchant"}, true, 2); !slices.Equal(got, []string{want[3], want[2], want[1], want[0]}) {
		t.Fatalf("by reference, newest first: %v", got)
	}
}

// examined counts the candidates a listing with filter f walks after from,
// using the same choice of set as ListTransfers. With to it stops there.
func (l *Ledger) examined(f TransferFilter, from uint64, desc bool, to ...uint64) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	src := f.source(l.st.ix)
	walk := src.ascend(from, from == 0)
	if desc {
		walk = src.descend(from, from == 0)
	}
	n := 0
	for seq := range walk {
		if len(to) > 0 && (!desc && seq > to[0] || desc && seq < to[0]) {
			break
		}
		n++
	}
	return n
}

// TestListTransfersScanBudget shrinks the scan budget so that filters which
// rarely match run out of it, and checks that paging still returns exactly
// the matches in both orders, that some pages come back short, and that no
// page examined more than the budget.
func TestListTransfersScanBudget(t *testing.T) {
	e := newEnv(t, false)
	indexWorkload(t, e, 12, 800)
	const budget = 4
	old := listScanBudget
	listScanBudget = func(int) int { return budget }
	t.Cleanup(func() { listScanBudget = old })

	short := 0
	for _, f := range transferFilters {
		for _, desc := range []bool{false, true} {
			want := bruteTransfers(e.l, f, desc)
			for _, limit := range []int{1, 2, 30} {
				got, err := walkTransfersErr(e.l, f, desc, limit, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got, want) {
					t.Fatalf("filter %+v desc %v limit %d: got %v, want %v", f, desc, limit, got, want)
				}
				var from uint64
				for {
					p, _ := e.l.ListTransfers(f, from, desc, limit)
					if p.Next == 0 {
						break
					}
					if len(p.Transfers) < limit {
						short++
					}
					if n := e.l.examined(f, from, desc, p.Next); n > budget {
						t.Fatalf("filter %+v desc %v: page from %d to %d examined %d candidates, budget %d", f, desc, from, p.Next, n, budget)
					}
					from = p.Next
				}
			}
		}
	}
	if short == 0 {
		t.Fatal("no page ran out of budget")
	}
}

func TestListTransfersErrors(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	var trs []string
	for range 5 {
		trs = append(trs, e.do(move(dr("bank", 5), cr("alice", 5))).Transfer.ID)
	}
	many := map[string]string{}
	for i := range MaxListMetadata + 1 {
		many[fmt.Sprint("k", i)] = "v"
	}
	cases := map[string]struct {
		f     TransferFilter
		limit int
	}{
		"limit zero":        {limit: 0},
		"limit too large":   {limit: MaxListLimit + 1},
		"bad account":       {f: TransferFilter{Account: "-x"}, limit: 1},
		"bad state":         {f: TransferFilter{State: "open"}, limit: 1},
		"bad currency":      {f: TransferFilter{Currency: "usd"}, limit: 1},
		"long reference":    {f: TransferFilter{Reference: strings.Repeat("r", MaxReference+1)}, limit: 1},
		"control in ref":    {f: TransferFilter{Reference: "a\nb"}, limit: 1},
		"too many metadata": {f: TransferFilter{Metadata: many}, limit: 1},
		"empty metadata":    {f: TransferFilter{Metadata: map[string]string{"": "v"}}, limit: 1},
	}
	for name, c := range cases {
		_, err := e.l.ListTransfers(c.f, 0, false, c.limit)
		var le *Error
		if !errors.As(err, &le) || le.Code != CodeInvalid {
			t.Errorf("%s: err = %v, want invalid_argument", name, err)
		}
	}
	list := func(from uint64, desc bool, limit int) ([]string, uint64) {
		p, err := e.l.ListTransfers(TransferFilter{}, from, desc, limit)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, tr := range p.Transfers {
			ids = append(ids, tr.ID)
		}
		return ids, p.Next
	}
	// An exactly full last page reports no more; a page that is not last
	// continues from its last transfer.
	if ids, next := list(0, false, 5); !slices.Equal(ids, trs) || next != 0 {
		t.Fatalf("full page: %v next %d", ids, next)
	}
	if ids, next := list(0, false, 2); !slices.Equal(ids, trs[:2]) || next != 2 {
		t.Fatalf("page of 2: %v next %d", ids, next)
	}
	if ids, next := list(0, true, 2); !slices.Equal(ids, []string{trs[4], trs[3]}) || next != 4 {
		t.Fatalf("newest page of 2: %v next %d", ids, next)
	}
	// A position need not be a transfer, and may be past either end.
	if ids, next := list(3, true, 10); !slices.Equal(ids, []string{trs[1], trs[0]}) || next != 0 {
		t.Fatalf("before 3: %v next %d", ids, next)
	}
	if ids, _ := list(99, true, 10); len(ids) != 5 {
		t.Fatalf("before 99: %v", ids)
	}
	if ids, next := list(99, false, 10); len(ids) != 0 || next != 0 {
		t.Fatalf("after 99: %v next %d", ids, next)
	}
}

// TestListTransfersDuringConcurrentWrites pages through transfer listings in
// both orders while writers create transfers and holds and post, void and
// reverse them. Every walk must be in order with no repeats, list only
// transfers that matched at their page's AsOfSeq, and list every transfer
// that existed when it began and matched for the whole walk: for a state
// filter, a transfer already in a final state (posted, voided, expired).
func TestListTransfersDuringConcurrentWrites(t *testing.T) {
	e := newEnv(t, true)
	e.opts.SnapshotEvery = 200
	e.restart()
	e.standard()
	e.do(move(dr("bank", 1_000_000_000), cr("alice", 1_000_000_000)))
	e.do(move(dr("bank", 1_000_000_000), cr("bob", 1_000_000_000)))

	// final holds transfers in a final state, by state, and labels every
	// transfer's fixed fields, so walks know what they must list.
	type known struct {
		id, ref string
		accts   []string
		seq     uint64
	}
	var mu sync.Mutex
	var all []known
	final := map[TransferState]map[string]bool{StatePosted: {}, StateVoided: {}, StateExpired: {}}
	note := func(tr *Transfer) {
		mu.Lock()
		defer mu.Unlock()
		if tr.Entries[len(tr.Entries)-1].Kind != KindPending || len(tr.Entries) > 1 {
			if final[tr.State] != nil {
				final[tr.State][tr.ID] = true
			}
		}
		if len(tr.Entries) == 1 {
			k := known{id: tr.ID, ref: tr.Reference, seq: createdAt(tr)}
			for _, p := range tr.Postings {
				k.accts = append(k.accts, p.Account)
			}
			all = append(all, k)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var writes atomic.Int64
	for w := range 4 {
		wg.Go(func() {
			r := rand.New(rand.NewSource(int64(300 + w)))
			payer := []string{"alice", "bob"}[w%2]
			var open, done []string
			for ctx.Err() == nil {
				var c Command
				switch k := r.Intn(10); {
				case k < 4:
					c = move(dr(payer, int64(1+r.Intn(9))), cr("merchant", 0)).balanced()
				case k < 7:
					c = hold(3600, dr(payer, int64(1+r.Intn(9))), cr("merchant", 0)).balanced()
				case k < 9 && len(open) > 0:
					i := r.Intn(len(open))
					id := open[i]
					open = slices.Delete(open, i, i+1)
					if r.Intn(2) == 0 {
						c = postCmd(id, 0)
					} else {
						c = voidCmd(id)
					}
				case len(done) > 0:
					c = reverseCmd(done[r.Intn(len(done))], 1)
				default:
					continue
				}
				if c.CreateTransfer != nil {
					c.CreateTransfer.Reference = fmt.Sprintf("ref-%d", r.Intn(6))
				}
				out, err := e.l.Do(context.Background(), c, nil)
				if err != nil {
					continue // a fully reversed transfer refuses another reversal
				}
				tr := out.Transfer
				note(tr)
				if tr.State == StatePending {
					open = append(open, tr.ID)
				} else if tr.State == StatePosted && tr.Reverses == "" {
					done = append(done, tr.ID)
				}
				writes.Add(1)
			}
		})
	}

	filters := []TransferFilter{{}, {State: StatePending}, {State: StatePosted}, {State: StateVoided},
		{Account: "bob"}, {Reference: "ref-2"}, {Account: "alice", State: StatePosted}, {Reference: "ref-4", State: StateVoided}}
	errc := make(chan error, 16)
	var walks atomic.Int64
	var pagers sync.WaitGroup
	for p := range 3 {
		pagers.Go(func() {
			r := rand.New(rand.NewSource(int64(400 + p)))
			for i := 0; i < 5000 && (i < 16 || writes.Load() < 1500); i++ {
				f := filters[(p+i)%len(filters)]
				desc := r.Intn(2) == 0
				limit := 1 + r.Intn(60)
				// What must be listed is fixed before the walk begins.
				mu.Lock()
				must := map[string]bool{}
				for _, k := range all {
					if (f.Reference == "" || k.ref == f.Reference) &&
						(f.Account == "" || slices.Contains(k.accts, f.Account)) &&
						(f.State == "" || final[f.State][k.id]) {
						must[k.id] = true
					}
				}
				mu.Unlock()
				if f.State == StatePending {
					must = nil // a pending transfer may be posted or voided mid-walk
				}
				got, err := walkTransfersErr(e.l, f, desc, limit, nil)
				if err != nil {
					errc <- err
					return
				}
				seen := map[string]bool{}
				for _, id := range got {
					if seen[id] {
						errc <- fmt.Errorf("filter %+v: %s listed twice", f, id)
						return
					}
					seen[id] = true
				}
				for id := range must {
					if !seen[id] {
						errc <- fmt.Errorf("filter %+v desc %v limit %d: %s existed and matched before the walk and was not listed", f, desc, limit, id)
						return
					}
				}
				walks.Add(1)
			}
		})
	}
	pagers.Wait()
	cancel()
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	if writes.Load() < 1500 {
		t.Fatalf("writers did only %d commands", writes.Load())
	}
	t.Logf("%d walks during %d commands", walks.Load(), writes.Load())
	e.invariants()
	for _, f := range filters {
		for _, desc := range []bool{false, true} {
			if got, want := walkTransfers(t, e.l, f, desc, 23), bruteTransfers(e.l, f, desc); !slices.Equal(got, want) {
				t.Fatalf("filter %+v desc %v after writes: got %d transfers, want %d", f, desc, len(got), len(want))
			}
		}
	}
}

// TestListTransfersSeesExpiry checks that a hold that expires moves between
// state listings at the entry that expired it.
func TestListTransfersSeesExpiry(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 100), cr("alice", 100)))
	h := e.do(hold(10, dr("alice", 7), cr("merchant", 7))).Transfer.ID
	pending := walkTransfers(t, e.l, TransferFilter{State: StatePending, Account: "alice"}, false, 10)
	if !slices.Equal(pending, []string{h}) {
		t.Fatalf("pending: %v", pending)
	}
	e.clock.Advance(11 * time.Second)
	if _, err := e.l.ExpireDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := walkTransfers(t, e.l, TransferFilter{State: StatePending}, false, 10); len(got) != 0 {
		t.Fatalf("pending after expiry: %v", got)
	}
	p, err := e.l.ListTransfers(TransferFilter{State: StateExpired}, 0, false, 10)
	if err != nil || len(p.Transfers) != 1 || p.Transfers[0].ID != h || p.Transfers[0].State != StateExpired || p.AsOfSeq != e.l.Head() {
		t.Fatalf("expired: %+v %v", p, err)
	}
}

// BenchmarkListTransfers pages through 200,000 transfers among 1,000
// accounts: all of them, one account's, one state's, and a reference that
// 20 transfers carry (the lookup by external reference).
func BenchmarkListTransfers(b *testing.B) {
	e := newEnv(b, false)
	ctx := context.Background()
	e.do(openAcct("bank", "USD", Debit))
	for i := range 1000 {
		e.do(openAcct(fmt.Sprintf("acct-%04d", i), "USD", Credit))
	}
	for i := range 200_000 {
		c := move(dr("bank", 1), cr(fmt.Sprintf("acct-%04d", i%1000), 1))
		if i%10 == 0 {
			c = hold(3600, dr("bank", 1), cr(fmt.Sprintf("acct-%04d", i%1000), 1))
		}
		c.CreateTransfer.Reference = fmt.Sprintf("batch-%05d", i/20)
		if _, err := e.l.Do(ctx, c, nil); err != nil {
			b.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		f    TransferFilter
		desc bool
	}{
		{"all", TransferFilter{}, false},
		{"all-newest-first", TransferFilter{}, true},
		{"account", TransferFilter{Account: "acct-0042"}, false},
		{"state-pending", TransferFilter{State: StatePending}, true},
		{"reference", TransferFilter{Reference: "batch-04242"}, false},
	} {
		b.Run(c.name, func(b *testing.B) {
			var from uint64
			n := 0
			for b.Loop() {
				p, err := e.l.ListTransfers(c.f, from, c.desc, 100)
				if err != nil {
					b.Fatal(err)
				}
				n += len(p.Transfers)
				from = p.Next
			}
			b.ReportMetric(float64(n)/float64(b.N), "transfers/op")
		})
	}
}
