package ledger

import (
	"fmt"
	"math/bits"
	"sort"
)

// maxViolations caps the report so a badly broken ledger can't produce an
// unbounded response.
const maxViolations = 100

// auditChunk is how many accounts or transfers the checker reads per hold of
// the read lock. A commit waits for at most one chunk, never for a scan.
var auditChunk = 1024

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

// frozen is a copy-on-write view of the committed state at one journal head.
// Taking it costs O(1): the journal and the list of account IDs are
// append-only, so their prefixes stand for themselves. What commit changes
// in place (an account's totals, version and history, or which version of a
// transfer the map points at) it first saves here, once per account or
// transfer, so the view keeps reading the values as of head. Reading live
// values that have no saved copy needs the ledger's read lock; the prefixes
// need no lock at all.
type frozen struct {
	st        *state
	head      uint64
	entries   []Entry  // st.entries[:head]
	ids       []string // st.order as of head
	open      int
	transfers int
	accounts  map[string]accountImage
	replaced  map[string]*Transfer
}

// accountImage is what commit changes in place on an account.
type accountImage struct {
	bal     Balance
	version uint64
	rows    []row
	life    []statusChange
	limits  []limitChange
}

func image(a *account) accountImage {
	return accountImage{bal: a.bal, version: a.version, rows: a.rows[:len(a.rows):len(a.rows)], life: a.life, limits: a.limits}
}

// keepAccount saves a's values before commit changes them for the first time
// since the view was taken. The caller holds the write lock.
func (v *frozen) keepAccount(a *account) {
	if _, ok := v.accounts[a.ID]; !ok {
		v.accounts[a.ID] = image(a)
	}
}

// keepTransfer saves the version of a transfer the view sees before commit
// replaces it for the first time. The caller holds the write lock.
func (v *frozen) keepTransfer(tr *Transfer) {
	if _, ok := v.replaced[tr.ID]; !ok {
		v.replaced[tr.ID] = tr
	}
}

// account returns an account's definition and its values as of head. The
// caller holds the read lock.
func (v *frozen) account(id string) (*account, accountImage, bool) {
	a, ok := v.st.accounts[id]
	if !ok {
		return nil, accountImage{}, false
	}
	if im, ok := v.accounts[id]; ok {
		return a, im, true
	}
	return a, image(a), true
}

// transfer returns a transfer as of head. The caller holds the read lock.
func (v *frozen) transfer(id string) (*Transfer, bool) {
	if tr, ok := v.replaced[id]; ok {
		return tr, true
	}
	tr, ok := v.st.transfers[id]
	if !ok || len(tr.Entries) == 0 || tr.Entries[0].Seq > v.head {
		return nil, false // created after the view was taken
	}
	return tr, true
}

// freeze takes a view of the committed state; commits made until thaw save
// what they overwrite into it.
func (l *Ledger) freeze() *frozen {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.st
	n, na := len(st.entries), len(st.order)
	v := &frozen{st: st, head: uint64(n), entries: st.entries[:n:n], ids: st.order[:na:na], open: st.open,
		transfers: len(st.transfers), accounts: map[string]accountImage{}, replaced: map[string]*Transfer{}}
	st.view = v
	return v
}

func (l *Ledger) thaw(v *frozen) {
	l.mu.Lock()
	v.st.view = nil
	l.mu.Unlock()
}

func (l *Ledger) step(name string) {
	if l.auditStep != nil {
		l.auditStep(name)
	}
}

