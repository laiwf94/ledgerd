package ledger

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"testing"
	"time"
)

// agingSummary is a report reduced to what tests compare: per bucket, the
// open count, the counts by kind and the USD total.
func agingSummary(rep *AgingReport) map[string]string {
	out := map[string]string{}
	for _, b := range rep.Buckets {
		if b.OpenBreaks == 0 {
			if len(b.Amounts) != 0 || b.Oldest != "" {
				out[b.Bucket] = fmt.Sprintf("empty bucket with %v %q", b.Amounts, b.Oldest)
			}
			continue
		}
		amt := ""
		for _, a := range b.Amounts {
			amt += " " + a.Currency + " " + a.Amount
		}
		out[b.Bucket] = fmt.Sprintf("%d (%d/%d/%d/%d)%s from %s", b.OpenBreaks, b.AmountMismatches, b.MissingInLedger,
			b.MissingInStatement, b.ReopenedMatches, amt, b.Oldest)
	}
	return out
}

func (f *reconFixture) aging(account, asOf string) *AgingReport {
	f.t.Helper()
	rep, err := f.l.ReconAging(account, asOf)
	if err != nil {
		f.t.Fatal(err)
	}
	return rep
}

func (f *reconFixture) wantAging(account, asOf string, want map[string]string) {
	f.t.Helper()
	rep := f.aging(account, asOf)
	got := agingSummary(rep)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		f.t.Fatalf("aging of %q as of %q:\n got %v\nwant %v", account, asOf, got, want)
	}
	n := 0
	for _, b := range rep.Buckets {
		n += b.OpenBreaks
	}
	if n != rep.OpenBreaks {
		f.t.Fatalf("total %d, buckets sum to %d", rep.OpenBreaks, n)
	}
}

// after is the date n days after the fixture's day d.
func after(f *reconFixture, d, n int) string { return day(f, d+n) }

