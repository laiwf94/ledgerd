package recon

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

func seqs(m Match) []uint64 {
	var out []uint64
	for _, p := range m.Postings {
		out = append(out, p.Seq)
	}
	return out
}

func TestBatchByReference(t *testing.T) {
	d := "2030-03-10"
	lines := []Line{
		{Row: 2, ExternalID: "po", Date: d, Amount: -1500, Currency: "USD", Reference: "PO-7"},
		{Row: 3, ExternalID: "one", Date: d, Amount: 100, Currency: "USD", Reference: "R"},
		{Row: 4, ExternalID: "mm", Date: d, Amount: 50, Currency: "USD", Reference: "M"},
	}
	ps := []Posting{
		{Seq: 1, Time: at("2030-03-01"), TransferID: "p1", Reference: "PO-7", Amount: -500},
		{Seq: 2, Time: at("2030-03-02"), TransferID: "p2", Reference: "PO-7", Amount: -999}, // no subset with it
		{Seq: 3, Time: at("2030-03-03"), TransferID: "p3", Reference: "PO-7", Amount: -700},
		{Seq: 4, Time: at("2030-03-04"), TransferID: "p4", Reference: "PO-7", Amount: -300},
		{Seq: 5, Time: at("2030-03-04"), TransferID: "p5", Reference: "PO-7", Amount: 200}, // wrong sign
		{Seq: 6, Time: at(d), TransferID: "r1", Reference: "R", Amount: 60},
		{Seq: 7, Time: at(d), TransferID: "r2", Reference: "R", Amount: 100}, // exact beats the 60+40 batch
		{Seq: 8, Time: at(d), TransferID: "r3", Reference: "R", Amount: 40},
		{Seq: 9, Time: at(d), TransferID: "m1", Reference: "M", Amount: 20}, // 20+20 is not 50: a mismatch
		{Seq: 10, Time: at(d), TransferID: "m2", Reference: "M", Amount: 20},
	}
	rep := Reconcile(lines, ps, day0("2030-03-01"), day0(d), 0)
	if len(rep.Matched) != 2 {
		t.Fatalf("matched %+v", rep.Matched)
	}
	b := rep.Matched[0]
	if b.By != ByReferenceBatch || b.Amount != -1500 || b.TransferID != "" || b.Seq != 0 ||
		!reflect.DeepEqual(seqs(b), []uint64{1, 3, 4}) || b.DaysApart != 9 {
		t.Fatalf("batch %+v", b)
	}
	if b.Postings[0] != (Part{Seq: 1, TransferID: "p1", Date: "2030-03-01", Amount: -500}) {
		t.Fatalf("part %+v", b.Postings[0])
	}
	if m := rep.Matched[1]; m.By != ByReference || m.Seq != 7 || m.Postings != nil {
		t.Fatalf("exact %+v", m)
	}
	if len(rep.AmountMismatches) != 1 || rep.AmountMismatches[0].Seq != 9 {
		t.Fatalf("mismatches %+v", rep.AmountMismatches)
	}
	if rep.Summary.BatchMatches != 1 || rep.Summary.BatchSearchesCut != 0 || rep.Summary.MissingInStatement != 5 {
		t.Fatalf("summary %+v", rep.Summary)
	}
}

// A batch never takes a movement another line matches exactly, whatever
// the lines' order.
func TestBatchLeavesExactMatches(t *testing.T) {
	lines := []Line{
		{Row: 2, ExternalID: "early", Date: "2030-03-01", Amount: 300, Reference: "R"},
		{Row: 3, ExternalID: "late", Date: "2030-03-05", Amount: 100, Reference: "R"},
	}
	ps := []Posting{
		{Seq: 1, Time: at("2030-03-01"), TransferID: "a", Reference: "R", Amount: 100},
		{Seq: 2, Time: at("2030-03-01"), TransferID: "b", Reference: "R", Amount: 200},
	}
	rep := Reconcile(lines, ps, day0("2030-03-01"), day0("2030-03-05"), 0)
	if len(rep.Matched) != 1 || rep.Matched[0].ExternalID != "late" || rep.Matched[0].By != ByReference {
		t.Fatalf("matched %+v", rep.Matched)
	}
	if len(rep.AmountMismatches) != 1 || rep.AmountMismatches[0].ExternalID != "early" || rep.AmountMismatches[0].Seq != 2 {
		t.Fatalf("mismatches %+v", rep.AmountMismatches)
	}
}

