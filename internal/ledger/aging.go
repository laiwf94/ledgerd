package ledger

import (
	"maps"
	"math/big"
	"math/bits"
	"slices"
	"sync"
	"time"

	"github.com/laiwf94/ledgerd/internal/money"
)

// Aging of open reconciliation breaks. A break stays open until a later run
// on the same account names its statement line or its movement again: the
// newer run's verdict on an item replaces the older one, so a break that a
// later statement matched is closed, and one that a later run found broken
// again is counted once, as that run's break. A reopened match is open too,
// with its line and movements, until a later run names one of them. Saved
// runs are checked against the journal and never edited, so the report is a
// pure function of the runs and the day it is taken on, and nothing new is
// stored for it.
//
// An open item's age is whole days from its earliest date (the statement
// line's booking date, the movement's journal date, the earlier of the two
// for an amount mismatch) to the report's day. Items dated after that day
// count as 0 days old.
//
// The report is computed from a summary of the open items grouped by
// account, currency, day and kind. The summary is rebuilt only when a run is
// saved or reviewed, without holding the ledger's lock, so scrapes of the
// per-bucket gauges and repeated requests cost a pass over the groups, not
// over every saved match and break.

// OpenReopenedMatch is the kind of open item a reopened match makes.
const OpenReopenedMatch = "reopened_match"

// AgingBucketSpec is one age band: MinDays to MaxDays inclusive, or MinDays
// and older when MaxDays is negative.
type AgingBucketSpec struct {
	Label   string
	MinDays int
	MaxDays int
}

// AgingBuckets are the report's age bands.
var AgingBuckets = []AgingBucketSpec{
	{"0-2", 0, 2},
	{"3-7", 3, 7},
	{"8-30", 8, 30},
	{"30+", 31, -1},
}

func agingBucket(age int64) int {
	for i, b := range AgingBuckets {
		if b.MaxDays < 0 || age <= int64(b.MaxDays) {
			return i
		}
	}
	return len(AgingBuckets) - 1
}

// AgingAmount is the total of a bucket's open items in one currency, in
// major units ("12.50"). An item counts with its size: a line's or a
// movement's amount without its sign, the difference for an amount
// mismatch and the line's amount for a reopened match.
type AgingAmount struct {
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
}

// AgingBucket is one age band of the report. MaxDays is nil for the last,
// open-ended band. Oldest is the earliest date of an item in the band.
type AgingBucket struct {
	Bucket             string        `json:"bucket"`
	MinDays            int           `json:"min_days"`
	MaxDays            *int          `json:"max_days"`
	OpenBreaks         int           `json:"open_breaks"`
	AmountMismatches   int           `json:"amount_mismatches"`
	MissingInLedger    int           `json:"missing_in_ledger"`
	MissingInStatement int           `json:"missing_in_statement"`
	ReopenedMatches    int           `json:"reopened_matches"`
	Amounts            []AgingAmount `json:"amounts"`
	Oldest             string        `json:"oldest,omitempty"`
}

// AgingReport is the open breaks of every saved run, or of one account's
// runs, by age as of a day.
type AgingReport struct {
	AsOf       string        `json:"as_of"`
	Account    string        `json:"account,omitempty"`
	OpenBreaks int           `json:"open_breaks"`
	Buckets    []AgingBucket `json:"buckets"`
}

// Open item kinds, as the summary keeps them.
const (
	openMismatch uint8 = iota
	openMissingInLedger
	openMissingInStatement
	openReopened
)

// agingGroup sums the open items of one account, day and kind. The size
// total is 128 bits wide: up to MaxReconItems items of up to 2^64 each.
type agingGroup struct {
	account  string
	currency string
	day      int64
	kind     uint8
	count    int
	hi, lo   uint64
}

// agingCache holds the summary and the runs it was built from. Committed
// runs never change (a review replaces the run), so the summary is current
// while the ledger holds the same run pointers.
type agingCache struct {
	mu     sync.Mutex
	runs   []*reconRun
	groups []agingGroup // sorted by account, then day
}