func TestReconAging(t *testing.T) {
	f := newReconFixture(t, true)
	today := day(f, 2)
	rep := f.aging("", "")
	if rep.OpenBreaks != 0 || len(rep.Buckets) != 4 || rep.AsOf != day(f, 2) || rep.Account != "" {
		t.Fatalf("empty report %+v", rep)
	}
	for i, b := range rep.Buckets {
		spec := AgingBuckets[i]
		if b.Bucket != spec.Label || b.MinDays != spec.MinDays || (b.MaxDays == nil) != (spec.MaxDays < 0) ||
			b.MaxDays != nil && *b.MaxDays != spec.MaxDays || b.Amounts == nil {
			t.Fatalf("bucket %d %+v", i, b)
		}
	}

	// r1's breaks: an amount mismatch dated day 1 (off by 0.01), a line the
	// ledger lacks (0.88) and a movement the statement lacks (0.75), both
	// dated day 2, today.
	f.do(saveCmd(f.run("r1")))
	f.wantAging("", "", map[string]string{"0-2": "3 (1/1/1/0) USD 1.64 from " + day(f, 1)})
	f.wantAging("settle", day(f, 2), map[string]string{"0-2": "3 (1/1/1/0) USD 1.64 from " + day(f, 1)})
	// The edges of every bucket.
	f.wantAging("", after(f, 1, 2), map[string]string{"0-2": "3 (1/1/1/0) USD 1.64 from " + day(f, 1)})
	f.wantAging("", after(f, 1, 3), map[string]string{"0-2": "2 (0/1/1/0) USD 1.63 from " + day(f, 2),
		"3-7": "1 (1/0/0/0) USD 0.01 from " + day(f, 1)})
	f.wantAging("", after(f, 2, 7), map[string]string{"3-7": "2 (0/1/1/0) USD 1.63 from " + day(f, 2),
		"8-30": "1 (1/0/0/0) USD 0.01 from " + day(f, 1)})
	f.wantAging("", after(f, 2, 30), map[string]string{"8-30": "2 (0/1/1/0) USD 1.63 from " + day(f, 2),
		"30+": "1 (1/0/0/0) USD 0.01 from " + day(f, 1)})
	f.wantAging("", after(f, 2, 400), map[string]string{"30+": "3 (1/1/1/0) USD 1.64 from " + day(f, 1)})
	// Items dated after the report's day are 0 days old.
	f.wantAging("", "1970-01-01", map[string]string{"0-2": "3 (1/1/1/0) USD 1.64 from " + day(f, 1)})
	f.wantAging("eur", "", map[string]string{})

	// A reopened match is open, from its earliest date, for the line's
	// amount; confirming it again closes it.
	f.do(reopenCmd("r1", "wrong payment", 1))
	open4 := map[string]string{"0-2": "4 (1/1/1/1) USD 11.64 from " + day(f, 0)}
	f.wantAging("", "", open4)
	f.do(confirmCmd("r1", false, 1))
	f.wantAging("", "", map[string]string{"0-2": "3 (1/1/1/0) USD 1.64 from " + day(f, 1)})
	f.do(reopenCmd("r1", "", 1))
	f.wantAging("", "", open4)

	// A later run naming the same items replaces r1's verdict on them: run
	// again, the breaks count once and the reopened match, matched again by
	// r2, is closed.
	f.do(saveCmd(f.run("r2")))
	f.wantAging("", "", map[string]string{"0-2": "3 (1/1/1/0) USD 1.64 from " + day(f, 1)})
	// A third run that matches the withdrawal to a new line closes that
	// break and leaves the others to r2.
	out := f.posting("out")
	f.do(saveCmd(&ReconSpec{ID: "r3", Account: "settle", From: day(f, 2), To: day(f, 2), WindowDays: 0, AsOfSeq: f.l.Head(),
		Matches: []ReconMatchSpec{{Line: ReconLine{Row: 2, ExternalID: "b9", Date: day(f, 2), Amount: -75}, Postings: []ReconPosting{out}, By: ByAmountDate}}}))
	after3 := map[string]string{"0-2": "2 (1/1/0/0) USD 0.89 from " + day(f, 1)}
	f.wantAging("", "", after3)
	// Reopening that match makes the withdrawal open again, once.
	f.do(reopenCmd("r3", "", 1))
	f.wantAging("", "", map[string]string{"0-2": "3 (1/1/0/1) USD 1.64 from " + day(f, 1)})
	f.do(confirmCmd("r3", true))
	f.wantAging("", "", after3)
	f.invariants()

	// The gauges count as of the clock.
	if got := f.l.openBreaksByBucket(); fmt.Sprint(got) != "[2 0 0 0]" {
		t.Fatalf("gauges %v", got)
	}
	f.clock.Advance(30 * 24 * time.Hour)
	if got := f.l.openBreaksByBucket(); fmt.Sprint(got) != "[0 0 1 1]" {
		t.Fatalf("gauges a month later %v", got)
	}

	// Nothing is stored for the report: a restart, which loads the runs
	// from the log, gives the same one.
	f.restart()
	f.wantAging("", today, after3)

	for _, c := range []struct {
		account, asOf string
		code          Code
	}{
		{"nobody", "", CodeNotFound},
		{"bad id!", "", CodeInvalid},
		{"", "2026-02-30", CodeInvalid},
		{"", "1969-12-31", CodeInvalid},
		{"", "26-01-01", CodeInvalid},
	} {
		_, err := f.l.ReconAging(c.account, c.asOf)
		var le *Error
		if !errors.As(err, &le) || le.Code != c.code {
			t.Errorf("ReconAging(%q, %q) = %v, want %s", c.account, c.asOf, err, c.code)
		}
	}
}