func TestBatchByAmountAndDate(t *testing.T) {
	lines := []Line{
		{Row: 2, ExternalID: "settle", Date: "2030-03-10", Amount: 1000},
		{Row: 3, ExternalID: "single", Date: "2030-03-10", Amount: 250},
		{Row: 4, ExternalID: "neg", Date: "2030-03-10", Amount: -90},
	}
	ps := []Posting{
		{Seq: 1, Time: at("2030-03-09"), TransferID: "a", Amount: 600},
		{Seq: 2, Time: at("2030-03-11"), TransferID: "b", Amount: 400},
		{Seq: 3, Time: at("2030-03-10"), TransferID: "c", Amount: 250}, // one-to-one first
		{Seq: 4, Time: at("2030-03-20"), TransferID: "d", Amount: 150}, // outside the window
		{Seq: 5, Time: at("2030-03-10"), TransferID: "e", Amount: 90},  // wrong sign for neg
		{Seq: 6, Time: at("2030-03-10"), TransferID: "f", Amount: -40},
		{Seq: 7, Time: at("2030-03-14"), TransferID: "g", Amount: -50}, // outside the window
	}
	rep := Reconcile(lines, ps, day0("2030-03-09"), day0("2030-03-20"), 2)
	if len(rep.Matched) != 2 {
		t.Fatalf("matched %+v", rep.Matched)
	}
	if m := rep.Matched[0]; m.By != ByAmountDateBatch || !reflect.DeepEqual(seqs(m), []uint64{1, 2}) || m.DaysApart != 1 {
		t.Fatalf("batch %+v", m)
	}
	if m := rep.Matched[1]; m.By != ByAmountDate || m.Seq != 3 {
		t.Fatalf("single %+v", m)
	}
	if len(rep.MissingInLedger) != 1 || rep.MissingInLedger[0].ExternalID != "neg" || rep.Summary.MissingInStatement != 4 {
		t.Fatalf("report %+v", rep)
	}
	// A postings taken by one batch is free for no other: two equal lines,
	// enough movements for one.
	rep = Reconcile([]Line{{Row: 2, ExternalID: "x", Date: "2030-03-10", Amount: 30}, {Row: 3, ExternalID: "y", Date: "2030-03-10", Amount: 30}},
		[]Posting{{Seq: 1, Time: at("2030-03-10"), Amount: 10}, {Seq: 2, Time: at("2030-03-10"), Amount: 20}, {Seq: 3, Time: at("2030-03-10"), Amount: 5}},
		day0("2030-03-10"), day0("2030-03-10"), 0)
	if len(rep.Matched) != 1 || rep.Matched[0].ExternalID != "x" || len(rep.MissingInLedger) != 1 || len(rep.MissingInStatement) != 1 {
		t.Fatalf("shared postings %+v", rep)
	}
}

// The fast path takes every candidate when they sum to the line, up to
// MaxBatchPostings; past that there are too many candidates to search.
func TestBatchSize(t *testing.T) {
	for _, n := range []int{MaxBatchPostings, MaxBatchPostings + 1} {
		var ps []Posting
		for i := 0; i < n; i++ {
			ps = append(ps, Posting{Seq: uint64(i + 1), Time: at("2030-03-01"), TransferID: fmt.Sprint("t", i), Reference: "BATCH", Amount: 7})
		}
		rep := Reconcile([]Line{{Row: 2, ExternalID: "x", Date: "2030-03-01", Amount: int64(7 * n), Reference: "BATCH"}}, ps,
			day0("2030-03-01"), day0("2030-03-01"), 0)
		if n == MaxBatchPostings {
			if len(rep.Matched) != 1 || len(rep.Matched[0].Postings) != n {
				t.Fatalf("%d postings: %+v", n, rep.Summary)
			}
			continue
		}
		if len(rep.Matched) != 0 || len(rep.AmountMismatches) != 1 || rep.Summary.BatchSearchesCut != 1 {
			t.Fatalf("%d postings: %+v", n, rep.Summary)
		}
	}
}

