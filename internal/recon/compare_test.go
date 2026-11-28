package recon

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/money"
)

// modelInputs builds a small statement and ledger whose amounts are chosen
// to hit every pass: lines that copy a movement, sum several of them
// (payout batches, by reference or by date), differ from one by a fee or
// rounding, or match nothing, with shared references, refunds, zero
// amounts and a tolerance that is sometimes off. Some ledgers also hold a
// cluster of repeated small amounts on two days, so that several sets of
// movements can sum to one line.
func modelInputs(r *rand.Rand, base time.Time) ([]Line, []Posting, Options) {
	ref := func(oneIn int) string {
		if r.Intn(oneIn) == 0 {
			return fmt.Sprint("r", r.Intn(3))
		}
		return ""
	}
	var ps []Posting
	add := func(amt int64, days int) {
		i := len(ps)
		ps = append(ps, Posting{Seq: uint64(10 + i*3), Time: base.AddDate(0, 0, r.Intn(days)).Add(time.Duration(r.Intn(86400)) * time.Second),
			TransferID: fmt.Sprint("t", i), Reference: ref(2), Amount: amt})
	}
	for n := r.Intn(9); len(ps) < n; {
		amt := int64(1 + r.Intn(40))
		switch r.Intn(10) {
		case 0:
			amt = -amt
		case 1:
			amt = 0
		}
		add(amt, 8)
	}
	cluster := r.Intn(3) == 0
	if cluster {
		for n := len(ps) + 2 + r.Intn(4); len(ps) < n; {
			add(int64(5*(1+r.Intn(3))), 2)
		}
	}
	date := func(days int) string { return base.AddDate(0, 0, r.Intn(days)).Format(dateLayout) }
	some := func() int64 { return ps[r.Intn(len(ps))].Amount }
	var lines []Line
	for i := 0; i < r.Intn(9); i++ {
		ln := Line{Row: i + 2, ExternalID: fmt.Sprint("x", i), Currency: "USD", Date: date(8), Reference: ref(3)}
		k := r.Intn(8)
		switch {
		case len(ps) == 0 || k == 0:
			ln.Amount = int64(r.Intn(80)) - 10
		case k <= 2:
			ln.Amount = some()
		case k == 3:
			ln.Amount = some() + some()
			if r.Intn(2) == 0 {
				ln.Amount += some()
			}
		case k == 4:
			// A payout batch under one reference: some of the movements
			// that carry it.
			ln.Reference = fmt.Sprint("r", r.Intn(3))
			for _, p := range ps {
				if p.Reference == ln.Reference && r.Intn(3) != 0 {
					ln.Amount += p.Amount
				}
			}
		case k == 5 && cluster:
			// A sum of the cluster's amounts, on its days and with no
			// reference, so pass 1 does not take it as a mismatch.
			ln.Amount, ln.Date, ln.Reference = int64(5*(3+r.Intn(4))), date(2), ""
		default:
			ln.Amount = some() + int64(r.Intn(7)) - 3
		}
		if len(ps) > 0 && k == 6 {
			ln.Reference = ps[r.Intn(len(ps))].Reference
		}
		lines = append(lines, ln)
	}
	opt := Options{WindowDays: r.Intn(3)}
	if r.Intn(4) != 0 {
		opt.Tolerance = int64(r.Intn(4))
		opt.ToleranceBP = int64(r.Intn(3) * 500) // 0, 5% or 10%
	}
	return lines, ps, opt
}