// naiveOpen is the definition of an open item, computed the slow way: a
// break or reopened match of run i is open unless a run after i on the same
// account names its line or one of its movements.
func naiveOpen(runs []*reconRun) map[string]int64 {
	names := func(r *reconRun, acct, ext string, seq uint64) bool {
		s := &r.def.spec
		if s.Account != acct {
			return false
		}
		for _, m := range s.Matches {
			if ext != "" && m.Line.ExternalID == ext {
				return true
			}
			for _, p := range m.Postings {
				if seq != 0 && p.Seq == seq {
					return true
				}
			}
		}
		for _, b := range s.Breaks {
			if ext != "" && b.Line != nil && b.Line.ExternalID == ext || seq != 0 && b.Posting != nil && b.Posting.Seq == seq {
				return true
			}
		}
		return false
	}
	later := func(i int, acct, ext string, seq uint64) bool {
		for _, r := range runs[i+1:] {
			if names(r, acct, ext, seq) {
				return true
			}
		}
		return false
	}
	out := map[string]int64{}
	add := func(acct, kind string, date string, size *big.Int) {
		k := acct + " " + date + " " + kind
		out[k+" n"]++
		cur := big.NewInt(out[k+" amt"])
		out[k+" amt"] = cur.Add(cur, size).Int64() // tests keep sums small
	}
	abs := func(v int64) *big.Int { return new(big.Int).Abs(big.NewInt(v)) }
	for i, r := range runs {
		s := &r.def.spec
		for _, b := range s.Breaks {
			ext, seq := "", uint64(0)
			if b.Line != nil {
				ext = b.Line.ExternalID
			}
			if b.Posting != nil {
				seq = b.Posting.Seq
			}
			if ext != "" && later(i, s.Account, ext, 0) || seq != 0 && later(i, s.Account, "", seq) {
				continue
			}
			switch b.Kind {
			case BreakAmountMismatch:
				d := min(b.Line.Date, b.Posting.Date)
				add(s.Account, "mismatch", d, new(big.Int).Abs(new(big.Int).Sub(big.NewInt(b.Line.Amount), big.NewInt(b.Posting.Amount))))
			case BreakMissingInLedger:
				add(s.Account, "ledger", b.Line.Date, abs(b.Line.Amount))
			case BreakMissingInStatement:
				add(s.Account, "statement", b.Posting.Date, abs(b.Posting.Amount))
			}
		}
		for k, m := range s.Matches {
			if r.state(k).status != MatchReopened || later(i, s.Account, m.Line.ExternalID, 0) {
				continue
			}
			d, closed := m.Line.Date, false
			for _, p := range m.Postings {
				closed = closed || later(i, s.Account, "", p.Seq)
				d = min(d, p.Date)
			}
			if !closed {
				add(s.Account, "reopened", d, abs(m.Line.Amount))
			}
		}
	}
	return out
}

func groupsMap(groups []agingGroup) map[string]int64 {
	kinds := []string{"mismatch", "ledger", "statement", "reopened"}
	out := map[string]int64{}
	for _, g := range groups {
		k := g.account + " " + time.Unix(g.day*86400, 0).UTC().Format(reconDateLayout) + " " + kinds[g.kind]
		out[k+" n"] += int64(g.count)
		out[k+" amt"] += int64(g.lo)
	}
	return out
}

// randomRuns makes runs over a few accounts that share a small pool of
// statement lines and movements, so later runs often name earlier items.
// Within a run every line and movement appears once, as reconProblem
// requires; the journal itself plays no part in aging.
func randomRuns(rng *rand.Rand, n int) []*reconRun {
	accounts := []string{"a", "b", "c"}
	date := func() string {
		return time.Unix(int64(rng.IntN(60))*86400+1_700_000_000, 0).UTC().Format(reconDateLayout)
	}
	amount := func() int64 {
		v := rng.Int64N(10_000) + 1
		if rng.IntN(4) == 0 {
			v = -v
		}
		return v
	}
	var runs []*reconRun
	for i := range n {
		acct := accounts[rng.IntN(len(accounts))]
		lines := rng.Perm(30)
		seqs := rng.Perm(30)
		nextLine := func() ReconLine {
			id := lines[0]
			lines = lines[1:]
			return ReconLine{Row: 2, ExternalID: fmt.Sprintf("x%d", id), Date: date(), Amount: amount()}
		}
		nextPost := func() ReconPosting {
			seq := seqs[0]
			seqs = seqs[1:]
			return ReconPosting{Seq: uint64(seq) + 1, TransferID: "t", Date: date(), Amount: amount()}
		}
		s := ReconSpec{ID: fmt.Sprintf("r%d", i), Account: acct}
		for range rng.IntN(6) {
			m := ReconMatchSpec{Line: nextLine(), By: ByAmountDate}
			for range 1 + rng.IntN(3) {
				m.Postings = append(m.Postings, nextPost())
			}
			s.Matches = append(s.Matches, m)
		}
		for range rng.IntN(6) {
			switch rng.IntN(3) {
			case 0:
				ln, p := nextLine(), nextPost()
				s.Breaks = append(s.Breaks, ReconBreakSpec{Kind: BreakAmountMismatch, Line: &ln, Posting: &p})
			case 1:
				ln := nextLine()
				s.Breaks = append(s.Breaks, ReconBreakSpec{Kind: BreakMissingInLedger, Line: &ln})
			default:
				p := nextPost()
				s.Breaks = append(s.Breaks, ReconBreakSpec{Kind: BreakMissingInStatement, Posting: &p})
			}
		}
		r := &reconRun{def: newReconDef(&s, uint64(i)+1, time.Time{}, "USD")}
		if len(s.Matches) > 0 && rng.IntN(2) == 0 {
			r.status = make([]matchState, len(s.Matches))
			for k := range r.status {
				switch rng.IntN(3) {
				case 0:
					r.status[k] = matchState{status: MatchReopened}
				case 1:
					r.status[k] = matchState{status: MatchConfirmed}
				}
			}
		}
		runs = append(runs, r)
	}
	return runs
}