// subsets is an independent reference: how many sets of two or more
// candidates (by position) sum to target, counting up to 2.
func subsets(amts []int64, target int64) int {
	n, count := len(amts), 0
	for mask := 1; mask < 1<<n && count < 2; mask++ {
		var sum int64
		k := 0
		for i := 0; i < n; i++ {
			if mask&(1<<i) != 0 {
				sum += amts[i]
				k++
			}
		}
		if k >= 2 && sum == target {
			count++
		}
	}
	return count
}

// The search proposes a batch exactly when one set of movements sums to
// the line, and says ambiguous exactly when more than one does.
func TestBatchSearchIsExact(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	var nFound, nAmbiguous int
	for iter := 0; iter < 20000; iter++ {
		n := r.Intn(13)
		cs := make([]cand, n)
		amts := make([]int64, n)
		for i := range cs {
			amts[i] = int64(1 + r.Intn(1+r.Intn(60)))
			cs[i] = cand{pi: i, amt: amts[i], seq: uint64(i + 1)}
		}
		target := int64(1 + r.Intn(150))
		b := budget{total: SearchSteps}
		b.startLine()
		pick, res := batch(cs, target, &b)
		want := []searchResult{noBatch, found, ambiguous}[subsets(amts, target)]
		if res != want || (pick != nil) != (res == found) {
			t.Fatalf("iteration %d: %v %v, want %v\namounts %v target %d", iter, pick, res, want, amts, target)
		}
		if res == ambiguous {
			nAmbiguous++
		}
		if pick == nil {
			continue
		}
		nFound++
		seen := map[int]bool{}
		var sum int64
		for _, pi := range pick {
			if seen[pi] {
				t.Fatalf("iteration %d: %d picked twice", iter, pi)
			}
			seen[pi] = true
			sum += amts[pi]
		}
		if len(pick) < 2 || sum != target {
			t.Fatalf("iteration %d: picked %v summing to %d, want %d", iter, pick, sum, target)
		}
	}
	if nFound < 500 || nAmbiguous < 500 {
		t.Fatalf("the inputs exercise too little: %d found, %d ambiguous", nFound, nAmbiguous)
	}
}

// Two sets of movements that sum to a line, or a movement that could be
// swapped for another of the same amount, leave the line a break.
func TestBatchAmbiguity(t *testing.T) {
	for _, amts := range [][]int64{{10, 20, 15, 15}, {10, 20, 10}} {
		var ps []Posting
		for i, a := range amts {
			ps = append(ps, Posting{Seq: uint64(i + 1), Time: at("2030-03-10"), Amount: a})
		}
		rep := Reconcile([]Line{{Row: 2, ExternalID: "x", Date: "2030-03-10", Amount: 30}}, ps, day0("2030-03-10"), day0("2030-03-10"), 1)
		if len(rep.Matched) != 0 || rep.Summary.BatchAmbiguous != 1 || rep.Summary.BatchSearchesCut != 0 || len(rep.MissingInStatement) != len(amts) {
			t.Fatalf("%v: %+v", amts, rep.Summary)
		}
	}
	// By reference too, and a mismatch then takes the first movement.
	rep := Reconcile([]Line{{Row: 2, ExternalID: "x", Date: "2030-03-10", Amount: 30, Reference: "R"}},
		[]Posting{{Seq: 1, Time: at("2030-03-10"), Reference: "R", Amount: 10}, {Seq: 2, Time: at("2030-03-10"), Reference: "R", Amount: 20},
			{Seq: 3, Time: at("2030-03-10"), Reference: "R", Amount: 5}, {Seq: 4, Time: at("2030-03-10"), Reference: "R", Amount: 25}},
		day0("2030-03-10"), day0("2030-03-10"), 1)
	if len(rep.AmountMismatches) != 1 || rep.AmountMismatches[0].Seq != 1 || rep.Summary.BatchAmbiguous != 1 {
		t.Fatalf("by reference: %+v", rep)
	}
}