// openGroups summarises the open items of runs, numbered in order. Items
// belong to one account, so each account's runs are walked newest first
// with their own sets of the lines and movements a newer run named.
func openGroups(runs []*reconRun) []agingGroup {
	byAcct := map[string][]*reconRun{}
	for _, r := range runs {
		a := r.def.spec.Account
		byAcct[a] = append(byAcct[a], r)
	}
	type gk struct {
		day  int64
		kind uint8
	}
	var groups []agingGroup
	for _, acct := range slices.Sorted(maps.Keys(byAcct)) {
		list := byAcct[acct]
		// Sized for every item but the oldest run's, which are never looked up.
		nLines, nSeqs := 0, 0
		for _, r := range list[1:] {
			c := &r.def.counts
			nLines += c.StatementLines
			nSeqs += c.LedgerPostings
		}
		lines := make(map[string]struct{}, nLines)
		seqs := make(map[uint64]struct{}, nSeqs)
		idx := map[gk]int{}
		first := len(groups)
		add := func(d *reconDef, day int64, kind uint8, size uint64) {
			k := gk{day, kind}
			i, ok := idx[k]
			if !ok {
				i = len(groups)
				idx[k] = i
				groups = append(groups, agingGroup{account: acct, currency: d.currency, day: day, kind: kind})
			}
			g := &groups[i]
			g.count++
			var carry uint64
			g.lo, carry = bits.Add64(g.lo, size, 0)
			g.hi += carry
		}
		seenLine := func(ln *ReconLine) bool { _, ok := lines[ln.ExternalID]; return ok }
		seenSeq := func(p *ReconPosting) bool { _, ok := seqs[p.Seq]; return ok }
		for i := len(list) - 1; i >= 0; i-- {
			r := list[i]
			d := r.def
			s := &d.spec
			for k := range s.Breaks {
				b := &s.Breaks[k]
				if b.Line != nil && seenLine(b.Line) || b.Posting != nil && seenSeq(b.Posting) {
					continue
				}
				switch b.Kind {
				case BreakAmountMismatch:
					add(d, min(itemDay(b.Line.Date), itemDay(b.Posting.Date)), openMismatch, difference(b.Line.Amount, b.Posting.Amount))
				case BreakMissingInLedger:
					add(d, itemDay(b.Line.Date), openMissingInLedger, magnitude(b.Line.Amount))
				case BreakMissingInStatement:
					add(d, itemDay(b.Posting.Date), openMissingInStatement, magnitude(b.Posting.Amount))
				}
			}
			if r.status != nil {
			matches:
				for k := range s.Matches {
					if r.status[k].status != MatchReopened {
						continue
					}
					m := &s.Matches[k]
					if seenLine(&m.Line) {
						continue
					}
					day := itemDay(m.Line.Date)
					for j := range m.Postings {
						if seenSeq(&m.Postings[j]) {
							continue matches
						}
						day = min(day, itemDay(m.Postings[j].Date))
					}
					add(d, day, openReopened, magnitude(m.Line.Amount))
				}
			}
			if i == 0 {
				break // no older run to hide
			}
			// Only now, so that items of one run never hide each other.
			for k := range s.Breaks {
				b := &s.Breaks[k]
				if b.Line != nil {
					lines[b.Line.ExternalID] = struct{}{}
				}
				if b.Posting != nil {
					seqs[b.Posting.Seq] = struct{}{}
				}
			}
			for k := range s.Matches {
				m := &s.Matches[k]
				lines[m.Line.ExternalID] = struct{}{}
				for j := range m.Postings {
					seqs[m.Postings[j].Seq] = struct{}{}
				}
			}
		}
		slices.SortFunc(groups[first:], func(a, b agingGroup) int {
			if a.day != b.day {
				if a.day < b.day {
					return -1
				}
				return 1
			}
			return int(a.kind) - int(b.kind)
		})
	}
	return groups
}

