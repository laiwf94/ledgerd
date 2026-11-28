package recon

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/money"
)

func pst(seq uint64, date string, amt int64, ref string) Posting {
	return Posting{Seq: seq, Time: at(date), TransferID: fmt.Sprint("t", seq), Reference: ref, Amount: amt}
}

func byRow(rep Report) map[int]Match {
	out := map[int]Match{}
	for _, m := range rep.Matched {
		out[m.Row] = m
	}
	return out
}

// A reference line within the tolerance of a movement with its reference
// is a labelled near-match carrying the difference, not an amount mismatch;
// past the tolerance, or with two such movements, it is still a mismatch.
func TestNearByReference(t *testing.T) {
	lines := []Line{
		{Row: 2, ExternalID: "fee", Date: "2030-03-01", Amount: 10_000, Reference: "INV-1"}, // 100.00 less a 0.30 fee
		{Row: 3, ExternalID: "far", Date: "2030-03-01", Amount: 5_000, Reference: "INV-2"},  // 1.00 off
		{Row: 4, ExternalID: "two", Date: "2030-03-01", Amount: 2_000, Reference: "INV-3"},  // two movements fit
		{Row: 5, ExternalID: "r1", Date: "2030-03-01", Amount: 3_000, Reference: "INV-4"},   // two lines fit one movement
		{Row: 6, ExternalID: "r2", Date: "2030-03-02", Amount: 3_008, Reference: "INV-4"},
	}
	ps := []Posting{
		pst(1, "2030-02-20", 10_030, "INV-1"),
		pst(2, "2030-03-01", 5_100, "INV-2"),
		pst(3, "2030-03-01", 1_990, "INV-3"),
		pst(4, "2030-03-01", 2_010, "INV-3"),
		pst(5, "2030-03-01", 3_005, "INV-4"),
	}
	rep := ReconcileWith(lines, ps, day0("2030-03-01"), day0("2030-03-02"), Options{WindowDays: 0, Tolerance: 50})
	m := byRow(rep)
	if got := m[2]; got.By != ByReferenceNear || got.Seq != 1 || got.Difference != -30 || got.Amount != 10_030 || got.DaysApart != 9 {
		t.Fatalf("fee %+v", got)
	}
	if len(m) != 1 || len(rep.AmountMismatches) != 3 || rep.AmountMismatches[0].Row != 3 || rep.AmountMismatches[1].Row != 4 {
		t.Fatalf("matches %+v mismatches %+v", rep.Matched, rep.AmountMismatches)
	}
	// Rows 4, 5 and 6 had candidates but no unique one; row 3 had none.
	if rep.Summary.NearMatches != 1 || rep.Summary.NearAmbiguous != 3 || rep.Tolerance != 50 || rep.ToleranceBP != 0 {
		t.Fatalf("summary %+v tolerance %d %d", rep.Summary, rep.Tolerance, rep.ToleranceBP)
	}
	// 2% of 50.00 is 1.00: the larger tolerance applies.
	rep = ReconcileWith(lines, ps, day0("2030-03-01"), day0("2030-03-02"), Options{Tolerance: 50, ToleranceBP: 200})
	if got := byRow(rep)[3]; got.By != ByReferenceNear || got.Difference != -100 {
		t.Fatalf("percentage %+v", got)
	}
	// Without a tolerance nothing is near.
	rep = Reconcile(lines, ps, day0("2030-03-01"), day0("2030-03-02"), 0)
	if rep.Summary.NearMatches != 0 || rep.Summary.NearAmbiguous != 0 || rep.Summary.AmountMismatches != 4 {
		t.Fatalf("exact only %+v", rep.Summary)
	}
}

