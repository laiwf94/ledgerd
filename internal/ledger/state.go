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
	// life lists the account's status changes, oldest first; nil while it
	// has never left open. A new change replaces the slice, so readers may
	// keep the old one without a lock.
	life []statusChange
	// limits lists the account's set_limit changes, oldest first, replaced
	// the same way as life.
	limits []limitChange
}

// state is the committed ledger. Only the writer goroutine changes it, and
// only through txn.commit while holding the ledger's write lock.
type state struct {
	accounts  map[string]*account
	transfers map[string]*Transfer
	entries   []Entry // entries[i].Seq == i+1
	expiries  expiryHeap
	open      int      // pending transfers
	order     []string // account IDs in the order they were added; append-only
	ix        *indexes // secondary indexes; see index.go

	// view is set while the invariant checker scans a frozen copy of the
	// state. commit saves into it whatever it is about to overwrite.
	view *frozen
}

func newState() *state {
	return &state{accounts: map[string]*account{}, transfers: map[string]*Transfer{}, ix: newIndexes()}
}

func (s *state) head() uint64 { return uint64(len(s.entries)) }

// addAccount adds a new account.
func (s *state) addAccount(a *account) {
	s.accounts[a.ID] = a
	s.order = append(s.order, a.ID)
	s.ix.addAccount(&a.Account)
}

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
	sp        *savepoint // set while a batch runs; see begin
}

// savepoint is what a txn looked like when a batch began, so a failed batch
// can be taken back out of a txn that other commands share. It keeps the
// txn's own copy of each account and transfer the batch touches, from just
// before the first touch, and the lengths of the append-only parts.
type savepoint struct {
	entries, rows, expiries, order, open int
	accounts                             map[string]*account // nil: the txn had no copy
	transfers                            map[string]*Transfer
}

// begin starts a savepoint. Batches do not nest.
func (t *txn) begin() {
	t.sp = &savepoint{entries: len(t.entries), rows: len(t.rows), expiries: len(t.expiries), order: len(t.order),
		open: t.open, accounts: map[string]*account{}, transfers: map[string]*Transfer{}}
}

// keep drops the savepoint, keeping everything done since begin.
func (t *txn) keep() { t.sp = nil }

// rollback undoes everything done since begin. Entries, rows, expiries and
// new transfer IDs are only ever appended, so cutting them back is enough;
// accounts and transfers get back the copies saved at their first touch.
func (t *txn) rollback() {
	sp := t.sp
	t.sp = nil
	for id, a := range sp.accounts {
		if a == nil {
			delete(t.accounts, id)
		} else {
			t.accounts[id] = a
		}
	}
	for id, tr := range sp.transfers {
		if tr == nil {
			delete(t.transfers, id)
		} else {
			t.transfers[id] = tr
		}
	}
	clear(t.entries[sp.entries:])
	t.entries = t.entries[:sp.entries]
	clear(t.rows[sp.rows:])
	t.rows = t.rows[:sp.rows]
	t.expiries = t.expiries[:sp.expiries]
	t.order = t.order[:sp.order]
	t.open = sp.open
}

// saveAccount remembers the txn's copy of an account before a batch first
// touches it.
func (t *txn) saveAccount(id string) {
	if _, done := t.sp.accounts[id]; done {
		return
	}
	var prev *account
	if a, ok := t.accounts[id]; ok {
		c := *a
		prev = &c
	}
	t.sp.accounts[id] = prev
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
	if t.sp != nil {
		t.saveAccount(id)
	}
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
	if t.sp != nil {
		if _, done := t.sp.transfers[tr.ID]; !done {
			t.sp.transfers[tr.ID] = t.transfers[tr.ID]
		}
	}
	if _, ok := t.transfers[tr.ID]; !ok {
		t.order = append(t.order, tr.ID)
	}
	t.transfers[tr.ID] = tr
}