// itemDay is a stored date as days since 1970. Saved runs hold only dates
// reconProblem accepted (YYYY-MM-DD, real, from 1970), so this reads the
// digits directly instead of going through time.Parse, which would dominate
// a rebuild.
func itemDay(date string) int64 {
	num := func(s string) int64 {
		var n int64
		for i := 0; i < len(s); i++ {
			n = n*10 + int64(s[i]-'0')
		}
		return n
	}
	y, m, d := num(date[0:4]), num(date[5:7]), num(date[8:10])
	// Days from civil (proleptic Gregorian), counting March as the first month.
	if m <= 2 {
		y--
	}
	era := y / 400
	yoe := y - era*400
	mp := (m + 9) % 12
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

func magnitude(v int64) uint64 {
	if v < 0 {
		return uint64(-(v + 1)) + 1 // safe for math.MinInt64
	}
	return uint64(v)
}

// difference is |a-b| without overflow; at most 2^64-1 for any int64s.
func difference(a, b int64) uint64 {
	if (a < 0) == (b < 0) {
		return magnitude(a - b) // same sign: cannot overflow
	}
	return magnitude(a) + magnitude(b)
}

// agingGroups returns the current summary, rebuilding it if a run was saved
// or reviewed since it was built.
func (l *Ledger) agingGroups() []agingGroup {
	l.mu.RLock()
	runs := slices.Clone(l.st.recons) // run pointers: reviews replace them, never edit them
	l.mu.RUnlock()
	c := &l.aging
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.groups == nil || !slices.Equal(c.runs, runs) {
		c.groups, c.runs = openGroups(runs), runs
		if c.groups == nil {
			c.groups = []agingGroup{}
		}
	}
	return c.groups
}

// ReconAging reports the open breaks of the saved runs by age as of asOf
// (YYYY-MM-DD; today, UTC, when empty), for one account or, when account is
// empty, for all.
func (l *Ledger) ReconAging(account, asOf string) (*AgingReport, error) {
	if account != "" {
		if !ValidID(account) {
			return nil, errf(CodeInvalid, "%q is not a valid account id", account)
		}
		l.mu.RLock()
		_, ok := l.st.accounts[account]
		l.mu.RUnlock()
		if !ok {
			return nil, errf(CodeNotFound, "account %s does not exist", account)
		}
	}
	if asOf == "" {
		asOf = l.now().Format(reconDateLayout)
	}
	today, ok := reconDay(asOf)
	if !ok || today < 0 {
		return nil, errf(CodeInvalid, "as_of must be a date (YYYY-MM-DD) from 1970-01-01")
	}
	return agingReport(l.agingGroups(), account, asOf, today), nil
}

// openBreaksByBucket counts all open items per bucket as of today, for the
// gauges.
func (l *Ledger) openBreaksByBucket() []int {
	today := l.now().Unix() / 86400
	out := make([]int, len(AgingBuckets))
	for _, g := range l.agingGroups() {
		out[agingBucket(max(today-g.day, 0))] += g.count
	}
	return out
}

func agingReport(groups []agingGroup, account, asOf string, today int64) *AgingReport {
	rep := &AgingReport{AsOf: asOf, Account: account, Buckets: make([]AgingBucket, len(AgingBuckets))}
	type sum struct{ hi, lo uint64 }
	totals := make([]map[string]*sum, len(AgingBuckets))
	oldest := make([]int64, len(AgingBuckets))
	for i, b := range AgingBuckets {
		rep.Buckets[i] = AgingBucket{Bucket: b.Label, MinDays: b.MinDays, Amounts: []AgingAmount{}}
		if b.MaxDays >= 0 {
			rep.Buckets[i].MaxDays = &b.MaxDays
		}
		totals[i] = map[string]*sum{}
		oldest[i] = -1
	}
	for i := range groups {
		g := &groups[i]
		if account != "" && g.account != account {
			continue
		}
		n := agingBucket(max(today-g.day, 0))
		b := &rep.Buckets[n]
		b.OpenBreaks += g.count
		rep.OpenBreaks += g.count
		switch g.kind {
		case openMismatch:
			b.AmountMismatches += g.count
		case openMissingInLedger:
			b.MissingInLedger += g.count
		case openMissingInStatement:
			b.MissingInStatement += g.count
		case openReopened:
			b.ReopenedMatches += g.count
		}
		t := totals[n][g.currency]
		if t == nil {
			t = &sum{}
			totals[n][g.currency] = t
		}
		var carry uint64
		t.lo, carry = bits.Add64(t.lo, g.lo, 0)
		t.hi += g.hi + carry
		if oldest[n] < 0 || g.day < oldest[n] {
			oldest[n] = g.day
		}
	}
	for i := range rep.Buckets {
		b := &rep.Buckets[i]
		if oldest[i] >= 0 {
			b.Oldest = time.Unix(oldest[i]*86400, 0).UTC().Format(reconDateLayout)
		}
		for _, cur := range slices.Sorted(maps.Keys(totals[i])) {
			t := totals[i][cur]
			b.Amounts = append(b.Amounts, AgingAmount{Currency: cur, Amount: formatWide(t.hi, t.lo, cur)})
		}
	}
	return rep
}

// formatWide renders a 128-bit count of minor units in major units.
func formatWide(hi, lo uint64, currency string) string {
	exp := 0
	if c, ok := money.Lookup(currency); ok {
		exp = c.Exponent
	}
	if hi == 0 && lo <= 1<<63-1 {
		return money.FormatDecimal(int64(lo), exp)
	}
	v := new(big.Int).Lsh(new(big.Int).SetUint64(hi), 64)
	v.Or(v, new(big.Int).SetUint64(lo))
	s := v.String()
	if exp > 0 {
		s = s[:len(s)-exp] + "." + s[len(s)-exp:] // at least 2^63, so longer than any exponent
	}
	return s
}