func TestNearByAmountAndDate(t *testing.T) {
	lines := []Line{
		{Row: 2, ExternalID: "a", Date: "2030-03-10", Amount: 1_000},
		{Row: 3, ExternalID: "two", Date: "2030-03-10", Amount: 5_000},
		{Row: 4, ExternalID: "out", Date: "2030-03-10", Amount: -700},
		{Row: 5, ExternalID: "none", Date: "2030-03-10", Amount: 9_000},
		{Row: 6, ExternalID: "r1", Date: "2030-03-10", Amount: 3_000},
		{Row: 7, ExternalID: "r2", Date: "2030-03-11", Amount: 3_030},
	}
	ps := []Posting{
		pst(1, "2030-03-12", 1_003, ""),
		pst(2, "2030-03-30", 1_000, ""), // exact but outside the window
		pst(3, "2030-03-12", 5_020, ""),
		pst(4, "2030-03-11", 4_980, ""),
		pst(5, "2030-03-10", 702, ""), // the other way from the line
		pst(6, "2030-03-10", -705, ""),
		pst(7, "2030-03-10", 8_000, ""), // too far
		pst(8, "2030-03-10", 3_010, ""), // fits both r1 and r2
	}
	rep := ReconcileWith(lines, ps, day0("2030-03-10"), day0("2030-03-12"), Options{WindowDays: 2, ToleranceBP: 100})
	m := byRow(rep)
	if got := m[2]; got.By != ByAmountDateNear || got.Seq != 1 || got.Difference != -3 || got.DaysApart != 2 {
		t.Fatalf("near %+v", got)
	}
	if got := m[4]; got.Seq != 6 || got.Difference != 5 {
		t.Fatalf("outgoing %+v", got)
	}
	if len(m) != 2 || rep.Summary.NearMatches != 2 || rep.Summary.NearAmbiguous != 3 || rep.Summary.MissingInLedger != 4 {
		t.Fatalf("matches %+v summary %+v", rep.Matched, rep.Summary)
	}
	// With a tighter tolerance the movement fits only r1: 0.5% of 30.00 is
	// 0.15, enough for r1's 0.10, and r2 is 0.20 off.
	rep = ReconcileWith(lines, ps, day0("2030-03-10"), day0("2030-03-12"), Options{WindowDays: 2, ToleranceBP: 50})
	if got := byRow(rep)[6]; got.Seq != 8 || got.Difference != -10 {
		t.Fatalf("r1 %+v", got)
	}
}

// Exact matches and batches come first, so a tolerance never takes a
// movement some line matches exactly or a batch needs.
func TestNearComesLast(t *testing.T) {
	lines := []Line{
		{Row: 2, ExternalID: "a", Date: "2030-03-10", Amount: 1_001},
		{Row: 3, ExternalID: "b", Date: "2030-03-10", Amount: 1_000},
		{Row: 4, ExternalID: "batch", Date: "2030-03-10", Amount: 300, Reference: "PO"},
		{Row: 5, ExternalID: "c", Date: "2030-03-10", Amount: 205},
	}
	ps := []Posting{
		pst(1, "2030-03-10", 1_000, ""),
		pst(2, "2030-03-10", 1_001, ""),
		pst(3, "2030-03-10", 100, "PO"),
		pst(4, "2030-03-10", 200, "PO"),
		pst(5, "2030-03-10", 299, "PO"),
		pst(6, "2030-03-10", 210, ""),
	}
	rep := ReconcileWith(lines, ps, day0("2030-03-10"), day0("2030-03-10"), Options{WindowDays: 1, Tolerance: 5})
	m := byRow(rep)
	if m[2].By != ByAmountDate || m[2].Seq != 2 || m[3].By != ByAmountDate || m[3].Seq != 1 {
		t.Fatalf("exact %+v %+v", m[2], m[3])
	}
	// 299 is within the tolerance of the batch's line, but the exact batch
	// 100 + 200 comes first.
	if got := m[4]; got.By != ByReferenceBatch || !reflect.DeepEqual(seqs(got), []uint64{3, 4}) {
		t.Fatalf("batch %+v", got)
	}
	if got := m[5]; got.By != ByAmountDateNear || got.Seq != 6 || got.Difference != -5 {
		t.Fatalf("near %+v", got)
	}
	if len(rep.MissingInStatement) != 1 || rep.MissingInStatement[0].Seq != 5 {
		t.Fatalf("missing %+v", rep.MissingInStatement)
	}
}

