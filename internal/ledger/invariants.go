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
//  1. every journal entry balances: debits equal credits in each currency,
//     posted and pending; an entry spans at most two currencies, and the
//     entry that creates a multi-currency transfer carries its rate, which
//     what the entry moves through exchange accounts agrees with;
//  2. per currency, the sum of all debits equals the sum of all credits;
//  3. every account's balance equals the fold of the journal entries that
//     touch it, and its running balances and version agree with that fold;
//  4. no entry took an account past the balance limit in force when it was
//     written, or further past it than the account already was, and the
//     account's limit history is well formed;
//  5. every account's pending totals equal the open pending transfers'
//     postings to it, and the open count matches;
//  6. every transfer's posted and reversed amounts are consistent with its
//     postings and its reversals, each posting is in its account's
//     currency, and a multi-currency transfer's rate is its entry's;
//  7. every hold's amounts are what its pending and increase entries
//     reserved, it was increased or extended only while open, and its
//     timeout is the one its last extension set;
//  8. every account's status history follows the life-cycle rules, no entry
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
	id       string
	cur      string
	exchange bool
	normal   Direction
	init     bounds // the limits the account was opened with
	bal      Balance
	version  uint64
	rows     []row
	life     []statusChange
	limits   []limitChange
	folded   Balance
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
			accts = append(accts, acctCheck{id: id, cur: a.Currency, exchange: a.Exchange, normal: a.NormalBalance, init: initialBounds(&a.Account),
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
		if extend := e.Kind == KindExtendPending; extend != (len(e.Lines) == 0) || extend == e.ExpiresAt.IsZero() {
			bad("entry %d (%s) has %d lines and timeout %v; only an extend_pending entry has a timeout, and it alone has no lines",
				e.Seq, e.Kind, len(e.Lines), e.ExpiresAt)
		}
		if e.Kind == KindIncreasePending {
			for _, ln := range e.Lines {
				if ln.Posted != 0 || ln.Pending <= 0 {
					bad("entry %d increases a hold but moves %d posted and %d pending on %s", e.Seq, ln.Posted, ln.Pending, ln.Account)
				}
			}
		}
		// sums[0] is the entry's own currency, sums[1] the other one of
		// a multi-currency entry.
		var sums [2]struct{ dp, cp, dq, cq wide }
		other := ""
		for j := range e.Lines {
			ln := &e.Lines[j]
			var b *Balance
			k, ok := index[ln.Account]
			if ok {
				b = &accts[k].folded
			} else {
				b = &Balance{}
			}
			c := 0
			if ln.Currency != "" {
				c = 1
				switch {
				case ln.Currency == e.Currency:
					bad("entry %d spells out its own currency on a line", e.Seq)
				case other == "":
					other = ln.Currency
				case ln.Currency != other:
					bad("entry %d has lines in more than two currencies", e.Seq)
				}
			}
			sm := &sums[c]
			if ln.Direction == Debit {
				sm.dp.add(ln.Posted)
				sm.dq.add(ln.Pending)
				b.DebitsPosted += ln.Posted
				b.DebitsPending += ln.Pending
			} else {
				sm.cp.add(ln.Posted)
				sm.cq.add(ln.Pending)
				b.CreditsPosted += ln.Posted
				b.CreditsPending += ln.Pending
			}
			if !ok || accts[k].cur != e.currency(ln) {
				bad("entry %d posts to %s, which does not exist or holds another currency", e.Seq, ln.Account)
			}
		}
		for _, sm := range sums {
			if sm.dp != sm.cp || sm.dq != sm.cq {
				bad("entry %d does not balance", e.Seq)
				break
			}
		}
		if msg := entryFXProblem(&e, other, func(j int) bool {
			k, ok := index[e.Lines[j].Account]
			return ok && accts[k].exchange
		}); msg != "" {
			bad("entry %d %s", e.Seq, msg)
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
			if msg := holdProblem(tr, v.entries); msg != "" {
				bad("transfer %s %s", id, msg)
			}
			if msg := transferFXProblem(tr, v.entries, func(id string) string {
				if k, ok := index[id]; ok {
					return accts[k].cur
				}
				return ""
			}); msg != "" {
				bad("transfer %s %s", id, msg)
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

// entryFXProblem checks an entry's rate: only an entry that creates a
// transfer carries one, it does exactly when its lines are in two currencies,
// its base is the entry's currency and its quote the other, and a posted or
// pending entry moves through exchange accounts what the rate says. A
// reversal is not held to the rate, since it may send back what is left
// after earlier partial reversals, each of which was.
func entryFXProblem(e *Entry, other string, exchange func(int) bool) string {
	creates := e.Kind == KindPosted || e.Kind == KindPending || e.Kind == KindReversal
	switch {
	case e.FX == nil && (other == "" || !creates):
		return ""
	case e.FX == nil:
		return fmt.Sprintf("is in %s and %s but has no rate", e.Currency, other)
	case !creates:
		return fmt.Sprintf("is a %s entry with a rate; only the entry that creates a transfer has one", e.Kind)
	case other == "":
		return fmt.Sprintf("has a rate but all its lines are in %s", e.Currency)
	case e.FX.Base != e.Currency || e.FX.Quote != other:
		return fmt.Sprintf("is in %s and %s but its rate is for %s to %s", e.Currency, other, e.FX.Base, e.FX.Quote)
	case e.Kind == KindReversal:
		return ""
	}
	return conversionProblem(e.FX, e.Currency, e.Lines, conversionAmount(e.Kind), exchange, true)
}

// transferFXProblem checks a transfer's currencies: each posting is in its
// account's currency, only a multi-currency transfer names a currency on a
// posting and it is the rate's quote, and the rate is the one the entry that
// created the transfer carries. currencyOf returns an account's currency.
func transferFXProblem(tr *Transfer, entries []Entry, currencyOf func(string) string) string {
	for i := range tr.Postings {
		p := &tr.Postings[i]
		if p.Currency != "" && (tr.FX == nil || p.Currency != tr.FX.Quote) {
			return fmt.Sprintf("posting to %s is in %s, which is not the transfer's other currency", p.Account, p.Currency)
		}
		if c := tr.currency(p); currencyOf(p.Account) != c {
			return fmt.Sprintf("posting to %s is in %s, but the account holds %q", p.Account, c, currencyOf(p.Account))
		}
	}
	if tr.FX != nil && len(currenciesOf(tr.Postings)) != 2 {
		return fmt.Sprintf("has a rate but all its postings are in %s", tr.Currency)
	}
	if len(tr.Entries) == 0 || tr.Entries[0].Seq == 0 || tr.Entries[0].Seq > uint64(len(entries)) {
		return "" // holdProblem reports it
	}
	if !sameFX(tr.FX, entries[tr.Entries[0].Seq-1].FX) {
		return fmt.Sprintf("has rate %+v but the entry that created it has %+v", tr.FX, entries[tr.Entries[0].Seq-1].FX)
	}
	return ""
}

func sameFX(a, b *FX) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Base == b.Base && a.Quote == b.Quote && a.Rate == b.Rate && a.Source == b.Source && a.QuotedAt.Equal(b.QuotedAt)
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

// holdProblem checks a transfer against the journal entries it lists: each
// one exists and belongs to it with the kind it says, increases and
// extensions appear only on a hold and only while it is open, each posting
// of a hold reserves exactly what its pending and increase entries put on its
// account, and the timeout is the one the last extension set. It returns ""
// when all of that holds.
func holdProblem(tr *Transfer, entries []Entry) string {
	hold := len(tr.Entries) > 0 && tr.Entries[0].Kind == KindPending
	var reserved []int64
	if hold {
		reserved = make([]int64, len(tr.Postings))
	}
	var extended *Entry
	open := hold
	for k, ref := range tr.Entries {
		if ref.Seq == 0 || ref.Seq > uint64(len(entries)) {
			return fmt.Sprintf("lists entry %d, which is not in the journal", ref.Seq)
		}
		e := &entries[ref.Seq-1]
		if e.TransferID != tr.ID || e.Kind != ref.Kind {
			return fmt.Sprintf("lists entry %d as %s, but it is %s of %s", ref.Seq, ref.Kind, e.Kind, e.TransferID)
		}
		switch ref.Kind {
		case KindIncreasePending, KindExtendPending:
			if !open || k == 0 {
				return fmt.Sprintf("has a %s entry %d while it is not an open hold", ref.Kind, ref.Seq)
			}
			if ref.Kind == KindExtendPending {
				if extended != nil && !e.ExpiresAt.After(extended.ExpiresAt) {
					return fmt.Sprintf("extension %d does not move its timeout later", ref.Seq)
				}
				extended = e
			}
		case KindPostPending, KindVoidPending, KindExpirePending:
			open = false
			continue
		}
		if !hold || (ref.Kind != KindPending && ref.Kind != KindIncreasePending) {
			continue
		}
		for _, ln := range e.Lines {
			found := false
			for i, p := range tr.Postings {
				if p.Account == ln.Account && p.Direction == ln.Direction {
					reserved[i] += ln.Pending
					found = true
					break
				}
			}
			if !found {
				return fmt.Sprintf("entry %d reserves on %s, which is not one of its postings", ref.Seq, ln.Account)
			}
		}
	}
	for i, p := range tr.Postings {
		if hold && p.Amount != reserved[i] {
			return fmt.Sprintf("posting to %s holds %d but its entries reserved %d", p.Account, p.Amount, reserved[i])
		}
	}
	if extended != nil && !tr.ExpiresAt.Equal(extended.ExpiresAt) {
		return fmt.Sprintf("expires at %v but its last extension set %v", tr.ExpiresAt, extended.ExpiresAt)
	}
	return ""
}