// CheckInvariants verifies the whole ledger and returns a description of
// each violation (empty when everything holds):
//
//  1. every journal entry balances: debits equal credits, posted and pending;
//  2. per currency, the sum of all debits equals the sum of all credits;
//  3. every account's balance equals the fold of the journal entries that
//     touch it, and its running balances and version agree with that fold;
//  4. no entry took an account past the balance limit in force when it was
//     written, or further past it than the account already was, and the
//     account's limit history is well formed;
//  5. every account's pending totals equal the open pending transfers'
//     postings to it, and the open count matches;
//  6. every transfer's posted and reversed amounts are consistent with its
//     postings and its reversals;
//  7. every account's status history follows the life-cycle rules, no entry
//     moved money through an account while it was frozen or closed, and a
//     closed account has a zero balance.
//
// It checks a frozen view of the committed state (see frozen), so commits
// continue while it runs: they wait for at most one chunk of auditChunk
// lookups, never for the scan. Cost is linear in the journal size.
func (l *Ledger) CheckInvariants() []string {
	_, v := l.Audit()
	return v
}

// acctCheck is one account as of the view's head, with the fold of the
// journal entries that touch it.
type acctCheck struct {
	id      string
	cur     string
	normal  Direction
	init    bounds // the limits the account was opened with
	bal     Balance
	version uint64
	rows    []row
	life    []statusChange
	limits  []limitChange
	folded  Balance
}

// lifeProblem checks an account's status history: each change is allowed
// from the one before, sequences never go back or past head, and
// a closed account holds nothing. Times are not compared: the wall clock may
// step back between batches.
func lifeProblem(a *acctCheck, head uint64, bad func(string, ...any)) {
	from := StatusOpen
	var seq uint64
	for _, c := range a.life {
		if !validStatus(c.status) || !transitionAllowed(from, c.status) {
			bad("account %s goes from %s to %q", a.id, from, c.status)
			return
		}
		if c.seq < seq || c.seq > head {
			bad("account %s has a status change out of order", a.id)
			return
		}
		from, seq = c.status, c.seq
	}
	if from == StatusClosed && (a.bal.DebitsPosted != a.bal.CreditsPosted || a.bal.DebitsPending != 0 || a.bal.CreditsPending != 0) {
		bad("account %s is closed but holds %+v", a.id, a.bal)
	}
}