// Random statements against brute force: every near-match moves the line's
// way within the tolerance (and the window, or under the line's
// reference), nothing is used twice, and among the lines left open and the
// movements left free no line and movement are each other's only
// candidate, so none was missed. Shuffling the inputs never changes the
// report, and with no tolerance the report is the exact one.
func TestNearMatchesAreSoundAndComplete(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	base := day0("2030-01-01")
	near := 0
	for iter := 0; iter < 3000; iter++ {
		var lines []Line
		var ps []Posting
		amt := func() int64 {
			v := int64(50 + r.Intn(60))
			if r.Intn(5) == 0 {
				v = -v
			}
			return v
		}
		ref := func() string {
			if r.Intn(3) == 0 {
				return fmt.Sprint("r", r.Intn(3))
			}
			return ""
		}
		for i := 0; i < 1+r.Intn(12); i++ {
			lines = append(lines, Line{Row: i + 2, ExternalID: fmt.Sprint(i), Date: base.AddDate(0, 0, r.Intn(8)).Format(dateLayout), Amount: amt(), Reference: ref()})
		}
		for i := 0; i < r.Intn(20); i++ {
			ps = append(ps, Posting{Seq: uint64(i + 1), Time: base.AddDate(0, 0, r.Intn(8)), TransferID: fmt.Sprint("t", i), Reference: ref(), Amount: amt()})
		}
		opt := Options{WindowDays: r.Intn(3), Tolerance: int64(r.Intn(6)), ToleranceBP: int64(r.Intn(3) * 100)}
		from, to := base, base.AddDate(0, 0, 7)
		rep := ReconcileWith(lines, ps, from, to, opt)
		r.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
		r.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
		if again := ReconcileWith(lines, ps, from, to, opt); !reflect.DeepEqual(again, rep) {
			t.Fatalf("iteration %d: order changed the report\n%+v\n%+v", iter, rep, again)
		}
		if exact, plain := ReconcileWith(lines, ps, from, to, Options{WindowDays: opt.WindowDays}), Reconcile(lines, ps, from, to, opt.WindowDays); !reflect.DeepEqual(exact, plain) {
			t.Fatalf("iteration %d: zero tolerance differs from exact matching", iter)
		}
		if rep.Summary.NearSearchesCut != 0 {
			t.Fatalf("iteration %d: a small search was cut", iter)
		}
		bySeq := map[uint64]Posting{}
		for _, p := range ps {
			bySeq[p.Seq] = p
		}
		lineOf := map[int]Line{}
		for _, ln := range lines {
			lineOf[ln.Row] = ln
		}
		usedP, usedL := map[uint64]bool{}, map[int]bool{}
		use := func(seq uint64) {
			if usedP[seq] {
				t.Fatalf("iteration %d: posting %d used twice", iter, seq)
			}
			usedP[seq] = true
		}
		dayOf := func(date string) int64 { d, _ := time.Parse(dateLayout, date); return day(d) }
		fits := func(ln Line, p Posting) bool {
			return (p.Amount > 0) == (ln.Amount > 0) && abs(ln.Amount-p.Amount) <= money.Tolerance(ln.Amount, opt.Tolerance, opt.ToleranceBP)
		}
		inWindow := func(ln Line, p Posting) bool { return abs(dayOf(ln.Date)-day(p.Time)) <= int64(opt.WindowDays) }
		nearHere := 0
		for _, m := range rep.Matched {
			usedL[m.Row] = true
			ln := lineOf[m.Row]
			if m.Postings != nil {
				for _, p := range m.Postings {
					use(p.Seq)
				}
				continue
			}
			use(m.Seq)
			p := bySeq[m.Seq]
			switch m.By {
			case ByReference, ByAmountDate:
				if p.Amount != ln.Amount || m.Difference != 0 {
					t.Fatalf("iteration %d: exact match %+v of %d", iter, m, p.Amount)
				}
			case ByReferenceNear, ByAmountDateNear:
				nearHere++
				d := ln.Amount - p.Amount
				if d == 0 || d != m.Difference || !fits(ln, p) {
					t.Fatalf("iteration %d: near-match %+v of %d, tolerance %+v", iter, m, p.Amount, opt)
				}
				if m.By == ByReferenceNear && (ln.Reference == "" || p.Reference != ln.Reference) {
					t.Fatalf("iteration %d: reference near-match %+v", iter, m)
				}
				if m.By == ByAmountDateNear && !inWindow(ln, p) {
					t.Fatalf("iteration %d: near-match outside the window %+v", iter, m)
				}
			}
		}
		for _, mm := range rep.AmountMismatches {
			usedL[mm.Row] = true
			use(mm.Seq)
		}
		if nearHere != rep.Summary.NearMatches {
			t.Fatalf("iteration %d: %d near-matches, summary says %d", iter, nearHere, rep.Summary.NearMatches)
		}
		near += nearHere
		// Complete: no open line and free movement within the tolerance
		// and the window are each other's only candidate.
		cands := map[int][]uint64{}
		lineCount := map[uint64]int{}
		for _, ln := range lines {
			if usedL[ln.Row] || ln.Amount == 0 {
				continue
			}
			for _, p := range ps {
				if !usedP[p.Seq] && fits(ln, p) && inWindow(ln, p) {
					cands[ln.Row] = append(cands[ln.Row], p.Seq)
					lineCount[p.Seq]++
				}
			}
		}
		for row, cs := range cands {
			if len(cs) == 1 && lineCount[cs[0]] == 1 && (opt.Tolerance > 0 || opt.ToleranceBP > 0) {
				t.Fatalf("iteration %d: line %+v and posting %+v are each other's only candidate, tolerance %+v\n%+v", iter, lineOf[row], bySeq[cs[0]], opt, rep)
			}
		}
	}
	if near < 500 {
		t.Fatalf("only %d near-matches in all", near)
	}
}

