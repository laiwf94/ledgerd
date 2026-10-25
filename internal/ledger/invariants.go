package ledger

import (
	"fmt"
	"math/bits"
	"sort"
)

// maxViolations caps the report so a badly broken ledger can't produce an
// unbounded response.
const maxViolations = 100

// wide is a 128-bit two's-complement accumulator. Sums across many accounts
// can exceed an int64 even though every single total fits.
type wide struct {
	hi int64
	lo uint64
}

func (w *wide) add(v int64) {
	lo, carry := bits.Add64(w.lo, uint64(v), 0)
	w.hi += int64(carry)
	if v < 0 {
		w.hi--
	}
	w.lo = lo
}

// CheckInvariants verifies the whole ledger and returns a description of
// each violation (empty when everything holds):
//
//  1. every journal entry balances: debits equal credits, posted and pending;
//  2. per currency, the sum of all debits equals the sum of all credits;
//  3. every account's balance equals the fold of the journal entries that
//     touch it, and its running balances and version agree with that fold;
//  4. no limit-flagged account is past its limit;
//  5. every account's pending totals equal the open pending transfers'
//     postings to it, and the open count matches;
//  6. every transfer's posted and reversed amounts are consistent with its
//     postings and its reversals.
//
// It reads the committed state under the read lock, so it can run while the
// ledger serves traffic. Cost is linear in the journal size.
func (l *Ledger) CheckInvariants() []string {
	_, v := l.Audit()
	return v
}