func TestOpenGroupsMatchDefinition(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 28))
	for trial := range 300 {
		runs := randomRuns(rng, 1+rng.IntN(12))
		got, want := groupsMap(openGroups(runs)), naiveOpen(runs)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("trial %d:\n got %v\nwant %v", trial, got, want)
		}
		// The report adds up the groups, whatever day it is taken on.
		groups := openGroups(runs)
		total := 0
		for _, g := range groups {
			total += g.count
		}
		for _, today := range []int64{0, 19_675, 19_700, 19_800} {
			rep := agingReport(groups, "", "x", today)
			if rep.OpenBreaks != total {
				t.Fatalf("trial %d: report counts %d of %d", trial, rep.OpenBreaks, total)
			}
		}
	}
}

func TestAgingWideSums(t *testing.T) {
	// Two of the largest movements overflow int64 but not the 128-bit sum.
	groups := []agingGroup{
		{account: "a", currency: "USD", day: 10, kind: openMissingInStatement, count: 1, lo: math.MaxInt64},
		{account: "a", currency: "USD", day: 10, kind: openMissingInLedger, count: 1, lo: 1 << 63},
		{account: "a", currency: "JPY", day: 10, kind: openMismatch, count: 1, lo: math.MaxUint64},
		{account: "a", currency: "JPY", day: 11, kind: openMismatch, count: 1, lo: 2},
	}
	rep := agingReport(groups, "", "1970-01-12", 11)
	got := fmt.Sprint(rep.Buckets[0].Amounts)
	if want := "[{JPY 18446744073709551617} {USD 184467440737095516.15}]"; got != want {
		t.Fatalf("amounts %s, want %s", got, want)
	}
	if difference(math.MinInt64, math.MaxInt64) != math.MaxUint64 || difference(-5, -7) != 2 || magnitude(math.MinInt64) != 1<<63 {
		t.Fatal("difference or magnitude overflows")
	}
}

// BenchmarkOpenGroups rebuilds the summary for 200 runs of 10,000 items,
// the most saved runs may hold together (MaxReconItems), and then takes
// the report from it.
func BenchmarkOpenGroups(b *testing.B) {
	runs := make([]*reconRun, 200)
	for i := range runs {
		s := ReconSpec{ID: fmt.Sprint(i), Account: fmt.Sprintf("acct%d", i%20)}
		for k := range 10_000 {
			ln := ReconLine{Row: 2, ExternalID: fmt.Sprintf("l%d", i*10_000+k), Date: time.Unix(int64(k%365)*86400, 0).UTC().Format(reconDateLayout), Amount: int64(k + 1)}
			s.Breaks = append(s.Breaks, ReconBreakSpec{Kind: BreakMissingInLedger, Line: &ln})
		}
		runs[i] = &reconRun{def: newReconDef(&s, uint64(i)+1, time.Time{}, "USD")}
	}
	b.Run("rebuild", func(b *testing.B) {
		for b.Loop() {
			openGroups(runs)
		}
	})
	groups := openGroups(runs)
	b.Run("report", func(b *testing.B) {
		for b.Loop() {
			agingReport(groups, "", "x", 400)
		}
	})
}

func TestItemDayMatchesTime(t *testing.T) {
	for d := int64(0); d < 200_000; d += 7 { // 1970 to past 2500, every weekday in turn
		date := time.Unix(d*86400, 0).UTC().Format(reconDateLayout)
		want, ok := reconDay(date)
		if got := itemDay(date); !ok || got != want || got != d {
			t.Fatalf("%s: itemDay %d, reconDay %d %v, want %d", date, got, want, ok, d)
		}
	}
	for _, date := range []string{"2000-02-29", "2100-03-01", "2024-12-31", "1970-01-01"} {
		if want, _ := reconDay(date); itemDay(date) != want {
			t.Fatalf("%s: itemDay %d, want %d", date, itemDay(date), want)
		}
	}
}
