package recon

import (
	"math/bits"
	"sort"

	"github.com/laiwf94/ledgerd/internal/money"
)

// Near-matches. Fees, rounding and FX spreads make a statement line differ
// a little from the movement it stands for. A run may allow a tolerance:
// an absolute amount in minor units, a percentage of the line in basis
// points, or both, in which case the larger applies (money.Tolerance). A
// line still open after the exact passes may then match one free movement
// that moves its way and differs from it by no more than the line's
// tolerance:
//
//   - by reference (pass 1, after batches and before amount mismatches):
//     among the free movements with the line's reference (reference_near);
//   - by amount and date (pass 4, after batches): among the free movements
//     within the window (amount_date_near).
//
// As with batches, a near-match is proposed only when it is unambiguous:
// the line has exactly one candidate movement, and that movement is a
// candidate of no other open line. Among many movements of similar amounts
// the nearest is often another transaction's, and a wrong match is worse
// than a break. In other words, the line and the movement are each
// other's only candidates. Each pass decides over the state at its start,
// so the order of the lines does not matter, and one round is enough:
// taking such a pair changes nothing for any other line or movement. Lines
// that had a candidate but no unique one stay breaks and are counted in
// Summary.NearAmbiguous.
//
// Exact matches and batches always come first, so a tolerance never takes
// a movement that matches some line exactly, and a near-match is labelled
// as one with its difference (line minus movement) recorded. Without a
// tolerance, nothing changes.
//
// The searches spend steps from a budget of their own, as the batch search
// does: a scanned movement or line, or an amount group probed, is a step;
// one search may spend NearLineSteps and a reconciliation NearSearchSteps.
// A line whose search runs out stays a break and is counted in
// Summary.NearSearchesCut.

// Near-match search bounds.
const (
	// NearLineSteps is the work one line's near-match search may do: a
	// scanned movement with the line's reference, or an amount probed in
	// the amount-and-date pass.
	NearLineSteps = 10_000
	// NearSearchSteps is the work all near-match searches of one
	// reconciliation may do together.
	NearSearchSteps = 1_000_000
)

// Tolerance limits.
const (
	// MaxToleranceBasisPoints is the largest percentage tolerance, 10%.
	MaxToleranceBasisPoints = money.MaxToleranceBasisPoints
)

// Options tune a reconciliation.
type Options struct {
	// WindowDays is how many days a movement may be from a line it matches
	// by amount and date.
	WindowDays int
	// Tolerance is the absolute difference in minor units a near-match may
	// have, and ToleranceBP the difference in basis points of the line's
	// amount; the larger of the two applies. Both zero means exact matching
	// only.
	Tolerance   int64
	ToleranceBP int64
}

func (o Options) allowed(amount int64) int64 {
	if o.Tolerance <= 0 && o.ToleranceBP <= 0 {
		return 0
	}
	return money.Tolerance(amount, max(o.Tolerance, 0), max(o.ToleranceBP, 0))
}

// nearIndex holds items (free movements, or open lines) grouped by amount
// in ascending order and, in a group, by (day, index). Union-find indexes
// skip taken items in a group and groups with none left.
type nearIndex struct {
	days  []int64 // day of each item, by item index
	amt   []int64 // the groups' amounts, ascending
	idx   [][]int // item indexes per group
	next  [][]int // per group: next untaken position at or after i
	right []int   // next group at or after i with an untaken item
	group []int   // each item's group, or -1
	pos   []int   // each item's position in its group
}

// newNearIndex indexes the items (in index order) by amount.
func newNearIndex(items []int, amount func(int) int64, days []int64) *nearIndex {
	order := append([]int(nil), items...)
	sort.SliceStable(order, func(x, y int) bool {
		a, b := order[x], order[y]
		if amount(a) != amount(b) {
			return amount(a) < amount(b)
		}
		return days[a] < days[b]
	})
	ix := &nearIndex{days: days, group: make([]int, len(days)), pos: make([]int, len(days))}
	for i := range ix.group {
		ix.group[i] = -1
	}
	for _, it := range order {
		if n := len(ix.amt); n == 0 || ix.amt[n-1] != amount(it) {
			ix.amt = append(ix.amt, amount(it))
			ix.idx = append(ix.idx, nil)
		}
		g := len(ix.idx) - 1
		ix.group[it], ix.pos[it] = g, len(ix.idx[g])
		ix.idx[g] = append(ix.idx[g], it)
	}
	ix.next = make([][]int, len(ix.amt))
	for g := range ix.idx {
		ix.next[g] = identity(len(ix.idx[g]) + 1)
	}
	ix.right = identity(len(ix.amt) + 1)
	return ix
}