func TestBatchOverflow(t *testing.T) {
	// Candidates whose sum overflows an int64 are searched, not summed.
	big := int64(math.MaxInt64/2 + 1)
	cs := []cand{{pi: 0, amt: big}, {pi: 1, amt: big}, {pi: 2, amt: big}, {pi: 3, amt: 5}, {pi: 4, amt: 6}}
	b := budget{total: SearchSteps}
	b.startLine()
	if pick, res := batch(cs, 11, &b); res != found || len(pick) != 2 {
		t.Fatalf("pick %v result %v", pick, res)
	}
	b.startLine()
	if pick, res := batch(cs, big, &b); pick != nil || res != noBatch {
		t.Fatalf("one movement is not a batch: %v %v", pick, res)
	}
	rep := Reconcile([]Line{{Row: 2, ExternalID: "x", Date: "2030-03-01", Amount: math.MinInt64}},
		[]Posting{{Seq: 1, Time: at("2030-03-01"), Amount: math.MinInt64 / 2}, {Seq: 2, Time: at("2030-03-01"), Amount: math.MinInt64 / 2}},
		day0("2030-03-01"), day0("2030-03-01"), 0)
	if len(rep.Matched) != 0 {
		t.Fatalf("an amount with no absolute value matched %+v", rep.Matched)
	}
}

// hostile builds amounts no subset of which sums to target: even, distinct
// amounts and an odd target, so neither the remaining-sum bound nor the
// repeated-amount rule prunes much.
func hostile(n int) (amts []int64, target int64) {
	for i := 0; i < n; i++ {
		amts = append(amts, int64(2*(1000+i*37)))
	}
	var sum int64
	for _, a := range amts {
		sum += a
	}
	return amts, sum/2 | 1
}

func TestBatchSearchIsBounded(t *testing.T) {
	amts, target := hostile(MaxBatchCandidates)
	cs := make([]cand, len(amts))
	for i, a := range amts {
		cs[i] = cand{pi: i, amt: a, seq: uint64(i + 1)}
	}
	b := budget{total: SearchSteps}
	b.startLine()
	pick, res := batch(cs, target, &b)
	if pick != nil || res != cut || b.line != 0 || b.total != SearchSteps-LineSearchSteps {
		t.Fatalf("pick %v result %v budget %+v", pick, res, b)
	}
	// One more candidate than the search takes is cut without a search.
	more := append(cs, cand{pi: len(cs), amt: 2, seq: uint64(len(cs) + 1)})
	b.startLine()
	if pick, res := batch(more, target, &b); pick != nil || res != cut || b.line != LineSearchSteps {
		t.Fatalf("too many candidates: %v %v %+v", pick, res, b)
	}
	// A small hostile set is searched to the end: no cut, no match.
	amts, target = hostile(12)
	cs = cs[:0]
	for i, a := range amts {
		cs = append(cs, cand{pi: i, amt: a, seq: uint64(i + 1)})
	}
	b.startLine()
	if pick, res := batch(cs, target, &b); pick != nil || res != noBatch {
		t.Fatalf("small hostile set: pick %v result %v", pick, res)
	}
}

