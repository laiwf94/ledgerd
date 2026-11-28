package recon

import (
	"sort"
	"time"

	"github.com/laiwf94/ledgerd/internal/money"
)

// modelReconcile is a second, deliberately naive implementation of the
// matching rules in the package comment, batch.go and near.go, written to
// be read against them rather than to be fast: every search scans all
// lines and movements and every batch search tries every subset. It has no
// work budget, so it is only for small inputs, on which ReconcileWith never
// cuts a search short. Its stats count the searches that found more than
// one batch or near-match, whatever became of the line afterwards.
func modelReconcile(lines []Line, postings []Posting, from, to time.Time, opt Options) (Report, modelStats) {
	var st modelStats
	type mpost struct {
		Posting
		day int64
	}
	type mline struct {
		Line
		day int64
		ok  bool
	}
	ps := make([]mpost, len(postings))
	for i, p := range postings {
		ps[i] = mpost{Posting: p, day: day(p.Time)}
		ps[i].Date = dayDate(ps[i].day)
	}
	// Movements in journal order, lines in date order.
	sort.Slice(ps, func(i, j int) bool { return ps[i].Seq < ps[j].Seq })
	ls := make([]mline, len(lines))
	for i, ln := range lines {
		ls[i].Line = ln
		if d, err := time.Parse(dateLayout, ln.Date); err == nil {
			ls[i].day, ls[i].ok = day(d), true
		}
	}
	sort.Slice(ls, func(i, j int) bool {
		if ls[i].day != ls[j].day {
			return ls[i].day < ls[j].day
		}
		return ls[i].Row < ls[j].Row
	})
	// byDay lists the movements in (day, seq) order.
	byDay := make([]int, len(ps))
	for i := range byDay {
		byDay[i] = i
	}
	sort.SliceStable(byDay, func(x, y int) bool { return ps[byDay[x]].day < ps[byDay[y]].day })

	w := int64(opt.WindowDays)
	tolerant := opt.Tolerance > 0 || opt.ToleranceBP > 0
	allowed := func(amount int64) int64 {
		if !tolerant {
			return 0
		}
		return money.Tolerance(amount, max(opt.Tolerance, 0), max(opt.ToleranceBP, 0))
	}
	sameWay := func(a, b int64) bool { return (a > 0) == (b > 0) }
	// near: a movement of amount p moves the way of a line of amount l (a
	// zero movement moves neither way) and is within the line's tolerance.
	near := func(l, p int64) bool { return l != 0 && p != 0 && sameWay(l, p) && abs(l-p) <= allowed(l) }
	inWindow := func(li, pi int) bool { return abs(ls[li].day-ps[pi].day) <= w }
	sameRef := func(li, pi int) bool { return ps[pi].Reference == ls[li].Reference }
	refLine := func(li int) bool { return ls[li].Reference != "" && ls[li].ok }
	nearLine := func(li int) bool { return ls[li].ok && ls[li].Amount != 0 && allowed(ls[li].Amount) > 0 }
	// partOf: a movement of amount p can be part of a batch for a line of
	// amount l: the same sign and no larger.
	partOf := func(p, l int64) bool { return l != 0 && p != 0 && sameWay(p, l) && abs(p) <= abs(l) }

	rep := Report{From: from.Format(dateLayout), To: to.Format(dateLayout), WindowDays: opt.WindowDays,
		Tolerance: max(opt.Tolerance, 0), ToleranceBP: max(opt.ToleranceBP, 0),
		Matched: []Match{}, AmountMismatches: []Mismatch{}, MissingInLedger: []Line{}, MissingInStatement: []Posting{}}
	used := make([]bool, len(ps))
	done := make([]bool, len(ls))
	batchAmbiguous := make([]bool, len(ls))
	nearAmbiguous := make([]bool, len(ls))
	var nBatchAmbiguous, nNearAmbiguous int
	broken := func(li int) {
		if batchAmbiguous[li] {
			nBatchAmbiguous++
		}
		if nearAmbiguous[li] {
			nNearAmbiguous++
		}
	}

	single := func(li, pi int, by string) {
		used[pi], done[li] = true, true
		ln, p := ls[li], ps[pi]
		m := Match{ExternalID: ln.ExternalID, Row: ln.Row, TransferID: p.TransferID, Seq: p.Seq, Amount: p.Amount,
			By: by, DaysApart: abs(ln.day - p.day)}
		if by == ByReferenceNear || by == ByAmountDateNear {
			m.Difference = ln.Amount - p.Amount
		}
		rep.Matched = append(rep.Matched, m)
	}
	// tryBatch matches the line to the one set of two or more candidates
	// that sums to it, trying every subset; two or more such sets make it
	// ambiguous.
	tryBatch := func(li int, cands []int, by string) {
		target := ls[li].Amount
		var pick []int
		sets := 0
		for mask := 1; mask < 1<<len(cands) && sets < 2; mask++ {
			var sum int64
			var set []int
			for k, pi := range cands {
				if mask&(1<<k) != 0 {
					sum += ps[pi].Amount
					set = append(set, pi)
				}
			}
			if len(set) >= 2 && sum == target {
				sets++
				pick = set
			}
		}
		if sets > 1 {
			batchAmbiguous[li] = true
			st.batchAmbiguous++
		}
		if sets != 1 {
			return
		}
		sort.Ints(pick)
		ln := ls[li]
		m := Match{ExternalID: ln.ExternalID, Row: ln.Row, Amount: ln.Amount, By: by}
		for _, pi := range pick {
			used[pi] = true
			p := ps[pi]
			m.Postings = append(m.Postings, Part{Seq: p.Seq, TransferID: p.TransferID, Date: p.Date, Amount: p.Amount})
			m.DaysApart = max(m.DaysApart, abs(ln.day-p.day))
		}
		done[li] = true
		rep.Matched = append(rep.Matched, m)
	}
	// nearRound decides near-matches over the state at its start: an open
	// line that eligible accepts and a free movement that link joins to it
	// are matched when each is the other's only candidate.
	nearRound := func(eligible func(li int) bool, link func(li, pi int) bool, by string) {
		var open []int
		for li := range ls {
			if !done[li] && eligible(li) {
				open = append(open, li)
			}
		}
		var pairs [][2]int
		for _, li := range open {
			var cands []int
			for pi := range ps {
				if !used[pi] && link(li, pi) {
					cands = append(cands, pi)
				}
			}
			if len(cands) > 1 {
				nearAmbiguous[li] = true
				st.nearAmbiguous++
			}
			if len(cands) != 1 {
				continue
			}
			rival := false
			for _, lj := range open {
				rival = rival || (lj != li && link(lj, cands[0]))
			}
			if rival {
				nearAmbiguous[li] = true
				st.nearAmbiguous++
			} else {
				pairs = append(pairs, [2]int{li, cands[0]})
			}
		}
		for _, pr := range pairs {
			single(pr[0], pr[1], by)
		}
	}

	// Pass 1, by reference. Exact amounts first, each line taking the
	// earliest movement with its reference and amount.
	for li := range ls {
		if !refLine(li) {
			continue
		}
		for pi := range ps {
			if !used[pi] && sameRef(li, pi) && ps[pi].Amount == ls[li].Amount {
				single(li, pi, ByReference)
				break
			}
		}
	}
	// Then batches among the free movements with the line's reference, for
	// a reference that two or more movements carry.
	for li := range ls {
		if done[li] || !refLine(li) || ls[li].Amount == 0 {
			continue
		}
		var cands []int
		carriers := 0
		for pi := range ps {
			if sameRef(li, pi) {
				carriers++
				if !used[pi] && partOf(ps[pi].Amount, ls[li].Amount) {
					cands = append(cands, pi)
				}
			}
		}
		if carriers >= 2 {
			tryBatch(li, cands, ByReferenceBatch)
		}
	}
	// Then near-matches by reference.
	if tolerant {
		nearRound(func(li int) bool { return refLine(li) && nearLine(li) },
			func(li, pi int) bool {
				return refLine(li) && nearLine(li) && sameRef(li, pi) && near(ls[li].Amount, ps[pi].Amount)
			}, ByReferenceNear)
	}
	// Then a mismatch with the earliest free movement with the reference.
	for li := range ls {
		if done[li] || !refLine(li) {
			continue
		}
		for pi := range ps {
			if !used[pi] && sameRef(li, pi) {
				used[pi], done[li] = true, true
				broken(li)
				ln, p := ls[li], ps[pi]
				rep.AmountMismatches = append(rep.AmountMismatches, Mismatch{ExternalID: ln.ExternalID, Row: ln.Row,
					TransferID: p.TransferID, Seq: p.Seq, StatementAmount: ln.Amount, LedgerAmount: p.Amount})
				break
			}
		}
	}

	// Pass 2: each line in date order takes the earliest free movement
	// of its amount within the window.
	for li := range ls {
		if done[li] || !ls[li].ok {
			continue
		}
		for _, pi := range byDay {
			if !used[pi] && ps[pi].Amount == ls[li].Amount && inWindow(li, pi) {
				single(li, pi, ByAmountDate)
				break
			}
		}
	}

	// Pass 3: batches among the free movements within the window.
	for li := range ls {
		if done[li] || !ls[li].ok || ls[li].Amount == 0 {
			continue
		}
		var cands []int
		for _, pi := range byDay {
			if !used[pi] && inWindow(li, pi) && partOf(ps[pi].Amount, ls[li].Amount) {
				cands = append(cands, pi)
			}
		}
		tryBatch(li, cands, ByAmountDateBatch)
	}

	// Pass 4: near-matches by amount and date.
	if tolerant {
		nearRound(nearLine, func(li, pi int) bool {
			return nearLine(li) && inWindow(li, pi) && near(ls[li].Amount, ps[pi].Amount)
		}, ByAmountDateNear)
	}

	for li := range ls {
		if !done[li] {
			broken(li)
			rep.MissingInLedger = append(rep.MissingInLedger, ls[li].Line)
		}
	}
	for pi, p := range ps {
		if !used[pi] && p.day >= day(from) && p.day <= day(to) {
			rep.MissingInStatement = append(rep.MissingInStatement, p.Posting)
		}
	}
	sort.SliceStable(rep.Matched, func(i, j int) bool { return rep.Matched[i].Row < rep.Matched[j].Row })
	sort.SliceStable(rep.AmountMismatches, func(i, j int) bool { return rep.AmountMismatches[i].Row < rep.AmountMismatches[j].Row })
	sort.SliceStable(rep.MissingInLedger, func(i, j int) bool { return rep.MissingInLedger[i].Row < rep.MissingInLedger[j].Row })
	rep.Summary = Summary{StatementLines: len(lines), LedgerPostings: len(postings), Matched: len(rep.Matched),
		AmountMismatches: len(rep.AmountMismatches), MissingInLedger: len(rep.MissingInLedger),
		MissingInStatement: len(rep.MissingInStatement), BatchAmbiguous: nBatchAmbiguous, NearAmbiguous: nNearAmbiguous}
	for _, m := range rep.Matched {
		switch m.By {
		case ByReferenceBatch, ByAmountDateBatch:
			rep.Summary.BatchMatches++
		case ByReferenceNear, ByAmountDateNear:
			rep.Summary.NearMatches++
		}
	}
	return rep, st
}

type modelStats struct {
	batchAmbiguous, nearAmbiguous int
}