func (t *txn) commit() {
	v := t.st.view
	for id, c := range t.accounts {
		if a, ok := t.st.accounts[id]; ok {
			if v != nil {
				v.keepAccount(a)
			}
			a.bal, a.version, a.life, a.limits = c.bal, c.version, c.life, c.limits
		} else {
			t.st.addAccount(c)
		}
	}
	for _, r := range t.rows {
		a := t.st.accounts[r.account]
		a.rows = append(a.rows, r.row)
	}
	for _, id := range t.order {
		tr := t.transfers[id]
		if old, ok := t.st.transfers[id]; ok {
			if v != nil {
				v.keepTransfer(old)
			}
			t.st.ix.updateTransfer(old, tr)
		} else {
			t.st.ix.addTransfer(tr)
		}
		t.st.transfers[id] = tr
	}
	t.st.entries = append(t.st.entries, t.entries...)
	for _, e := range t.expiries {
		heap.Push(&t.st.expiries, e)
	}
	t.st.open += t.open
}

// exec executes one command. id is the ID for a transfer the command creates
// and ids the IDs for a batch's transfers: freshly generated when live, read
// from the log on replay.
func (t *txn) exec(c *Command, id string, ids []string) (Outcome, *Error) {
	if c.CreateBatch != nil {
		return t.createBatch(c.CreateBatch, ids)
	}
	if ids != nil {
		return Outcome{}, errf(CodeInvalid, "only a batch takes a list of transfer ids")
	}
	return t.apply(c, id)
}

// createBatch creates a batch's transfers in order, all or none. A refused
// transfer rolls back the ones before it, and the error says which one it
// was. Commands before and after the batch in the same group commit are
// unaffected.
func (t *txn) createBatch(s *BatchSpec, ids []string) (Outcome, *Error) {
	if len(ids) != len(s.Transfers) {
		return Outcome{}, errf(CodeInvalid, "a batch of %d transfers came with %d ids", len(s.Transfers), len(ids))
	}
	out := make([]*Transfer, len(s.Transfers))
	t.begin()
	for i := range s.Transfers {
		o, err := t.createTransfer(&s.Transfers[i], ids[i])
		if err != nil {
			t.rollback()
			return Outcome{}, itemError(i, err)
		}
		out[i] = o.Transfer
	}
	t.keep()
	return Outcome{Transfers: out}, nil
}

// apply executes one command other than a batch. id is the ID for a transfer
// the command creates.
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
	case c.IncreasePending != nil:
		return t.increasePending(c.IncreasePending)
	case c.ExtendPending != nil:
		return t.extendPending(c.ExtendPending)
	case c.Reverse != nil:
		return t.reverse(c.Reverse, id)
	case c.FreezeAccount != nil:
		return t.changeStatus(c.FreezeAccount, StatusFrozen)
	case c.UnfreezeAccount != nil:
		return t.changeStatus(c.UnfreezeAccount, StatusOpen)
	case c.CloseAccount != nil:
		return t.changeStatus(c.CloseAccount, StatusClosed)
	case c.SetLimit != nil:
		return t.setLimit(c.SetLimit)
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
		NonNegative: s.NonNegative, NonPositive: s.NonPositive, Exchange: s.Exchange, Metadata: copyMap(s.Metadata), CreatedAt: t.now}}
	t.accounts[s.ID] = a
	st := a.state(t.st.head() + uint64(len(t.entries)))
	return Outcome{Account: &st}, nil
}