func identity(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}

func root(next []int, i int) int {
	for next[i] != i {
		next[i] = next[next[i]] // path halving
		i = next[i]
	}
	return i
}

// take marks an item taken and drops its group once it has none left.
func (ix *nearIndex) take(it int) {
	g, pos := ix.group[it], ix.pos[it]
	ix.next[g][pos] = pos + 1
	if root(ix.next[g], 0) == len(ix.idx[g]) {
		ix.right[g] = g + 1
	}
}

// collect appends to out the untaken items with amounts in [lo, hi] dated
// within w days of day that ok accepts, other than skip, stopping once out
// holds limit. It returns cut if the budget ran out first.
func (ix *nearIndex) collect(out []int, lo, hi, day, w int64, skip, limit int, ok func(int) bool, b *budget) ([]int, searchResult) {
	g := root(ix.right, sort.Search(len(ix.amt), func(i int) bool { return ix.amt[i] >= lo }))
	for ; g < len(ix.amt) && ix.amt[g] <= hi; g = root(ix.right, g+1) {
		if !b.spend() {
			return out, cut
		}
		idx := ix.idx[g]
		start := sort.Search(len(idx), func(i int) bool { return ix.days[idx[i]] >= day-w })
		for p := root(ix.next[g], start); p < len(idx) && ix.days[idx[p]] <= day+w; p = root(ix.next[g], p+1) {
			if !b.spend() {
				return out, cut
			}
			if it := idx[p]; it != skip && ok(it) {
				if out = append(out, it); len(out) == limit {
					return out, noBatch
				}
			}
		}
	}
	return out, noBatch
}

// span returns the amounts [lo, hi] within d of amount on its side of zero.
func span(amount, d int64) (lo, hi int64) {
	lo, hi = satSub(amount, d), satAdd(amount, d)
	if amount > 0 {
		lo = max(lo, 1)
	} else {
		hi = min(hi, -1)
	}
	return lo, hi
}

// reach bounds how far from a movement's amount a line may be and still
// have the movement within its tolerance: d <= max(abs, (|m|+d)·bp/10,000)
// gives d <= max(abs, |m|·bp/(10,000-bp)), rounded up here.
func reach(m int64, o Options) int64 {
	if o.ToleranceBP <= 0 {
		return max(o.Tolerance, 0)
	}
	u := uint64(m)
	if m < 0 {
		u = -u
	}
	hi, lo := bits.Mul64(u, uint64(o.ToleranceBP))
	den := uint64(10_000 - o.ToleranceBP)
	q, r := bits.Div64(hi, lo, den) // hi < den since bp <= 1,000
	if r != 0 {
		q++
	}
	if q > 1<<63-1 {
		q = 1<<63 - 1
	}
	return max(max(o.Tolerance, 0), int64(q))
}

// nearRound decides a pass's near-matches over the state at its start.
// open lists the open lines to try. cands finds up to two candidate
// movements of a line, and rivals an open line other than li that has a
// movement among its candidates. It returns the pairs to match, marks the
// lines that had a candidate but no unique one, and records for each line
// whether its latest search was cut.
func nearRound(open []int, cands func(li int) ([]int, searchResult), rivals func(pi, li int) ([]int, searchResult),
	ambiguousLine, cutLine []bool) (pairs [][2]int) {
	for _, li := range open {
		cs, res := cands(li)
		if cutLine[li] = res == cut; cutLine[li] {
			continue
		}
		if len(cs) != 1 {
			ambiguousLine[li] = ambiguousLine[li] || len(cs) > 1
			continue
		}
		rs, res := rivals(cs[0], li)
		switch {
		case res == cut:
			cutLine[li] = true
		case len(rs) > 0:
			ambiguousLine[li] = true
		default:
			pairs = append(pairs, [2]int{li, cs[0]})
		}
	}
	return pairs
}

func satAdd(a, b int64) int64 {
	if s, ok := money.Add(a, b); ok {
		return s
	}
	if b > 0 {
		return 1<<63 - 1
	}
	return -1 << 63
}

func satSub(a, b int64) int64 {
	if s, ok := money.Sub(a, b); ok {
		return s
	}
	if b > 0 {
		return -1 << 63
	}
	return 1<<63 - 1
}
