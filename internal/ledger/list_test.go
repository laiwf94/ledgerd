package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
)

// walkAccounts pages through a listing to the end and returns every account
// it returned, checking the page invariants on the way.
func walkAccounts(t *testing.T, l *Ledger, f AccountFilter, limit int) []AccountState {
	t.Helper()
	var out []AccountState
	after := ""
	for pages := 0; ; pages++ {
		if pages > 100_000 {
			t.Fatal("listing never ended")
		}
		p, err := l.ListAccounts(f, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Accounts) > limit {
			t.Fatalf("page holds %d accounts, limit %d", len(p.Accounts), limit)
		}
		out = append(out, p.Accounts...)
		if p.Next == "" {
			return out
		}
		if p.Next <= after {
			t.Fatalf("listing went back from %q to %q", after, p.Next)
		}
		after = p.Next
	}
}

func ids(as []AccountState) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.ID
	}
	return out
}

// listFixture opens accounts across three currencies with overlapping
// metadata and returns the definitions by ID.
func listFixture(t *testing.T, e *env, n int, rng *rand.Rand) map[string]*AccountSpec {
	t.Helper()
	want := map[string]*AccountSpec{}
	currencies := []string{"USD", "EUR", "JPY"}
	for len(want) < n {
		id := fmt.Sprintf("acct-%06d", rng.Intn(1_000_000))
		if want[id] != nil {
			continue
		}
		s := &AccountSpec{ID: id, Currency: currencies[rng.Intn(3)], NormalBalance: Credit, Metadata: map[string]string{}}
		if rng.Intn(2) == 0 {
			s.Metadata["tier"] = []string{"gold", "silver"}[rng.Intn(2)]
		}
		if rng.Intn(3) == 0 {
			s.Metadata["region"] = []string{"eu", "us", "apac"}[rng.Intn(3)]
		}
		if rng.Intn(50) == 0 {
			s.Metadata["vip"] = ""
		}
		e.do(Command{CreateAccount: s})
		want[id] = s
	}
	return want
}