// The reconciliation agrees with the naive model on random statements that
// mix exact, batch and near-matches, and its one-to-one matches stand up
// to maximum bipartite matchings: the exact matches by amount and date are
// as many as any assignment of what pass 1 left could make, and all the
// one-to-one matches together, exact and within the tolerance, are no more
// than a maximum matching of the lines and movements outside batches
// allows.
func TestMatchingAgreesWithModel(t *testing.T) {
	r := rand.New(rand.NewSource(17))
	base := day0("2030-01-01")
	from, to := base.AddDate(0, 0, 1), base.AddDate(0, 0, 6)
	kinds := map[string]int{}
	var nMismatch, nBatchAmbiguous, nNearAmbiguous int
	for iter := 0; iter < 30000; iter++ {
		lines, ps, opt := modelInputs(r, base)
		rep := ReconcileWith(lines, ps, from, to, opt)
		want, st := modelReconcile(lines, ps, from, to, opt)
		if !reflect.DeepEqual(rep, want) {
			t.Fatalf("iteration %d: options %+v\nlines %+v\npostings %+v\ngot  %+v\nwant %+v", iter, opt, lines, ps, rep, want)
		}
		if rep.Summary.BatchSearchesCut != 0 || rep.Summary.NearSearchesCut != 0 {
			t.Fatalf("iteration %d: a small search was cut: %+v", iter, rep.Summary)
		}
		for _, m := range rep.Matched {
			kinds[m.By]++
		}
		nMismatch += rep.Summary.AmountMismatches
		nBatchAmbiguous += st.batchAmbiguous
		nNearAmbiguous += st.nearAmbiguous

		lineAt := map[int]int{}
		for i, ln := range lines {
			lineAt[ln.Row] = i
		}
		postingAt := map[uint64]int{}
		for i, p := range ps {
			postingAt[p.Seq] = i
		}
		// What pass 1 took, and what batches took.
		pass1L, pass1P := make([]bool, len(lines)), make([]bool, len(ps))
		batchL, batchP := make([]bool, len(lines)), make([]bool, len(ps))
		oneToOne := 0
		for _, m := range rep.Matched {
			li := lineAt[m.Row]
			switch m.By {
			case ByReference, ByReferenceNear:
				pass1L[li], pass1P[postingAt[m.Seq]] = true, true
			case ByReferenceBatch:
				pass1L[li], batchL[li] = true, true
				for _, p := range m.Postings {
					pass1P[postingAt[p.Seq]], batchP[postingAt[p.Seq]] = true, true
				}
			case ByAmountDateBatch:
				batchL[li] = true
				for _, p := range m.Postings {
					batchP[postingAt[p.Seq]] = true
				}
			}
			if m.Postings == nil {
				oneToOne++
			}
		}
		for _, mm := range rep.AmountMismatches {
			pass1L[lineAt[mm.Row]], pass1P[postingAt[mm.Seq]] = true, true
		}
		dayOf := func(li int) int64 { d, _ := time.Parse(dateLayout, lines[li].Date); return day(d) }
		inWindow := func(li, pi int) bool { return abs(dayOf(li)-day(ps[pi].Time)) <= int64(opt.WindowDays) }

		exact := maxMatchingBy(len(lines), len(ps), func(li, pi int) bool {
			return !pass1L[li] && !pass1P[pi] && lines[li].Amount == ps[pi].Amount && inWindow(li, pi)
		})
		if got := countBy(rep, ByAmountDate); got != exact {
			t.Fatalf("iteration %d: %d exact matches by amount and date, a maximum matching of what pass 1 left has %d", iter, got, exact)
		}
		tolerant := opt.Tolerance > 0 || opt.ToleranceBP > 0
		bound := maxMatchingBy(len(lines), len(ps), func(li, pi int) bool {
			if batchL[li] || batchP[pi] {
				return false
			}
			l, p := lines[li].Amount, ps[pi].Amount
			fit := l == p || (tolerant && l != 0 && (l > 0) == (p > 0) &&
				abs(l-p) <= money.Tolerance(l, opt.Tolerance, opt.ToleranceBP))
			ref := lines[li].Reference != "" && lines[li].Reference == ps[pi].Reference
			return fit && (ref || inWindow(li, pi))
		})
		if oneToOne > bound {
			t.Fatalf("iteration %d: %d one-to-one matches, more than a maximum matching's %d", iter, oneToOne, bound)
		}
	}
	for _, by := range []string{ByReference, ByAmountDate, ByReferenceBatch, ByAmountDateBatch, ByReferenceNear, ByAmountDateNear} {
		if kinds[by] < 300 {
			t.Fatalf("the inputs exercise too little: %v", kinds)
		}
	}
	if nMismatch < 1000 || nBatchAmbiguous < 300 || nNearAmbiguous < 300 {
		t.Fatalf("the inputs exercise too little: %d mismatches, %d ambiguous batch searches, %d ambiguous near-match searches", nMismatch, nBatchAmbiguous, nNearAmbiguous)
	}
	t.Logf("matches %v, mismatches %d, ambiguous batch searches %d, ambiguous near-match searches %d", kinds, nMismatch, nBatchAmbiguous, nNearAmbiguous)
}

func countBy(rep Report, by string) int {
	n := 0
	for _, m := range rep.Matched {
		if m.By == by {
			n++
		}
	}
	return n
}
