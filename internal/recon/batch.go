package recon

import (
	"math"
	"sort"
)

// Many-to-one matching. A payout batch, a card settlement or a sweep shows
// on the bank statement as one line that stands for several ledger
// movements. After the one-to-one passes, a line left unmatched may be
// matched to a set of two or more free movements whose amounts sum exactly
// to the line's amount:
//
//   - by reference (pass 1): the movements whose transfers carry the line's
//     reference, at any date, as one-to-one reference matching does;
//   - by amount and date (pass 3): free movements dated within the window
//     of the line.
//
// Only movements with the line's sign and no larger than the line are
// candidates. If they sum to the line together, they are the batch;
// otherwise a depth-first subset-sum search runs over them if there are at
// most MaxBatchCandidates, largest first, with pruning on the remaining sum
// and on repeated amounts. A batch is proposed only when it is the only set
// of candidates that sums to the line: among many movements of assorted
// amounts some subset hits almost any total by coincidence, and a wrong
// batch is worse than a break. Lines with two or more batches are counted
// in Summary.BatchAmbiguous.
//
// Subset sum is NP-complete, so the search is bounded by work, not by luck:
// each line may spend LineSearchSteps steps (a scanned candidate or a
// search node) and a whole reconciliation SearchSteps. A line whose search
// is cut off, by the budget or by having too many candidates, stays a break
// and is counted in Summary.BatchSearchesCut, so a hostile statement costs
// a bounded, known amount of time and the result stays deterministic: the
// same statement and movements give the same report on every machine.

// Batch search bounds.
const (
	// MaxBatchPostings is the most movements one statement line may stand
	// for.
	MaxBatchPostings = 1000
	// MaxBatchCandidates is the most movements the subset search runs over
	// for one line. Candidates that sum to the line all together are
	// accepted up to MaxBatchPostings without a search.
	MaxBatchCandidates = 256
	// LineSearchSteps is the work one line's batch search may do.
	LineSearchSteps = 50_000
	// SearchSteps is the work all batch searches of one reconciliation may
	// do together.
	SearchSteps = 2_000_000
)

// budget counts search work: what the current line may still spend and
// what the reconciliation may.
type budget struct {
	line, total int
}

func (b *budget) startLine() { b.startLineOf(LineSearchSteps) }

// startLineOf starts a line that may spend up to n steps.
func (b *budget) startLineOf(n int) { b.line = min(n, b.total) }

// spend takes one step and reports whether there was one to take.
func (b *budget) spend() bool {
	if b.line <= 0 {
		return false
	}
	b.line--
	b.total--
	return true
}

// cand is a candidate movement for a batch: its index among the postings,
// its amount made positive, and its entry.
type cand struct {
	pi  int
	amt int64
	seq uint64
}

// searchResult says why batch found no batch.
type searchResult int

const (
	noBatch   searchResult = iota // no subset sums to the line
	found                         // exactly one does
	ambiguous                     // more than one does
	cut                           // the search could not finish
)

// batch looks for the one set of two or more candidates whose amounts sum
// to target (positive) and returns their posting indexes. A batch is only
// proposed when it is the only one: if two sets of movements sum to the
// line, or a picked movement could be swapped for another of the same
// amount, the result is ambiguous and nothing is picked. More than
// MaxBatchCandidates candidates, or a budget that runs out, cut the search
// short. cs may be reordered.
func batch(cs []cand, target int64, b *budget) ([]int, searchResult) {
	if len(cs) < 2 {
		return nil, noBatch
	}
	total, overflow := int64(0), false
	for _, c := range cs {
		if total > math.MaxInt64-c.amt {
			overflow = true
			break
		}
		total += c.amt
	}
	if !overflow && total < target {
		return nil, noBatch
	}
	// All of them: no other set can sum to the line, since every amount is
	// positive and any other set leaves one out.
	if !overflow && total == target && len(cs) <= MaxBatchPostings {
		pick := make([]int, len(cs))
		for i, c := range cs {
			pick[i] = c.pi
		}
		return pick, found
	}
	if len(cs) > MaxBatchCandidates {
		return nil, cut
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].amt != cs[j].amt {
			return cs[i].amt > cs[j].amt
		}
		return cs[i].seq < cs[j].seq
	})
	n := len(cs)
	suffix := make([]int64, n+1) // suffix[i]: sum of cs[i:], saturating
	for i := n - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1]
		if suffix[i] > math.MaxInt64-cs[i].amt {
			suffix[i] = math.MaxInt64
		} else {
			suffix[i] += cs[i].amt
		}
	}
	// The search visits each distinct multiset of amounts once: at one
	// depth an amount equal to the one before it was already tried. It
	// stops at the second solution.
	var first []int
	solutions, stopped := 0, false
	path := make([]int, 0, 16)
	var dfs func(i int, rem int64)
	dfs = func(i int, rem int64) {
		for j := i; j < n && !stopped; j++ {
			if !b.spend() {
				stopped = true
				return
			}
			if suffix[j] < rem {
				return // even every candidate left falls short
			}
			a := cs[j].amt
			if a > rem || (j > i && a == cs[j-1].amt) {
				continue // too big, or this amount was already tried here
			}
			if a == rem {
				if len(path) == 0 {
					continue // a single movement is a one-to-one match, not a batch
				}
				solutions++
				if solutions == 1 {
					first = append(append([]int(nil), path...), j)
				} else {
					stopped = true
				}
				continue
			}
			if len(path)+1 == MaxBatchPostings {
				continue
			}
			path = append(path, j)
			dfs(j+1, rem-a)
			path = path[:len(path)-1]
		}
	}
	dfs(0, target)
	switch {
	case solutions > 1:
		return nil, ambiguous
	case stopped:
		return nil, cut
	case solutions == 0:
		return nil, noBatch
	}
	// One multiset of amounts; it is one set of movements only if no
	// movement left out has the amount of one picked.
	picked := make(map[int64]int, len(first))
	for _, j := range first {
		picked[cs[j].amt]++
	}
	for _, c := range cs {
		picked[c.amt]--
	}
	for _, j := range first {
		if picked[cs[j].amt] < 0 {
			return nil, ambiguous
		}
	}
	pick := make([]int, len(first))
	for i, j := range first {
		pick[i] = cs[j].pi
	}
	return pick, found
}

// fits reports whether a movement of amount p can be part of a batch for a
// line of amount target: the same sign and no larger.
func fits(p, target int64) bool {
	if target > 0 {
		return p > 0 && p <= target
	}
	return target < 0 && p < 0 && p >= target
}

// batchable reports whether a line can be matched by a batch: it has an
// amount, and one whose size fits in an int64.
func batchable(amount int64) bool { return amount != 0 && amount != math.MinInt64 }

func absAmount(v int64) int64 {
	if v == math.MinInt64 {
		return math.MaxInt64 // never a candidate for anything smaller
	}
	return abs(v)
}