func brute(defs map[string]*AccountSpec, f AccountFilter) []string {
	var out []string
	for id, s := range defs {
		if f.matches(&Account{ID: id, Currency: s.Currency, Metadata: s.Metadata}) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

var listFilters = []AccountFilter{
	{},
	{Currency: "USD"},
	{Currency: "EUR"},
	{Currency: "GBP"},
	{Metadata: map[string]string{"tier": "gold"}},
	{Metadata: map[string]string{"vip": ""}},
	{Metadata: map[string]string{"tier": "bronze"}},
	{Currency: "JPY", Metadata: map[string]string{"tier": "silver"}},
	{Currency: "USD", Metadata: map[string]string{"tier": "gold", "region": "eu"}},
	{Metadata: map[string]string{"tier": "gold", "region": "us"}},
}

// fund opens a USD funding account (added to defs) and moves money from it
// into n random USD accounts.
func fund(t *testing.T, e *env, defs map[string]*AccountSpec, n int, rng *rand.Rand) {
	t.Helper()
	e.do(Command{CreateAccount: &AccountSpec{ID: "bank", Currency: "USD", NormalBalance: Debit}})
	defs["bank"] = &AccountSpec{ID: "bank", Currency: "USD"}
	var usd []string
	for id, s := range defs {
		if s.Currency == "USD" && id != "bank" {
			usd = append(usd, id)
		}
	}
	sort.Strings(usd)
	for range n {
		e.do(move(dr("bank", int64(1+rng.Intn(1000))), cr(usd[rng.Intn(len(usd))], 0)).balanced())
	}
}

// balanced makes a two-leg move's credit equal its debit.
func (c Command) balanced() Command {
	c.CreateTransfer.Postings[1].Amount = c.CreateTransfer.Postings[0].Amount
	return c
}

func TestListAccountsMatchesAScan(t *testing.T) {
	e := newEnv(t, true)
	rng := rand.New(rand.NewSource(1))
	defs := listFixture(t, e, 600, rng)
	fund(t, e, defs, 200, rng)

	check := func(stage string) {
		t.Helper()
		for _, f := range listFilters {
			want := brute(defs, f)
			for _, limit := range []int{1, 2, 7, 100, MaxListLimit} {
				got := walkAccounts(t, e.l, f, limit)
				if !slices.Equal(ids(got), want) {
					t.Fatalf("%s: filter %+v limit %d: got %d accounts, want %d", stage, f, limit, len(got), len(want))
				}
			}
		}
		// Every balance on a page is as of the page's AsOfSeq.
		p, err := e.l.ListAccounts(AccountFilter{Currency: "USD"}, "", MaxListLimit)
		if err != nil {
			t.Fatal(err)
		}
		if p.AsOfSeq != e.l.Head() || p.Next != "" {
			t.Fatalf("%s: page as of %d, head %d, next %q", stage, p.AsOfSeq, e.l.Head(), p.Next)
		}
		var sum int64
		for _, a := range p.Accounts {
			if a.AsOfSeq != p.AsOfSeq {
				t.Fatalf("%s: %s as of %d on a page as of %d", stage, a.ID, a.AsOfSeq, p.AsOfSeq)
			}
			one, err := e.l.Account(a.ID, p.AsOfSeq)
			if err != nil {
				t.Fatal(err)
			}
			if one.Balance != a.Balance || one.Version != a.Version {
				t.Fatalf("%s: %s listed with %+v v%d, read as %+v v%d", stage, a.ID, a.Balance, a.Version, one.Balance, one.Version)
			}
			sum += a.Balance.DebitsPosted - a.Balance.CreditsPosted
		}
		if sum != 0 {
			t.Fatalf("%s: USD debits and credits differ by %d", stage, sum)
		}
	}
	check("live")
	e.restart() // replay from snapshot plus log
	check("after restart")
}

// TestListAccountsScanBudget shrinks the scan budget so that filters which
// rarely match run out of it, and checks that paging still returns exactly
// the matches, that some pages come back short or empty with a position to
// continue from, and that no page examined more than the budget.
func TestListAccountsScanBudget(t *testing.T) {
	e := newEnv(t, false)
	rng := rand.New(rand.NewSource(2))
	defs := listFixture(t, e, 400, rng)
	const budget = 5
	old := listScanBudget
	listScanBudget = func(int) int { return budget }
	t.Cleanup(func() { listScanBudget = old })

	short := 0
	for _, f := range listFilters {
		want := brute(defs, f)
		for _, limit := range []int{1, 3, 50} {
			var got []string
			after := ""
			for {
				p, err := e.l.ListAccounts(f, after, limit)
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, ids(p.Accounts)...)
				if p.Next == "" {
					break
				}
				if len(p.Accounts) < limit {
					short++
				}
				// The page examined the members of the walked set between after
				// and Next: no more than the budget, plus one when the page
				// filled and peeked at the next match.
				if n := e.l.countBetween(f, after, p.Next); n > budget {
					t.Fatalf("filter %+v: page from %q to %q examined %d candidates, budget %d", f, after, p.Next, n, budget)
				}
				after = p.Next
			}
			if !slices.Equal(got, want) {
				t.Fatalf("filter %+v limit %d: got %v, want %v", f, limit, got, want)
			}
		}
	}
	if short == 0 {
		t.Fatal("no page ran out of budget")
	}
}

// countBetween counts the candidates a listing with filter f walks in
// (after, to], using the same choice of set as ListAccounts.
func (l *Ledger) countBetween(f AccountFilter, after, to string) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	src := &l.st.ix.accounts
	if f.Currency != "" {
		src = l.st.ix.acctCurrency[f.Currency]
	}
	for k, v := range f.Metadata {
		if s := l.st.ix.acctMeta[metaPair{k, v}]; s.Len() < src.Len() {
			src = s
		}
	}
	n := 0
	for id := range src.ascend(after, after == "") {
		if id > to {
			break
		}
		n++
	}
	return n
}