// Extreme amounts and tolerances neither overflow nor match across zero.
func TestNearOverflow(t *testing.T) {
	huge := Options{Tolerance: math.MaxInt64, ToleranceBP: MaxToleranceBasisPoints}
	lines := []Line{
		{Row: 2, ExternalID: "max", Date: "2030-03-01", Amount: math.MaxInt64},
		{Row: 3, ExternalID: "min", Date: "2030-03-01", Amount: math.MinInt64, Reference: "R"},
	}
	ps := []Posting{
		pst(1, "2030-03-01", math.MinInt64, ""),
		pst(2, "2030-03-01", math.MaxInt64-1, ""),
		pst(3, "2030-03-01", math.MinInt64+1, "R"),
	}
	rep := ReconcileWith(lines, ps, day0("2030-03-01"), day0("2030-03-01"), huge)
	m := byRow(rep)
	if m[2].Seq != 2 || m[2].Difference != 1 || m[3].Seq != 3 || m[3].Difference != -1 {
		t.Fatalf("extremes %+v %+v", m[2], m[3])
	}
	rep = ReconcileWith([]Line{{Row: 2, ExternalID: "one", Date: "2030-03-01", Amount: 1}}, []Posting{pst(1, "2030-03-01", -1, "")},
		day0("2030-03-01"), day0("2030-03-01"), huge)
	if len(rep.Matched) != 0 || rep.Summary.NearAmbiguous != 0 {
		t.Fatalf("1 matched across zero: %+v", rep)
	}
	for _, a := range []int64{1, 9_999, math.MaxInt64, math.MinInt64, -77} {
		for _, o := range []Options{{ToleranceBP: 1}, {ToleranceBP: MaxToleranceBasisPoints}, {Tolerance: 5, ToleranceBP: 300}, {Tolerance: 7}} {
			// reach bounds every line whose tolerance can take a.
			d := reach(a, o)
			for _, l := range []int64{satAdd(a, d), satSub(a, d), satAdd(a, d+1), satSub(a, d+1)} {
				if (l > 0) == (a > 0) && abs(l-a) > d && abs(l-a) <= money.Tolerance(l, o.Tolerance, o.ToleranceBP) && abs(l-a) > 0 {
					t.Fatalf("reach(%d, %+v) = %d, but a line of %d takes it", a, o, d, l)
				}
			}
		}
	}
}