func (a *account) state(asOf uint64) AccountState {
	s := AccountState{Account: a.Account, Version: a.version, Balance: a.bal.View(a.NormalBalance), AsOfSeq: asOf}
	if n := len(a.life); n > 0 {
		s.setStatus(&a.life[n-1])
	} else {
		s.Status = StatusOpen
	}
	s.Limits = limitsView(&a.Account, a.limits, asOf)
	return s
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
	if s.BalanceDebit || s.BalanceCredit {
		amount, err := t.balancingAmount(s, kind)
		if err != nil {
			return Outcome{}, err
		}
		tr.Balancing = &Balancing{Debit: s.BalanceDebit, Credit: s.BalanceCredit, Requested: s.Postings[0].Amount, Amount: amount}
	}
	lines := make([]Line, len(s.Postings))
	for i, p := range s.Postings {
		if tr.Balancing != nil {
			p.Amount = tr.Balancing.Amount
		}
		if p.Currency != "" && tr.FX == nil {
			tr.FX = &FX{Base: s.Currency, Quote: p.Currency, Rate: s.FX.Rate, Source: s.FX.Source, QuotedAt: s.FX.QuotedAt}
		}
		tr.Postings[i] = Posting{Account: p.Account, Direction: p.Direction, Currency: p.Currency, Amount: p.Amount}
		lines[i] = Line{Account: p.Account, Direction: p.Direction, Currency: p.Currency}
		if s.Pending {
			lines[i].Pending = p.Amount
		} else {
			lines[i].Posted = p.Amount
			tr.Postings[i].Posted = p.Amount
		}
	}
	seq, err := t.post(kind, id, s.Currency, lines, tr.FX, tr.FX)
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

// balancingAmount is what a balancing transfer moves: the amount asked for,
// cut down to the room of each side that may balance. Validate has made sure
// the transfer has two postings of the same amount, one each way.
// Status and currency are checked first, so a balancing transfer is refused
// for the same reasons, with the same errors, as any other.
func (t *txn) balancingAmount(s *TransferSpec, kind EntryKind) (int64, *Error) {
	amount := s.Postings[0].Amount
	for _, p := range s.Postings {
		if p.Direction == Debit && !s.BalanceDebit || p.Direction == Credit && !s.BalanceCredit {
			continue
		}
		a, ok := t.account(p.Account)
		if !ok {
			return 0, errf(CodeNotFound, "account %s does not exist", p.Account)
		}
		if err := a.usable(kind); err != nil {
			return 0, err
		}
		if a.Currency != s.Currency {
			return 0, errf(CodeCurrencyMismatch, "account %s holds %s, not %s", a.ID, a.Currency, s.Currency)
		}
		room := a.room(p.Direction)
		if room <= 0 {
			return 0, errf(CodeLimitExceeded, "account %s has nothing to balance against: a %s would take it past its limit", a.ID, p.Direction)
		}
		amount = min(amount, room)
	}
	return amount, nil
}

// room is the largest amount a line on side d, posted or pending, can move
// through the account without crossing its limits, counting a limit that is
// not set as zero (TigerBeetle's balancing rule, measured on the account's
// normal side). A line on the normal side raises the balance including
// incoming reservations, so the credit limit bounds it; a line on the other
// side lowers the available balance, so the overdraft limit bounds it. Both
// quantities move by exactly the line's amount whether it posts or reserves.
// The result is negative when the account is already past the limit.
func (a *account) room(d Direction) int64 {
	b := a.bounds()
	if d == a.NormalBalance {
		c := max(b.credit, 0)
		return satSub(c, ceilingOf(a.bal, a.NormalBalance))
	}
	o := max(b.overdraft, 0)
	return satAdd(floorOf(a.bal, a.NormalBalance), o)
}

// satAdd and satSub add and subtract, stopping at the int64 range.
func satAdd(x, y int64) int64 {
	if r, ok := money.Add(x, y); ok {
		return r
	}
	if y > 0 {
		return math.MaxInt64
	}
	return math.MinInt64
}

func satSub(x, y int64) int64 {
	if r, ok := money.Sub(x, y); ok {
		return r
	}
	if y < 0 {
		return math.MaxInt64
	}
	return math.MinInt64
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
		lines[i] = Line{Account: p.Account, Direction: p.Direction, Currency: p.Currency, Posted: amounts[i], Pending: -p.Amount}
	}
	seq, err := t.post(KindPostPending, tr.ID, tr.Currency, lines, nil, explicitRate(tr, s.Amounts))
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

// increasePending reserves more on a pending transfer's postings. The new
// lines go through the same checks as a new reservation: the accounts must be
// open, and no limit may be crossed. A balancing hold is refused, because its
// amount is what its limits allowed when it was made; a new balancing hold
// can take more.
func (t *txn) increasePending(s *IncreaseSpec) (Outcome, *Error) {
	tr, err := t.pendingTransfer(s.TransferID)
	if err != nil {
		return Outcome{}, err
	}
	if tr.Balancing != nil {
		return Outcome{}, errf(CodeInvalidState, "transfer %s is a balancing hold; its amount was cut to its limits, so make a new hold for more", tr.ID)
	}
	unbounded := make([]int64, len(tr.Postings))
	for i := range unbounded {
		unbounded[i] = math.MaxInt64
	}
	more, err := resolve(tr, s.Amounts, unbounded, "")
	if err != nil {
		return Outcome{}, err
	}
	var lines []Line
	for i, p := range tr.Postings {
		if more[i] > 0 {
			lines = append(lines, Line{Account: p.Account, Direction: p.Direction, Currency: p.Currency, Pending: more[i]})
		}
	}
	seq, err := t.post(KindIncreasePending, tr.ID, tr.Currency, lines, nil, tr.FX)
	if err != nil {
		return Outcome{}, err
	}
	// Each posting's amount is part of its account's pending total, which
	// post kept within an int64, so the sums below fit.
	c := tr.clone()
	for i := range c.Postings {
		c.Postings[i].Amount += more[i]
	}
	c.Entries = append(c.Entries, EntryRef{Seq: seq, Kind: KindIncreasePending})
	t.putTransfer(c)
	return Outcome{Transfer: c}, nil
}

// extendPending moves a pending transfer's timeout later. It writes an entry
// with no lines, so balances and account versions stay as they are, but the
// transfer's history and the journal both show the change. Like every other
// change to a reservation except its release, it is refused while one of the
// transfer's accounts is frozen. The old timeout stays in the expiry heap,
// where the sweep skips it because it no longer matches the transfer.
func (t *txn) extendPending(s *ExtendSpec) (Outcome, *Error) {
	tr, err := t.pendingTransfer(s.TransferID)
	if err != nil {
		return Outcome{}, err
	}
	at := t.now.Add(time.Duration(s.TimeoutSeconds) * time.Second)
	if !at.After(tr.ExpiresAt) {
		return Outcome{}, errf(CodeInvalidState, "transfer %s already expires at %s; timeout_seconds %d would not extend it",
			tr.ID, tr.ExpiresAt.Format(time.RFC3339), s.TimeoutSeconds)
	}
	for _, p := range tr.Postings {
		a, ok := t.account(p.Account)
		if !ok {
			return Outcome{}, errf(CodeNotFound, "account %s does not exist", p.Account)
		}
		if err := a.usable(KindExtendPending); err != nil {
			return Outcome{}, err
		}
	}
	seq := t.nextSeq()
	t.entries = append(t.entries, Entry{Seq: seq, Time: t.now, Kind: KindExtendPending, TransferID: tr.ID, Currency: tr.Currency, ExpiresAt: at})
	c := tr.clone()
	c.ExpiresAt = at
	c.Entries = append(c.Entries, EntryRef{Seq: seq, Kind: KindExtendPending})
	t.putTransfer(c)
	t.expiries = append(t.expiries, expiry{at: at, id: tr.ID})
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
		lines[i] = Line{Account: p.Account, Direction: p.Direction, Currency: p.Currency, Pending: -p.Amount}
	}
	seq, err := t.post(kind, tr.ID, tr.Currency, lines, nil, nil)
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
		lines = append(lines, Line{Account: p.Account, Direction: d, Currency: p.Currency, Posted: amounts[i]})
		rev.Postings = append(rev.Postings, Posting{Account: p.Account, Direction: d, Currency: p.Currency, Amount: amounts[i], Posted: amounts[i]})
	}
	// A reversal sends money back at the rate it went out at, so it carries
	// the original's rate, and is a multi-currency transfer itself when it
	// sends back both sides of the conversion.
	if orig.FX != nil && len(currenciesOf(rev.Postings)) == 2 {
		rev.FX = orig.FX
	}
	seq, err := t.post(KindReversal, id, orig.Currency, lines, rev.FX, explicitRate(orig, s.Amounts))
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
		// An increase has no upper limit per posting, so the sums can
		// overflow even though each amount fits. A multi-currency
		// transfer's amounts balance in each currency.
		var sums [2]struct{ debits, credits int64 }
		for i, p := range tr.Postings {
			k := 0
			if p.Currency != "" {
				k = 1
			}
			ok := true
			if p.Direction == Debit {
				sums[k].debits, ok = money.Add(sums[k].debits, out[i])
			} else {
				sums[k].credits, ok = money.Add(sums[k].credits, out[i])
			}
			if !ok {
				return nil, errf(CodeOverflow, "the amounts add up to more than %d", int64(math.MaxInt64))
			}
		}
		for k, cur := range tr.currencies() {
			if sums[k].debits == sums[k].credits {
				continue
			}
			if tr.FX == nil {
				return nil, errf(CodeInvalid, "debits (%d) must equal credits (%d)", sums[k].debits, sums[k].credits)
			}
			return nil, errf(CodeInvalid, "%s debits (%d) must equal %s credits (%d)", cur, sums[k].debits, cur, sums[k].credits)
		}
		return out, nil
	default:
		copy(out, limit)
		return out, nil
	}
}