// The worst statement for batch matching: every line is a batch candidate
// against every movement and none can match. The whole reconciliation must
// stay within the total budget, report every line it gave up on and still
// finish quickly.
func TestBatchWorstCase(t *testing.T) {
	cases := []struct {
		name  string
		lines int
		ps    int
		ref   string
	}{
		{"amount and date, wide", MaxLines, 100_000, ""},
		{"amount and date, deep", MaxLines, MaxBatchCandidates, ""},
		{"reference, deep", MaxLines, MaxBatchCandidates, "BATCH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amts, target := hostile(tc.ps)
			var ps []Posting
			for i, a := range amts {
				ps = append(ps, Posting{Seq: uint64(i + 1), Time: at("2030-01-01"), TransferID: fmt.Sprint("t", i), Reference: tc.ref, Amount: a})
			}
			var lines []Line
			for i := 0; i < tc.lines; i++ {
				lines = append(lines, Line{Row: i + 2, ExternalID: fmt.Sprint(i), Date: "2030-01-01", Amount: target + 2*int64(i), Reference: tc.ref})
			}
			start := time.Now()
			rep := Reconcile(lines, ps, day0("2030-01-01"), day0("2030-01-01"), MaxWindowDays)
			took := time.Since(start)
			if rep.Summary.Matched != 0 || rep.Summary.MissingInLedger+rep.Summary.AmountMismatches != tc.lines {
				t.Fatalf("summary %+v", rep.Summary)
			}
			if rep.Summary.BatchSearchesCut != tc.lines {
				t.Fatalf("%d lines gave up, want every one of %d", rep.Summary.BatchSearchesCut, tc.lines)
			}
			if took > 10*time.Second {
				t.Fatalf("took %v", took)
			}
			t.Logf("%d lines against %d movements: %v", tc.lines, tc.ps, took)
		})
	}
}

// Reports do not depend on the order lines and postings arrive in.
func TestBatchIsDeterministic(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	base := day0("2030-01-01")
	for iter := 0; iter < 300; iter++ {
		var lines []Line
		var ps []Posting
		for i := 0; i < 1+r.Intn(15); i++ {
			ref := ""
			if r.Intn(3) == 0 {
				ref = fmt.Sprint("r", r.Intn(3))
			}
			lines = append(lines, Line{Row: i + 2, ExternalID: fmt.Sprint(i), Date: base.AddDate(0, 0, r.Intn(6)).Format("2006-01-02"),
				Amount: int64(1 + r.Intn(40)), Reference: ref})
		}
		for i := 0; i < r.Intn(30); i++ {
			ref := ""
			if r.Intn(3) == 0 {
				ref = fmt.Sprint("r", r.Intn(3))
			}
			ps = append(ps, Posting{Seq: uint64(i + 1), Time: base.AddDate(0, 0, r.Intn(6)), TransferID: fmt.Sprint("t", i),
				Reference: ref, Amount: int64(1 + r.Intn(20))})
		}
		want := Reconcile(lines, ps, base, base.AddDate(0, 0, 5), 2)
		r.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
		r.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
		got := Reconcile(lines, ps, base, base.AddDate(0, 0, 5), 2)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: order changed the report\n%+v\n%+v", iter, want, got)
		}
		// Every batch is sound: distinct free postings of the line's sign
		// summing to it, within the window unless by reference.
		used := map[uint64]bool{}
		for _, m := range got.Matched {
			if m.Postings == nil {
				used[m.Seq] = true
				continue
			}
			var sum int64
			for _, p := range m.Postings {
				if used[p.Seq] {
					t.Fatalf("iteration %d: posting %d used twice", iter, p.Seq)
				}
				used[p.Seq] = true
				sum += p.Amount
			}
			if sum != m.Amount || len(m.Postings) < 2 || (m.By == ByAmountDateBatch && m.DaysApart > 2) {
				t.Fatalf("iteration %d: unsound batch %+v", iter, m)
			}
		}
	}
}

func BenchmarkBatchWorstCase(b *testing.B) {
	amts, target := hostile(MaxBatchCandidates)
	var ps []Posting
	for i, a := range amts {
		ps = append(ps, Posting{Seq: uint64(i + 1), Time: at("2030-01-01"), Amount: a})
	}
	var lines []Line
	for i := 0; i < MaxLines; i++ {
		lines = append(lines, Line{Row: i + 2, ExternalID: fmt.Sprint(i), Date: "2030-01-01", Amount: target + 2*int64(i)})
	}
	for b.Loop() {
		Reconcile(lines, ps, day0("2030-01-01"), day0("2030-01-01"), MaxWindowDays)
	}
}