// The worst statements for near-matching. By reference, every line has
// 100,000 movements within its tolerance: each search stops at its second
// candidate. By amount and date, every line has 100,000 distinct amounts
// within its tolerance, all outside its window: each search stops at its
// step limit and the whole run at its total. Both stay fast, and every
// line is counted.
func TestNearWorstCase(t *testing.T) {
	const nLines, nPostings = 10_000, 100_000
	lines := make([]Line, nLines)
	for i := range lines {
		lines[i] = Line{Row: i + 2, ExternalID: fmt.Sprint(i), Date: "2030-01-01", Amount: 10_000_000, Reference: "R"}
	}
	ps := make([]Posting, nPostings)
	for i := range ps {
		ps[i] = Posting{Seq: uint64(i + 1), Time: at("2030-03-01"), TransferID: fmt.Sprint("t", i), Reference: "R", Amount: int64(9_000_001 + 2*i)}
	}
	opt := Options{WindowDays: MaxWindowDays, ToleranceBP: MaxToleranceBasisPoints}
	start := time.Now()
	rep := ReconcileWith(lines, ps, day0("2030-01-01"), day0("2030-01-01"), opt)
	t.Logf("by reference: %v", time.Since(start))
	if rep.Summary.NearMatches != 0 || rep.Summary.NearAmbiguous != nLines || rep.Summary.AmountMismatches != nLines {
		t.Fatalf("summary %+v", rep.Summary)
	}
	for i := range lines {
		lines[i].Reference = ""
	}
	start = time.Now()
	rep = ReconcileWith(lines, ps, day0("2030-01-01"), day0("2030-01-01"), opt)
	t.Logf("by amount and date: %v", time.Since(start))
	if rep.Summary.NearMatches != 0 || rep.Summary.NearSearchesCut != nLines || rep.Summary.MissingInLedger != nLines {
		t.Fatalf("summary %+v", rep.Summary)
	}
}

// BenchmarkNearMatches reconciles a month of 5,000 card settlements by
// amount and date, each paid out less a small difference: rounding of up to
// 2 cents with a 2-cent tolerance, or a fee of up to 0.49% with a 0.5%
// tolerance. It reports how many lines matched near, how many stayed open
// and how many were paired with another settlement's movement: a wide
// tolerance among dense amounts has several candidates per line, and the
// nearest is not always the right one.
func BenchmarkNearMatches(b *testing.B) {
	for _, tc := range []struct {
		name string
		diff func(r *rand.Rand, amt int64) int64
		opt  Options
	}{
		{"rounding", func(r *rand.Rand, amt int64) int64 { return int64(1 + r.Intn(2)) }, Options{WindowDays: 3, Tolerance: 2}},
		{"fees", func(r *rand.Rand, amt int64) int64 { return amt * int64(r.Intn(50)) / 10_000 }, Options{WindowDays: 3, ToleranceBP: 50}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := rand.New(rand.NewSource(1))
			var lines []Line
			var ps []Posting
			for i := 0; i < 5000; i++ {
				d := day0("2030-01-01").AddDate(0, 0, r.Intn(30))
				amt := int64(1_000 + r.Intn(500_000))
				ps = append(ps, Posting{Seq: uint64(i + 1), Time: d.Add(time.Hour), TransferID: fmt.Sprint("t", i), Amount: amt})
				lines = append(lines, Line{Row: i + 2, ExternalID: fmt.Sprint(i), Date: d.AddDate(0, 0, r.Intn(3)).Format(dateLayout), Amount: amt - tc.diff(r, amt)})
			}
			b.ReportAllocs()
			var rep Report
			for b.Loop() {
				rep = ReconcileWith(lines, ps, day0("2030-01-01"), day0("2030-01-30"), tc.opt)
			}
			wrong := 0
			for _, m := range rep.Matched {
				if m.Postings == nil && m.Seq != uint64(m.Row-1) { // line i+2 came from movement i+1
					wrong++
				}
			}
			b.ReportMetric(float64(rep.Summary.NearMatches), "near")
			b.ReportMetric(float64(rep.Summary.MissingInLedger), "open")
			b.ReportMetric(float64(wrong), "wrong")
		})
	}
}

// A zero movement moves neither way, so it is never near a line: not by
// reference, where it is still an amount mismatch, and not by amount and
// date.
func TestNearIgnoresZeroMovements(t *testing.T) {
	lines := []Line{
		{Row: 2, ExternalID: "a", Date: "2030-01-02", Amount: -2, Reference: "r"},
		{Row: 3, ExternalID: "b", Date: "2030-01-02", Amount: -2},
	}
	ps := []Posting{pst(1, "2030-01-02", 0, "r"), pst(2, "2030-01-02", 0, "")}
	rep := ReconcileWith(lines, ps, day0("2030-01-02"), day0("2030-01-02"), Options{WindowDays: 1, Tolerance: 5})
	if len(rep.Matched) != 0 || len(rep.AmountMismatches) != 1 || rep.AmountMismatches[0].Seq != 1 ||
		len(rep.MissingInLedger) != 1 || rep.MissingInLedger[0].Row != 3 {
		t.Fatalf("zero movements: %+v", rep)
	}
}