// explicitRate is the rate a follow-up on a multi-currency transfer must
// agree with: the transfer's when the client named the amounts, and none when
// it takes what is left, which the earlier checked amounts already fix.
func explicitRate(tr *Transfer, a Amounts) *FX {
	if a.Amount == 0 && len(a.Postings) == 0 {
		return nil
	}
	return tr.FX
}

// currenciesOf lists the currencies of a transfer's postings: "" for the
// transfer's own, then the other one if any posting names it. A transfer is
// multi-currency when it has two.
func currenciesOf(ps []Posting) []string {
	out := []string{""}
	for _, p := range ps {
		if p.Currency != "" {
			return append(out, p.Currency)
		}
	}
	return out
}

// post appends a journal entry after checking every line: the account
// exists, its status allows the entry, it has the line's currency, no total
// overflows, and no balance limit is crossed. fx is stored with the entry.
// When conv is set, the amounts the lines move through exchange accounts
// must agree with its rate (see conversionProblem). Nothing changes unless
// every check passes.
func (t *txn) post(kind EntryKind, transferID, currency string, lines []Line, fx, conv *FX) (uint64, *Error) {
	accts := make([]*account, len(lines))
	next := make([]Balance, len(lines))
	for i, ln := range lines {
		a, ok := t.account(ln.Account)
		if !ok {
			return 0, errf(CodeNotFound, "account %s does not exist", ln.Account)
		}
		if err := a.usable(kind); err != nil {
			return 0, err
		}
		if cur := lineCurrency(currency, &ln); a.Currency != cur {
			return 0, errf(CodeCurrencyMismatch, "account %s holds %s, not %s", a.ID, a.Currency, cur)
		}
		nb, err := a.bal.with(ln)
		if err != nil {
			return 0, err
		}
		if err := a.bounds().check(a.ID, a.NormalBalance, a.bal, nb); err != nil {
			return 0, err
		}
		accts[i], next[i] = a, nb
	}
	if conv != nil {
		exchange := func(i int) bool { return accts[i].Exchange }
		if msg := conversionProblem(conv, currency, lines, conversionAmount(kind), exchange, fx != nil); msg != "" {
			return 0, errf(CodeInvalid, "%s", msg)
		}
	}
	seq := t.nextSeq()
	for i, a := range accts {
		a.bal = next[i]
		a.version++
		t.rows = append(t.rows, pendingRow{account: a.ID, row: row{seq: seq, line: i, balance: next[i]}})
	}
	t.entries = append(t.entries, Entry{Seq: seq, Time: t.now, Kind: kind, TransferID: transferID, Currency: currency, Lines: lines, FX: fx})
	return seq, nil
}

func lineCurrency(entry string, ln *Line) string {
	if ln.Currency != "" {
		return ln.Currency
	}
	return entry
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