// payouts builds a month of 500 payout lines, each settling 20 movements
// made up to two days before it, among 10,000 movements; half the payouts
// carry a reference their transfers share.
func payouts() ([]Line, []Posting) {
	r := rand.New(rand.NewSource(5))
	var ps []Posting
	var lines []Line
	seq := uint64(0)
	for i := 0; i < 500; i++ {
		d := day0("2030-01-01").AddDate(0, 0, r.Intn(28))
		var sum int64
		ref := ""
		if i%2 == 0 {
			ref = fmt.Sprint("po-", i)
		}
		for k := 0; k < 20; k++ {
			seq++
			amt := -int64(100 + r.Intn(100_000))
			sum += amt
			ps = append(ps, Posting{Seq: seq, Time: d.Add(-time.Duration(r.Intn(48)) * time.Hour), TransferID: fmt.Sprint("t", seq), Reference: ref, Amount: amt})
		}
		lines = append(lines, Line{Row: i + 2, ExternalID: fmt.Sprint(i), Date: d.Format("2006-01-02"), Amount: sum, Reference: ref})
	}
	return lines, ps
}

// Interleaved payouts: every payout with a reference matches the 20
// movements it pays. Those without one share their window with hundreds of
// movements, so no batch is proposed for them rather than a coincidental
// one. Weekly sweeps of a few movements each match by amount and date.
func TestPayoutBatches(t *testing.T) {
	lines, ps := payouts()
	start := time.Now()
	rep := Reconcile(lines, ps, day0("2030-01-01"), day0("2030-01-31"), 3)
	t.Logf("%v: %+v", time.Since(start), rep.Summary)
	if rep.Summary.BatchMatches != 250 || rep.Summary.MissingInLedger != 250 ||
		rep.Summary.BatchSearchesCut+rep.Summary.BatchAmbiguous != 250 {
		t.Fatalf("summary %+v", rep.Summary)
	}
	for _, m := range rep.Matched {
		if m.By != ByReferenceBatch || len(m.Postings) != 20 {
			t.Fatalf("match %+v", m)
		}
		for _, p := range m.Postings {
			if (p.Seq-1)/20 != uint64(m.Row-2) {
				t.Fatalf("payout %d took movement %d", m.Row-2, p.Seq)
			}
		}
	}

	r := rand.New(rand.NewSource(9))
	lines, ps = nil, nil
	for w := 0; w < 4; w++ {
		d := day0("2030-02-03").AddDate(0, 0, 7*w)
		var sum int64
		for k := 0; k < 5; k++ {
			amt := int64(1000 + r.Intn(9000))
			sum += amt
			ps = append(ps, Posting{Seq: uint64(len(ps) + 1), Time: d.AddDate(0, 0, -k%3), TransferID: fmt.Sprint("t", len(ps)), Amount: amt})
		}
		lines = append(lines, Line{Row: w + 2, ExternalID: fmt.Sprint("sweep", w), Date: d.Format("2006-01-02"), Amount: sum})
	}
	rep = Reconcile(lines, ps, day0("2030-02-01"), day0("2030-02-28"), 3)
	if rep.Summary.BatchMatches != 4 || len(rep.MissingInStatement) != 0 {
		t.Fatalf("sweeps: %+v", rep.Summary)
	}
	for _, m := range rep.Matched {
		if m.By != ByAmountDateBatch || !reflect.DeepEqual(seqs(m), []uint64{uint64(5*(m.Row-2) + 1), uint64(5*(m.Row-2) + 2),
			uint64(5*(m.Row-2) + 3), uint64(5*(m.Row-2) + 4), uint64(5*(m.Row-2) + 5)}) {
			t.Fatalf("sweep %+v", m)
		}
	}
}

func BenchmarkPayoutBatches(b *testing.B) {
	lines, ps := payouts()
	for b.Loop() {
		Reconcile(lines, ps, day0("2030-01-01"), day0("2030-01-31"), 3)
	}
}
