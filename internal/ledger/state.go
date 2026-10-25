package ledger

import (
	"container/heap"
	"fmt"
	"math"
	"time"

	"github.com/laiwf94/ledgerd/internal/money"
)

// account is an account's definition plus its running state. The history
// slice is append-only, so readers may keep a prefix of it without a lock.
type account struct {
	Account
	bal     Balance
	version uint64
	rows    []row
}

// state is the committed ledger. Only the writer goroutine changes it, and
// only through txn.commit while holding the ledger's write lock.
type state struct {
	accounts  map[string]*account
	transfers map[string]*Transfer
	entries   []Entry // entries[i].Seq == i+1
	expiries  expiryHeap
	open      int // pending transfers
}

func newState() *state {
	return &state{accounts: map[string]*account{}, transfers: map[string]*Transfer{}}
}

func (s *state) head() uint64 { return uint64(len(s.entries)) }

// expiry schedules a pending transfer's timeout. Entries are removed lazily:
// one whose transfer is no longer pending is skipped when it surfaces.
type expiry struct {
	at time.Time
	id string
}

type expiryHeap []expiry

func (h expiryHeap) Len() int { return len(h) }
func (h expiryHeap) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].id < h[j].id
	}
	return h[i].at.Before(h[j].at)
}
func (h expiryHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *expiryHeap) Push(x any)   { *h = append(*h, x.(expiry)) }
func (h *expiryHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// txn is a batch of commands applied on top of the committed state without
// changing it. Readers keep seeing the committed state until the batch is on
// disk and commit runs; if the disk write fails, the txn is simply dropped.
//
// Every command checks everything it needs before it changes anything in the
// txn, so a rejected command leaves no trace and the next command in the
// batch sees exactly the effects of the accepted ones.
type txn struct {
	st        *state
	now       time.Time
	accounts  map[string]*account // copies of touched accounts, without rows
	transfers map[string]*Transfer
	order     []string // transfer IDs in the order they were written, for a deterministic commit
	entries   []Entry
	rows      []pendingRow
	expiries  []expiry
	open      int
}

type pendingRow struct {
	account string
	row     row
}

func newTxn(st *state, now time.Time) *txn {
	return &txn{st: st, now: now, accounts: map[string]*account{}, transfers: map[string]*Transfer{}}
}

func (t *txn) nextSeq() uint64 { return t.st.head() + uint64(len(t.entries)) + 1 }

// account returns the txn's copy of an account, copying it on first use.
func (t *txn) account(id string) (*account, bool) {
	if a, ok := t.accounts[id]; ok {
		return a, true
	}
	a, ok := t.st.accounts[id]
	if !ok {
		return nil, false
	}
	c := *a
	c.rows = nil
	t.accounts[id] = &c
	return &c, true
}

// transfer returns the newest version of a transfer for reading.
func (t *txn) transfer(id string) (*Transfer, bool) {
	if tr, ok := t.transfers[id]; ok {
		return tr, true
	}
	tr, ok := t.st.transfers[id]
	return tr, ok
}

// putTransfer records a new version of a transfer. Versions handed out in
// earlier outcomes are never modified.
func (t *txn) putTransfer(tr *Transfer) {
	if _, ok := t.transfers[tr.ID]; !ok {
		t.order = append(t.order, tr.ID)
	}
	t.transfers[tr.ID] = tr
}

func (t *txn) commit() {
	for id, c := range t.accounts {
		if a, ok := t.st.accounts[id]; ok {
			a.bal, a.version = c.bal, c.version
		} else {
			t.st.accounts[id] = c
		}
	}
	for _, r := range t.rows {
		a := t.st.accounts[r.account]
		a.rows = append(a.rows, r.row)
	}
	for _, id := range t.order {
		t.st.transfers[id] = t.transfers[id]
	}
	t.st.entries = append(t.st.entries, t.entries...)
	for _, e := range t.expiries {
		heap.Push(&t.st.expiries, e)
	}
	t.st.open += t.open
}

// apply executes one command. id is the ID for a transfer the command
// creates: freshly generated when live, read from the log on replay.
func (t *txn) apply(c *Command, id string) (Outcome, *Error) {
	switch {
	case c.CreateAccount != nil:
		return t.createAccount(c.CreateAccount)
	case c.CreateTransfer != nil:
		return t.createTransfer(c.CreateTransfer, id)
	case c.Post != nil:
		return t.postPending(c.Post)
	case c.Void != nil:
		return t.release(c.Void.TransferID, KindVoidPending, StateVoided)
	case c.Reverse != nil:
		return t.reverse(c.Reverse, id)
	}
	return Outcome{}, errf(CodeInvalid, "empty command")
}

func (t *txn) createAccount(s *AccountSpec) (Outcome, *Error) {
	if _, ok := t.account(s.ID); ok {
		return Outcome{}, errf(CodeAlreadyExists, "account %s already exists", s.ID)
	}
	cur, ok := money.Lookup(s.Currency)
	if !ok {
		return Outcome{}, errf(CodeInvalid, "unknown currency %q", s.Currency)
	}
	a := &account{Account: Account{ID: s.ID, Currency: cur.Code, Exponent: cur.Exponent, NormalBalance: s.NormalBalance,
		NonNegative: s.NonNegative, NonPositive: s.NonPositive, Metadata: copyMap(s.Metadata), CreatedAt: t.now}}
	t.accounts[s.ID] = a
	st := a.state(t.st.head() + uint64(len(t.entries)))
	return Outcome{Account: &st}, nil
}

func (a *account) state(asOf uint64) AccountState {
	return AccountState{Account: a.Account, Version: a.version, Balance: a.bal.View(a.NormalBalance), AsOfSeq: asOf}
}

func copyMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func (t *txn) createTransfer(s *TransferSpec, id string) (Outcome, *Error) {
	if !ValidID(id) {
		return Outcome{}, errf(CodeInvalid, "generated transfer id %q is not valid", id)
	}
	if _, ok := t.transfer(id); ok {
		return Outcome{}, errf(CodeAlreadyExists, "transfer %s already exists", id)
	}
	for _, p := range s.Preconditions {
		a, ok := t.account(p.Account)
		if !ok {
			return Outcome{}, errf(CodeNotFound, "account %s does not exist", p.Account)
		}
		if p.Version != nil && *p.Version != a.version {
			return Outcome{}, errf(CodePrecondition, "account %s is at version %d, not %d", p.Account, a.version, *p.Version)
		}
		if p.PostedBalance != nil {
			if got := a.bal.View(a.NormalBalance).Posted; got != *p.PostedBalance {
				return Outcome{}, errf(CodePrecondition, "account %s has posted balance %d, not %d", p.Account, got, *p.PostedBalance)
			}
		}
	}
	kind := KindPosted
	if s.Pending {
		kind = KindPending
	}
	tr := &Transfer{ID: id, State: StatePosted, Currency: s.Currency, Reference: s.Reference,
		Metadata: copyMap(s.Metadata), CreatedAt: t.now, Postings: make([]Posting, len(s.Postings))}
	lines := make([]Line, len(s.Postings))
	for i, p := range s.Postings {
		tr.Postings[i] = Posting{Account: p.Account, Direction: p.Direction, Amount: p.Amount}
		lines[i] = Line{Account: p.Account, Direction: p.Direction}
		if s.Pending {
			lines[i].Pending = p.Amount
		} else {
			lines[i].Posted = p.Amount
			tr.Postings[i].Posted = p.Amount
		}
	}
	seq, err := t.post(kind, id, s.Currency, lines)
	if err != nil {
		return Outcome{}, err
	}
	tr.Entries = []EntryRef{{Seq: seq, Kind: kind}}
	if s.Pending {
		tr.State = StatePending
		tr.ExpiresAt = t.now.Add(time.Duration(s.TimeoutSeconds) * time.Second)
		t.expiries = append(t.expiries, expiry{at: tr.ExpiresAt, id: id})
		t.open++
	}
	t.putTransfer(tr)
	return Outcome{Transfer: tr}, nil
}

// pendingTransfer returns a transfer that must currently be pending.
func (t *txn) pendingTransfer(id string) (*Transfer, *Error) {
	tr, ok := t.transfer(id)
	if !ok {
		return nil, errf(CodeNotFound, "transfer %s does not exist", id)
	}
	switch tr.State {
	case StatePending:
		return tr, nil
	case StateExpired:
		return nil, errf(CodeExpired, "transfer %s expired at %s and its reservation was released", id, tr.ExpiresAt.Format(time.RFC3339))
	default:
		return nil, errf(CodeInvalidState, "transfer %s is %s, not pending", id, tr.State)
	}
}

func (t *txn) postPending(s *PostSpec) (Outcome, *Error) {
	tr, err := t.pendingTransfer(s.TransferID)
	if err != nil {
		return Outcome{}, err
	}
	reserved := make([]int64, len(tr.Postings))
	for i, p := range tr.Postings {
		reserved[i] = p.Amount
	}
	amounts, err := resolve(tr, s.Amounts, reserved, "pending")
	if err != nil {
		return Outcome{}, err
	}
	lines := make([]Line, len(tr.Postings))
	for i, p := range tr.Postings {
		lines[i] = Line{Account: p.Account, Direction: p.Direction, Posted: amounts[i], Pending: -p.Amount}
	}
	seq, err := t.post(KindPostPending, tr.ID, tr.Currency, lines)
	if err != nil {
		return Outcome{}, err
	}
	c := tr.clone()
	c.State = StatePosted
	for i := range c.Postings {
		c.Postings[i].Posted = amounts[i]
	}
	c.Entries = append(c.Entries, EntryRef{Seq: seq, Kind: KindPostPending})
	t.putTransfer(c)
	t.open--
	return Outcome{Transfer: c}, nil
}

// release voids or expires a pending transfer.
func (t *txn) release(id string, kind EntryKind, to TransferState) (Outcome, *Error) {
	tr, err := t.pendingTransfer(id)
	if err != nil {
		return Outcome{}, err
	}
	lines := make([]Line, len(tr.Postings))
	for i, p := range tr.Postings {
		lines[i] = Line{Account: p.Account, Direction: p.Direction, Pending: -p.Amount}
	}
	seq, err := t.post(kind, tr.ID, tr.Currency, lines)
	if err != nil {
		return Outcome{}, err
	}
	c := tr.clone()
	c.State = to
	c.Entries = append(c.Entries, EntryRef{Seq: seq, Kind: kind})
	t.putTransfer(c)
	t.open--
	return Outcome{Transfer: c}, nil
}

func (t *txn) reverse(s *ReverseSpec, id string) (Outcome, *Error) {
	orig, ok := t.transfer(s.TransferID)
	if !ok {
		return Outcome{}, errf(CodeNotFound, "transfer %s does not exist", s.TransferID)
	}
	if orig.Reverses != "" {
		return Outcome{}, errf(CodeInvalidState, "transfer %s is itself a reversal; create a new transfer instead", orig.ID)
	}
	if orig.State != StatePosted {
		return Outcome{}, errf(CodeInvalidState, "transfer %s is %s; only posted transfers can be reversed", orig.ID, orig.State)
	}
	if !ValidID(id) {
		return Outcome{}, errf(CodeInvalid, "generated transfer id %q is not valid", id)
	}
	if _, ok := t.transfer(id); ok {
		return Outcome{}, errf(CodeAlreadyExists, "transfer %s already exists", id)
	}
	left := make([]int64, len(orig.Postings))
	any := false
	for i, p := range orig.Postings {
		left[i] = p.Posted - p.Reversed
		any = any || left[i] > 0
	}
	if !any {
		return Outcome{}, errf(CodeInvalidState, "transfer %s is already fully reversed", orig.ID)
	}
	amounts, err := resolve(orig, s.Amounts, left, "not yet reversed")
	if err != nil {
		return Outcome{}, err
	}
	rev := &Transfer{ID: id, State: StatePosted, Currency: orig.Currency, Reference: s.Reference,
		Metadata: copyMap(s.Metadata), CreatedAt: t.now, Reverses: orig.ID}
	var lines []Line
	for i, p := range orig.Postings {
		if amounts[i] == 0 {
			continue
		}
		d := p.Direction.flip()
		lines = append(lines, Line{Account: p.Account, Direction: d, Posted: amounts[i]})
		rev.Postings = append(rev.Postings, Posting{Account: p.Account, Direction: d, Amount: amounts[i], Posted: amounts[i]})
	}
	seq, err := t.post(KindReversal, id, orig.Currency, lines)
	if err != nil {
		return Outcome{}, err
	}
	rev.Entries = []EntryRef{{Seq: seq, Kind: KindReversal}}
	c := orig.clone()
	for i := range c.Postings {
		c.Postings[i].Reversed += amounts[i]
	}
	c.Reversals = append(c.Reversals, id)
	t.putTransfer(c)
	t.putTransfer(rev)
	return Outcome{Transfer: rev}, nil
}

// resolve turns an Amounts selector into one amount per posting, each at most
// limit[i], balanced, and not all zero. what names the limit in errors.
func resolve(tr *Transfer, a Amounts, limit []int64, what string) ([]int64, *Error) {
	out := make([]int64, len(tr.Postings))
	switch {
	case a.Amount > 0:
		if len(tr.Postings) != 2 {
			return nil, errf(CodeInvalid, "amount works for two-posting transfers; this one has %d postings, so give postings", len(tr.Postings))
		}
		for i := range out {
			if a.Amount > limit[i] {
				return nil, errf(CodeInvalid, "amount %d is more than the %d %s", a.Amount, limit[i], what)
			}
			out[i] = a.Amount
		}
		return out, nil
	case len(a.Postings) > 0:
		index := make(map[string]int, len(tr.Postings))
		for i, p := range tr.Postings {
			index[p.Account] = i
		}
		for _, p := range a.Postings {
			i, ok := index[p.Account]
			if !ok {
				return nil, errf(CodeInvalid, "account %s is not part of transfer %s", p.Account, tr.ID)
			}
			if p.Amount > limit[i] {
				return nil, errf(CodeInvalid, "amount %d for %s is more than the %d %s", p.Amount, p.Account, limit[i], what)
			}
			out[i] = p.Amount
		}
		var debits, credits int64 // each is at most a sum of the transfer's own balanced amounts, so no overflow
		for i, p := range tr.Postings {
			if p.Direction == Debit {
				debits += out[i]
			} else {
				credits += out[i]
			}
		}
		if debits != credits {
			return nil, errf(CodeInvalid, "debits (%d) must equal credits (%d)", debits, credits)
		}
		return out, nil
	default:
		copy(out, limit)
		return out, nil
	}
}

// post appends a journal entry after checking every line: the account
// exists, has the entry's currency, no total overflows, and no balance limit
// is crossed. Nothing changes unless every line passes.
func (t *txn) post(kind EntryKind, transferID, currency string, lines []Line) (uint64, *Error) {
	accts := make([]*account, len(lines))
	next := make([]Balance, len(lines))
	for i, ln := range lines {
		a, ok := t.account(ln.Account)
		if !ok {
			return 0, errf(CodeNotFound, "account %s does not exist", ln.Account)
		}
		if a.Currency != currency {
			return 0, errf(CodeCurrencyMismatch, "account %s holds %s, not %s", a.ID, a.Currency, currency)
		}
		nb, err := a.bal.with(ln)
		if err != nil {
			return 0, err
		}
		if err := a.checkLimits(a.bal, nb); err != nil {
			return 0, err
		}
		accts[i], next[i] = a, nb
	}
	seq := t.nextSeq()
	for i, a := range accts {
		a.bal = next[i]
		a.version++
		t.rows = append(t.rows, pendingRow{account: a.ID, row: row{seq: seq, line: i, balance: next[i]}})
	}
	t.entries = append(t.entries, Entry{Seq: seq, Time: t.now, Kind: kind, TransferID: transferID, Currency: currency, Lines: lines})
	return seq, nil
}

// with returns the balance after a line. Every total stays non-negative, and
// posted plus pending on each side stays within an int64, which is what lets
// View subtract without overflow.
func (b Balance) with(ln Line) (Balance, *Error) {
	posted, pending := &b.DebitsPosted, &b.DebitsPending
	if ln.Direction == Credit {
		posted, pending = &b.CreditsPosted, &b.CreditsPending
	}
	p, ok1 := money.Add(*posted, ln.Posted)
	q, ok2 := money.Add(*pending, ln.Pending)
	_, ok3 := money.Add(p, q)
	if !ok1 || !ok2 || !ok3 {
		return b, errf(CodeOverflow, "account %s: %s total would exceed %d", ln.Account, ln.Direction, int64(math.MaxInt64))
	}
	if q < 0 || p < 0 {
		return b, errf(CodeInvalidState, "account %s: %s total would go below zero", ln.Account, ln.Direction)
	}
	*posted, *pending = p, q
	return b, nil
}

// checkLimits refuses a change that takes a limit-flagged account past its
// limit, or further past it. A change that moves the balance back towards
// the limit is always allowed, so releasing or posting a reservation can't be
// blocked.
func (a *account) checkLimits(old, next Balance) *Error {
	if a.NonNegative {
		before, after := old.View(a.NormalBalance).Available, next.View(a.NormalBalance).Available
		if after < 0 && after < before {
			return errf(CodeLimitExceeded, "account %s may not go negative: available balance would be %d", a.ID, after)
		}
	}
	if a.NonPositive {
		ceiling := func(b Balance) int64 {
			ip, iq, dp, _ := b.sides(a.NormalBalance)
			return (ip + iq) - dp
		}
		before, after := ceiling(old), ceiling(next)
		if after > 0 && after > before {
			return errf(CodeLimitExceeded, "account %s may not go positive: balance including pending would be %d", a.ID, after)
		}
	}
	return nil
}

// sweep expires every pending transfer whose timeout is at or before now.
// It pops expired items off the committed heap directly: only the writer
// touches the heap, and a failed batch stops the ledger anyway.
func (t *txn) sweep() ([]*Transfer, error) {
	var out []*Transfer
	h := &t.st.expiries
	for h.Len() > 0 && !(*h)[0].at.After(t.now) {
		e := heap.Pop(h).(expiry)
		tr, ok := t.transfer(e.id)
		if !ok || tr.State != StatePending || !tr.ExpiresAt.Equal(e.at) {
			continue
		}
		o, err := t.release(e.id, KindExpirePending, StateExpired)
		if err != nil {
			return nil, fmt.Errorf("expiring %s: %v", e.id, err)
		}
		out = append(out, o.Transfer)
	}
	return out, nil
}