// Audit is CheckInvariants that also returns the journal head it checked.
func (l *Ledger) Audit() (head uint64, violations []string) {
	l.auditMu.Lock()
	defer l.auditMu.Unlock()
	v := l.freeze()
	defer l.thaw(v)
	l.step("frozen")

	var out []string
	bad := func(format string, args ...any) {
		if len(out) < maxViolations {
			out = append(out, fmt.Sprintf(format, args...))
		}
	}

	// Accounts as of head, read in chunks under the read lock.
	ids := append([]string(nil), v.ids...)
	sort.Strings(ids)
	accts := make([]acctCheck, 0, len(ids))
	for start := 0; start < len(ids); start += auditChunk {
		end := min(start+auditChunk, len(ids))
		l.mu.RLock()
		for _, id := range ids[start:end] {
			a, im, ok := v.account(id)
			if !ok {
				bad("account %s is listed but missing", id)
				continue
			}
			accts = append(accts, acctCheck{id: id, cur: a.Currency, normal: a.NormalBalance, init: initialBounds(&a.Account),
				bal: im.bal, version: im.version, rows: im.rows, life: im.life, limits: im.limits})
		}
		l.mu.RUnlock()
		l.step("accounts")
	}
	index := make(map[string]int, len(accts))
	for i := range accts {
		index[accts[i].id] = i
	}

	// The journal prefix never changes, so it needs no lock.
	var tids []string
	for i, e := range v.entries {
		if e.Seq != uint64(i+1) {
			bad("entry %d has sequence %d", i+1, e.Seq)
		}
		if e.Kind == KindPosted || e.Kind == KindPending || e.Kind == KindReversal {
			tids = append(tids, e.TransferID)
		}
		var dp, cp, dq, cq wide
		for _, ln := range e.Lines {
			var b *Balance
			k, ok := index[ln.Account]
			if ok {
				b = &accts[k].folded
			} else {
				b = &Balance{}
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
			if !ok || accts[k].cur != e.Currency {
				bad("entry %d posts to %s, which does not exist or holds another currency", e.Seq, ln.Account)
			}
		}
		if dp != cp || dq != cq {
			bad("entry %d does not balance", e.Seq)
		}
	}

	type totals struct{ dp, cp, dq, cq wide }
	byCurrency := map[string]*totals{}
	for i := range accts {
		a := &accts[i]
		id := a.id
		t := byCurrency[a.cur]
		if t == nil {
			t = &totals{}
			byCurrency[a.cur] = t
		}
		t.dp.add(a.bal.DebitsPosted)
		t.cp.add(a.bal.CreditsPosted)
		t.dq.add(a.bal.DebitsPending)
		t.cq.add(a.bal.CreditsPending)

		if a.folded != a.bal {
			bad("account %s balance %+v does not match its journal entries %+v", id, a.bal, a.folded)
		}
		if a.version != uint64(len(a.rows)) {
			bad("account %s version %d but %d entries", id, a.version, len(a.rows))
		}
		var run, prev Balance
		for _, r := range a.rows {
			if r.seq == 0 || r.seq > v.head || r.line >= len(v.entries[r.seq-1].Lines) || v.entries[r.seq-1].Lines[r.line].Account != id {
				bad("account %s has a history row that points at the wrong entry", id)
				break
			}
			ln := v.entries[r.seq-1].Lines[r.line]
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
			if err := boundsAt(a.init, a.limits, r.seq-1).check(id, a.normal, prev, run); err != nil {
				bad("entry %d broke the limit in force on account %s: %s", r.seq, id, err.Message)
				break
			}
			prev = run
			if c := statusAt(a.life, r.seq-1); c != nil {
				k := v.entries[r.seq-1].Kind
				if c.status == StatusClosed || (c.status == StatusFrozen && k != KindVoidPending && k != KindExpirePending) {
					bad("account %s was %s when entry %d (%s) touched it", id, c.status, r.seq, k)
					break
				}
			}
		}
		lifeProblem(a, v.head, bad)
		if a.bal.DebitsPosted < 0 || a.bal.CreditsPosted < 0 || a.bal.DebitsPending < 0 || a.bal.CreditsPending < 0 {
			bad("account %s has a negative total", id)
		}

		if limitHistoryProblem(a.limits, v.head) {
			bad("account %s has a limit history out of order or with a bad kind or amount", id)
		}
		// The balance itself, against the limits in force now and the
		// balance after its last entry: damage to the totals alone shows
		// here as well as in the fold.
		var last Balance
		if n := len(a.rows); n > 0 {
			last = a.rows[n-1].balance
		}
		if err := boundsAt(a.init, a.limits, v.head).check(id, a.normal, last, a.bal); err != nil {
			bad("account %s is past its limit: %s", id, err.Message)
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

	// Transfers: every one the view holds was created by exactly one entry
	// in the journal prefix, so the prefix lists them without a lock.
	sort.Strings(tids)
	n := 0
	for i, id := range tids {
		if i == 0 || id != tids[i-1] {
			tids[n] = id
			n++
		} else {
			bad("transfer %s is created by more than one journal entry", id)
		}
	}
	tids = tids[:n]
	if len(tids) != v.transfers {
		bad("the ledger holds %d transfers but the journal creates %d", v.transfers, len(tids))
	}
	type reversedSums struct {
		orig *Transfer
		sums []int64 // amounts sent back per posting
	}
	reserved := map[string]*Balance{}
	open := 0
	reversedBy := map[string]*reversedSums{} // original id -> what its reversals sent back
	withReversed := map[string]*Transfer{}   // transfers that say some amount was reversed
	type pair struct{ tr, orig *Transfer }
	chunk := make([]pair, 0, auditChunk)
	for start := 0; start < len(tids); start += auditChunk {
		end := min(start+auditChunk, len(tids))
		chunk = chunk[:0]
		l.mu.RLock()
		for _, id := range tids[start:end] {
			var p pair
			p.tr, _ = v.transfer(id)
			if p.tr != nil && p.tr.Reverses != "" {
				p.orig, _ = v.transfer(p.tr.Reverses)
			}
			chunk = append(chunk, p)
		}
		l.mu.RUnlock()
		l.step("transfers")
		for k, p := range chunk {
			tr := p.tr
			if tr == nil {
				bad("the journal creates transfer %s, which does not exist", tids[start+k])
				continue
			}
			id := tr.ID
			if tr.State == StatePending {
				open++
			}
			if b := tr.Balancing; b != nil && balancingProblem(tr) {
				bad("transfer %s says it balanced %d of %d, which its postings do not match", id, b.Amount, b.Requested)
			}
			for _, p := range tr.Postings {
				if p.Posted < 0 || p.Posted > p.Amount || p.Reversed < 0 || p.Reversed > p.Posted {
					bad("transfer %s posting to %s has inconsistent amounts", id, p.Account)
				}
				if tr.State != StatePosted && p.Posted != 0 {
					bad("transfer %s is %s but has posted amounts", id, tr.State)
				}
				if p.Reversed != 0 {
					withReversed[id] = tr
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
				orig := p.orig
				if orig == nil {
					bad("reversal %s points at missing transfer %s", id, tr.Reverses)
					continue
				}
				r := reversedBy[orig.ID]
				if r == nil {
					r = &reversedSums{orig: orig, sums: make([]int64, len(orig.Postings))}
					reversedBy[orig.ID] = r
				}
				for _, p := range tr.Postings {
					found := false
					for i, op := range orig.Postings {
						if op.Account == p.Account && op.Direction == p.Direction.flip() {
							r.sums[i] += p.Amount
							found = true
						}
					}
					if !found {
						bad("reversal %s posts to %s, which is not in %s", id, p.Account, orig.ID)
					}
				}
			}
		}
	}
	if open != v.open {
		bad("open pending count is %d but %d transfers are pending", v.open, open)
	}
	for i := range accts {
		a := &accts[i]
		var r Balance
		if p := reserved[a.id]; p != nil {
			r = *p
		}
		if a.bal.DebitsPending != r.DebitsPending || a.bal.CreditsPending != r.CreditsPending {
			bad("account %s reserves %d/%d (debit/credit) but open pending transfers hold %d/%d",
				a.id, a.bal.DebitsPending, a.bal.CreditsPending, r.DebitsPending, r.CreditsPending)
		}
	}
	// Only transfers that were reversed or say they were can disagree.
	rids := make([]string, 0, len(withReversed)+len(reversedBy))
	for id := range withReversed {
		rids = append(rids, id)
	}
	for id := range reversedBy {
		if _, ok := withReversed[id]; !ok {
			rids = append(rids, id)
		}
	}
	sort.Strings(rids)
	for _, id := range rids {
		tr, r := withReversed[id], reversedBy[id]
		if tr == nil {
			tr = r.orig
		}
		for i, p := range tr.Postings {
			var want int64
			if r != nil && i < len(r.sums) {
				want = r.sums[i]
			}
			if p.Reversed != want {
				bad("transfer %s posting to %s says %d reversed, its reversals add up to %d", id, p.Account, p.Reversed, want)
			}
		}
	}
	return v.head, out
}

// balancingProblem checks a balancing transfer's record against its
// postings: a side was named, both postings moved the amount it says, and
// that amount is positive and at most what was asked for.
func balancingProblem(tr *Transfer) bool {
	b := tr.Balancing
	if !b.Debit && !b.Credit || len(tr.Postings) != 2 || b.Amount <= 0 || b.Amount > b.Requested || tr.Reverses != "" {
		return true
	}
	for _, p := range tr.Postings {
		if p.Amount != b.Amount {
			return true
		}
	}
	return tr.Postings[0].Direction == tr.Postings[1].Direction
}