func TestListAccountsErrors(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	long := string(make([]byte, MaxMetadataKey+1))
	many := map[string]string{}
	for i := range MaxListMetadata + 1 {
		many[fmt.Sprint("k", i)] = "v"
	}
	cases := map[string]struct {
		f     AccountFilter
		after string
		limit int
	}{
		"limit zero":         {limit: 0},
		"limit too large":    {limit: MaxListLimit + 1},
		"bad position":       {after: "-x", limit: 1},
		"bad currency":       {f: AccountFilter{Currency: "usd"}, limit: 1},
		"too many metadata":  {f: AccountFilter{Metadata: many}, limit: 1},
		"empty metadata key": {f: AccountFilter{Metadata: map[string]string{"": "v"}}, limit: 1},
		"long metadata key":  {f: AccountFilter{Metadata: map[string]string{long: "v"}}, limit: 1},
		"control in value":   {f: AccountFilter{Metadata: map[string]string{"k": "a\x00"}}, limit: 1},
	}
	for name, c := range cases {
		_, err := e.l.ListAccounts(c.f, c.after, c.limit)
		var le *Error
		if !errors.As(err, &le) || le.Code != CodeInvalid {
			t.Errorf("%s: err = %v, want invalid_argument", name, err)
		}
	}
	// A position need not be an account that exists.
	p, err := e.l.ListAccounts(AccountFilter{}, "b", 10)
	if err != nil || fmt.Sprint(ids(p.Accounts)) != "[bank bob equity merchant]" || p.Next != "" {
		t.Fatalf("from b: %v %v next %q", err, ids(p.Accounts), p.Next)
	}
	// An exactly full last page reports no more.
	p, _ = e.l.ListAccounts(AccountFilter{}, "", 5)
	if len(p.Accounts) != 5 || p.Next != "" {
		t.Fatalf("full last page: %v next %q", ids(p.Accounts), p.Next)
	}
	p, _ = e.l.ListAccounts(AccountFilter{}, "", 4)
	if len(p.Accounts) != 4 || p.Next != "equity" {
		t.Fatalf("page of 4: %v next %q", ids(p.Accounts), p.Next)
	}
}