// Audit is CheckInvariants that also returns the journal head it checked.
func (l *Ledger) Audit() (head uint64, violations []string) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	st := l.st
	var out []string
	bad := func(format string, args ...any) {
		if len(out) < maxViolations {
			out = append(out, fmt.Sprintf(format, args...))
		}
	}

	folded := map[string]*Balance{}
	for i, e := range st.entries {
		if e.Seq != uint64(i+1) {
			bad("entry %d has sequence %d", i+1, e.Seq)
		}
		var dp, cp, dq, cq wide
		for _, ln := range e.Lines {
			b := folded[ln.Account]
			if b == nil {
				b = &Balance{}
				folded[ln.Account] = b
			}
			if ln.Direction == Debit {
				dp.add(ln.Posted)
				dq.add(ln.Pending)
				b.DebitsPosted += ln.Posted
				b.DebitsPending += ln.Pending
			} else {
				cp.add(ln.Posted)
				cq.add(ln.Pending)
				b.CreditsPosted += ln.Posted
				b.CreditsPending += ln.Pending
			}
			if a, ok := st.accounts[ln.Account]; !ok || a.Currency != e.Currency {
				bad("entry %d posts to %s, which does not exist or holds another currency", e.Seq, ln.Account)
			}
		}
		if dp != cp || dq != cq {
			bad("entry %d does not balance", e.Seq)
		}
	}

	type totals struct{ dp, cp, dq, cq wide }
	byCurrency := map[string]*totals{}
	ids := make([]string, 0, len(st.accounts))
	for id := range st.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := st.accounts[id]
		t := byCurrency[a.Currency]
		if t == nil {
			t = &totals{}
			byCurrency[a.Currency] = t
		}
		t.dp.add(a.bal.DebitsPosted)
		t.cp.add(a.bal.CreditsPosted)
		t.dq.add(a.bal.DebitsPending)
		t.cq.add(a.bal.CreditsPending)

		var f Balance
		if p := folded[id]; p != nil {
			f = *p
		}
		if f != a.bal {
			bad("account %s balance %+v does not match its journal entries %+v", id, a.bal, f)
		}
		if a.version != uint64(len(a.rows)) {
			bad("account %s version %d but %d entries", id, a.version, len(a.rows))
		}
		var run Balance
		for _, r := range a.rows {
			if r.seq == 0 || r.seq > st.head() || r.line >= len(st.entries[r.seq-1].Lines) || st.entries[r.seq-1].Lines[r.line].Account != id {
				bad("account %s has a history row that points at the wrong entry", id)
				break
			}
			ln := st.entries[r.seq-1].Lines[r.line]
			if ln.Direction == Debit {
				run.DebitsPosted += ln.Posted
				run.DebitsPending += ln.Pending
			} else {
				run.CreditsPosted += ln.Posted
				run.CreditsPending += ln.Pending
			}
			if run != r.balance {
				bad("account %s running balance after entry %d is wrong", id, r.seq)
				break
			}
		}
		if a.bal.DebitsPosted < 0 || a.bal.CreditsPosted < 0 || a.bal.DebitsPending < 0 || a.bal.CreditsPending < 0 {
			bad("account %s has a negative total", id)
		}

		v := a.bal.View(a.NormalBalance)
		if a.NonNegative && v.Available < 0 {
			bad("account %s is non_negative but its available balance is %d", id, v.Available)
		}
		if ip, iq, dp, _ := a.bal.sides(a.NormalBalance); a.NonPositive && ip+iq-dp > 0 {
			bad("account %s is non_positive but its balance including pending is %d", id, ip+iq-dp)
		}
	}
	for cur, t := range byCurrency {
		if t.dp != t.cp {
			bad("%s: posted debits and credits differ across accounts", cur)
		}
		if t.dq != t.cq {
			bad("%s: pending debits and credits differ across accounts", cur)
		}
	}

	reserved := map[string]*Balance{}
	open := 0
	reversedBy := map[string][]int64{} // original id -> amounts sent back per posting
	tids := make([]string, 0, len(st.transfers))
	for id := range st.transfers {
		tids = append(tids, id)
	}
	sort.Strings(tids)
	for _, id := range tids {
		tr := st.transfers[id]
		if tr.State == StatePending {
			open++
		}
		for _, p := range tr.Postings {
			if p.Posted < 0 || p.Posted > p.Amount || p.Reversed < 0 || p.Reversed > p.Posted {
				bad("transfer %s posting to %s has inconsistent amounts", id, p.Account)
			}
			if tr.State != StatePosted && p.Posted != 0 {
				bad("transfer %s is %s but has posted amounts", id, tr.State)
			}
			if tr.State == StatePending {
				b := reserved[p.Account]
				if b == nil {
					b = &Balance{}
					reserved[p.Account] = b
				}
				if p.Direction == Debit {
					b.DebitsPending += p.Amount
				} else {
					b.CreditsPending += p.Amount
				}
			}
		}
		if tr.Reverses != "" {
			orig, ok := st.transfers[tr.Reverses]
			if !ok {
				bad("reversal %s points at missing transfer %s", id, tr.Reverses)
				continue
			}
			sums := reversedBy[orig.ID]
			if sums == nil {
				sums = make([]int64, len(orig.Postings))
				reversedBy[orig.ID] = sums
			}
			for _, p := range tr.Postings {
				found := false
				for i, op := range orig.Postings {
					if op.Account == p.Account && op.Direction == p.Direction.flip() {
						sums[i] += p.Amount
						found = true
					}
				}
				if !found {
					bad("reversal %s posts to %s, which is not in %s", id, p.Account, orig.ID)
				}
			}
		}
	}
	if open != st.open {
		bad("open pending count is %d but %d transfers are pending", st.open, open)
	}
	for _, id := range ids {
		a := st.accounts[id]
		var r Balance
		if p := reserved[id]; p != nil {
			r = *p
		}
		if a.bal.DebitsPending != r.DebitsPending || a.bal.CreditsPending != r.CreditsPending {
			bad("account %s reserves %d/%d (debit/credit) but open pending transfers hold %d/%d",
				id, a.bal.DebitsPending, a.bal.CreditsPending, r.DebitsPending, r.CreditsPending)
		}
	}
	for _, id := range tids {
		tr := st.transfers[id]
		sums := reversedBy[id]
		for i, p := range tr.Postings {
			var want int64
			if sums != nil {
				want = sums[i]
			}
			if p.Reversed != want {
				bad("transfer %s posting to %s says %d reversed, its reversals add up to %d", id, p.Account, p.Reversed, want)
			}
		}
	}
	return st.head(), out
}