// TestListAccountsDuringConcurrentWrites pages through listings while other
// goroutines open accounts at random positions in the ID order and move
// money between them. Every walk must return IDs in strictly increasing
// order with no repeats, only accounts that match its filter, every
// matching account that was acknowledged before the walk began, and on each
// page balances that agree with a point-in-time read at the page's AsOfSeq.
func TestListAccountsDuringConcurrentWrites(t *testing.T) {
	e := newEnv(t, true)
	rng := rand.New(rand.NewSource(3))
	defs := listFixture(t, e, 300, rng)
	fund(t, e, defs, 50, rng)

	var mu sync.Mutex // guards known
	known := map[string]*AccountSpec{}
	var usd []string
	for id, s := range defs {
		known[id] = s
		if s.Currency == "USD" && id != "bank" {
			usd = append(usd, id)
		}
	}
	sort.Strings(usd)

	// Writers stop when ctx ends but never cancel a command, so every
	// account they open is either acknowledged or refused.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var created, moved atomic.Int64
	for w := range 4 {
		wg.Go(func() {
			rng := rand.New(rand.NewSource(int64(100 + w)))
			for ctx.Err() == nil {
				if rng.Intn(2) == 0 {
					s := &AccountSpec{ID: fmt.Sprintf("acct-%06d-w%d", rng.Intn(1_000_000), w), Currency: []string{"USD", "EUR"}[rng.Intn(2)],
						NormalBalance: Credit, Metadata: map[string]string{"tier": []string{"gold", "silver"}[rng.Intn(2)]}}
					if _, err := e.l.Do(context.Background(), Command{CreateAccount: s}, nil); err == nil {
						mu.Lock()
						known[s.ID] = s
						mu.Unlock()
						created.Add(1)
					}
					continue
				}
				n := int64(1 + rng.Intn(100))
				if _, err := e.l.Do(context.Background(), move(dr("bank", n), cr(usd[rng.Intn(len(usd))], n)), nil); err == nil {
					moved.Add(1)
				}
			}
		})
	}

	filters := []AccountFilter{{}, {Currency: "USD"}, {Metadata: map[string]string{"tier": "gold"}}, {Currency: "EUR", Metadata: map[string]string{"tier": "silver"}}}
	errc := make(chan error, 64)
	var walks atomic.Int64
	var pagers sync.WaitGroup
	for p := range 3 {
		pagers.Go(func() {
			rng := rand.New(rand.NewSource(int64(200 + p)))
			// Walk until the writers have done enough during the walks.
			for i := 0; i < 5000 && (i < 12 || created.Load() < 300 || moved.Load() < 300); i++ {
				f := filters[(p+i)%len(filters)]
				limit := 1 + rng.Intn(40)
				mu.Lock()
				var must []string
				for id, s := range known {
					if f.matches(&Account{Currency: s.Currency, Metadata: s.Metadata}) {
						must = append(must, id)
					}
				}
				mu.Unlock()
				if err := checkWalk(e.l, f, limit, must); err != nil {
					errc <- err
					return
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
	if created.Load() < 300 || moved.Load() < 300 {
		t.Fatalf("writers did nothing: %d accounts, %d transfers", created.Load(), moved.Load())
	}
	t.Logf("%d walks during %d new accounts and %d transfers", walks.Load(), created.Load(), moved.Load())
	e.invariants()
	// After the writers stop, a walk returns exactly what a scan does.
	for _, f := range filters {
		if got := ids(walkAccounts(t, e.l, f, 17)); !slices.Equal(got, brute(known, f)) {
			t.Fatalf("filter %+v after writes: got %d accounts, want %d", f, len(got), len(brute(known, f)))
		}
	}
}

func checkWalk(l *Ledger, f AccountFilter, limit int, must []string) error {
	seen := map[string]bool{}
	after, prev, asOf := "", "", uint64(0)
	for {
		p, err := l.ListAccounts(f, after, limit)
		if err != nil {
			return err
		}
		if p.AsOfSeq < asOf {
			return fmt.Errorf("page as of %d after a page as of %d", p.AsOfSeq, asOf)
		}
		asOf = p.AsOfSeq
		for _, a := range p.Accounts {
			if a.ID <= prev {
				return fmt.Errorf("filter %+v: %q listed after %q", f, a.ID, prev)
			}
			prev = a.ID
			seen[a.ID] = true
			if !f.matches(&a.Account) {
				return fmt.Errorf("filter %+v: %s (%s %v) does not match", f, a.ID, a.Currency, a.Metadata)
			}
			one, err := l.Account(a.ID, p.AsOfSeq)
			if err != nil {
				return err
			}
			if one.Balance != a.Balance || one.Version != a.Version {
				return fmt.Errorf("%s listed as %+v v%d as of %d, read as %+v v%d", a.ID, a.Balance, a.Version, p.AsOfSeq, one.Balance, one.Version)
			}
		}
		if p.Next == "" {
			break
		}
		after = p.Next
	}
	for _, id := range must {
		if !seen[id] {
			return fmt.Errorf("filter %+v limit %d: %s existed before the walk and was not listed", f, limit, id)
		}
	}
	return nil
}

// BenchmarkListAccounts pages through 100,000 accounts: unfiltered, by
// currency, and by two filters that each match half the accounts but
// rarely together, which exercises the scan budget.
func BenchmarkListAccounts(b *testing.B) {
	e := newEnv(b, false)
	ctx := context.Background()
	for i := range 100_000 {
		meta := map[string]string{"tier": []string{"gold", "silver"}[i%2]}
		cur := []string{"USD", "EUR"}[i/2%2]
		if i%1000 == 0 {
			meta["tier"], cur = "gold", "EUR" // gold+EUR is otherwise rare
		}
		if cur == "EUR" && meta["tier"] == "gold" && i%1000 != 0 {
			cur = "USD"
		}
		if _, err := e.l.Do(ctx, Command{CreateAccount: &AccountSpec{ID: fmt.Sprintf("acct-%07d", i), Currency: cur, NormalBalance: Credit, Metadata: meta}}, nil); err != nil {
			b.Fatal(err)
		}
	}
	for name, f := range map[string]AccountFilter{
		"all":       {},
		"currency":  {Currency: "USD"},
		"rare-pair": {Currency: "EUR", Metadata: map[string]string{"tier": "gold"}},
	} {
		b.Run(name, func(b *testing.B) {
			after, n := "", 0
			for b.Loop() {
				p, err := e.l.ListAccounts(f, after, 100)
				if err != nil {
					b.Fatal(err)
				}
				n += len(p.Accounts)
				after = p.Next
			}
			b.ReportMetric(float64(n)/float64(b.N), "accounts/op")
		})
	}
}
