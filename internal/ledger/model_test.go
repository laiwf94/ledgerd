package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
)

// The reference model below is a deliberately plain, single-threaded ledger
// with big.Int balances that shares no code with the real one. We throw
// random concurrent workloads at the real ledger, read the order its writer
// chose back out of the log, replay the same commands into the model in that
// order, and require every result, balance and invariant to agree after
// every round.

var maxI64 = big.NewInt(math.MaxInt64)

// mNoLimit is the model's "no limit set".
const mNoLimit int64 = -1

type mAcct struct {
	id             string
	cur            string
	normal         Direction
	meta           map[string]string
	initOD, initCR int64 // limits the account was opened with, mNoLimit for none
	od, cr         int64 // limits in force now
	exchange       bool
	status         AccountStatus
	life           []mStatus
	limits         []mLimit
	rows           []mRow
	dp, cp, dq, cq *big.Int
	version        uint64
}

// mStatus, mLimit and mRow are the model's histories: a status change and a
// limit change made at journal head seq, and the totals after entry seq.
type mStatus struct {
	seq    uint64
	at     time.Time
	status AccountStatus
	reason string
}

type mLimit struct {
	seq    uint64
	kind   LimitKind
	amount int64
	reason string
}

type mRow struct {
	seq            uint64
	dp, cp, dq, cq int64
}

type mPosting struct {
	acct                     string
	dir                      Direction
	cur                      string // set only when not the transfer's currency
	amount, posted, reversed int64
}

// mFX is the rate a multi-currency transfer converts at, as the model keeps it.
type mFX struct {
	base, quote, rate, source string
	quotedAt                  time.Time
}

type mTransfer struct {
	id        string
	seq       uint64 // the entry that created it
	state     TransferState
	cur       string
	ref       string
	meta      map[string]string
	postings  []mPosting
	expiresAt time.Time
	reverses  string
	reversals []string
	fx        *mFX
	bal       *Balancing
	entries   []EntryRef
}

type mResult struct {
	code      Code
	item      int      // the refused transfer of a batch, -1 otherwise
	transfer  string   // created or changed transfer
	snapshot  string   // the transfer as returned, for comparison with replays
	transfers []string // a batch's transfers as returned
	account   string   // the account as returned by account commands
	cut       bool     // a balancing transfer moved less than it asked for
	fx        bool     // the transfer returned converts between currencies
}

func mFail(code Code) mResult { return mResult{code: code, item: -1} }

type model struct {
	accts map[string]*mAcct
	xfers map[string]*mTransfer
	order []string // transfer IDs in creation order
	head  uint64   // journal entries written
	now   time.Time
	// journal holds what each entry did: journal[i] is entry i+1. Which
	// transfer an entry belongs to is in that transfer's entries.
	journal []mEntry
}

// mEntry is the model's journal entry: its kind and its lines, or none for
// an extend_pending entry.
type mEntry struct {
	kind  EntryKind
	lines []mLine
}

func newModel(now time.Time) *model {
	return &model{accts: map[string]*mAcct{}, xfers: map[string]*mTransfer{}, now: now}
}

// sides returns pointers to the totals that raise and lower the normal balance.
func (a *mAcct) sides() (incP, incQ, decP, decQ *big.Int) {
	if a.normal == Debit {
		return a.dp, a.dq, a.cp, a.cq
	}
	return a.cp, a.cq, a.dp, a.dq
}

func (a *mAcct) available() *big.Int {
	ip, _, dp, dq := a.sides()
	r := new(big.Int).Sub(ip, dp)
	return r.Sub(r, dq)
}

func (a *mAcct) ceiling() *big.Int {
	ip, iq, dp, _ := a.sides()
	r := new(big.Int).Add(ip, iq)
	return r.Sub(r, dp)
}

func (a *mAcct) clone() *mAcct {
	c := *a
	c.dp, c.cp, c.dq, c.cq = new(big.Int).Set(a.dp), new(big.Int).Set(a.cp), new(big.Int).Set(a.dq), new(big.Int).Set(a.cq)
	return &c
}

type mLine struct {
	acct            string
	dir             Direction
	cur             string // the line's currency, always spelled out
	posted, pending int64
}

// apply checks every line in order (exists, status, currency, overflow,
// limits) and applies all of them as one journal entry, or none. A void or
// an expiry only gives back reservations, which a frozen account still
// allows. When conv is set, what the lines move through exchange accounts
// must agree with its rate; required says whether moving nothing through
// them is refused too (see convProblem).
func (m *model) apply(lines []mLine, kind EntryKind, conv *mFX, required bool) Code {
	release := kind == KindVoidPending || kind == KindExpirePending
	next := make([]*mAcct, len(lines))
	for i, ln := range lines {
		a, ok := m.accts[ln.acct]
		if !ok {
			return CodeNotFound
		}
		switch {
		case a.status == StatusClosed:
			return CodeAccountClosed
		case a.status == StatusFrozen && !release:
			return CodeAccountFrozen
		}
		if a.cur != ln.cur {
			return CodeCurrencyMismatch
		}
		n := a.clone()
		p, q := n.dp, n.dq
		if ln.dir == Credit {
			p, q = n.cp, n.cq
		}
		p.Add(p, big.NewInt(ln.posted))
		q.Add(q, big.NewInt(ln.pending))
		if p.Cmp(maxI64) > 0 || q.Cmp(maxI64) > 0 || new(big.Int).Add(p, q).Cmp(maxI64) > 0 {
			return CodeOverflow
		}
		if p.Sign() < 0 || q.Sign() < 0 {
			return CodeInvalidState
		}
		// An entry may not end past a limit and further past it than the
		// account already was.
		if a.od != mNoLimit {
			after := n.available()
			if after.Cmp(big.NewInt(-a.od)) < 0 && after.Cmp(a.available()) < 0 {
				return CodeLimitExceeded
			}
		}
		if a.cr != mNoLimit {
			after := n.ceiling()
			if after.Cmp(big.NewInt(a.cr)) > 0 && after.Cmp(a.ceiling()) > 0 {
				return CodeLimitExceeded
			}
		}
		n.version++
		next[i] = n
	}
	if conv != nil && m.convProblem(conv, lines, kind == KindPending || kind == KindIncreasePending, required) {
		return CodeInvalid
	}
	m.head++
	m.journal = append(m.journal, mEntry{kind: kind, lines: lines})
	for _, n := range next {
		n.rows = append(n.rows[:len(n.rows):len(n.rows)], mRow{seq: m.head, dp: n.dp.Int64(), cp: n.cp.Int64(), dq: n.dq.Int64(), cq: n.cq.Int64()})
		m.accts[n.id] = n
	}
	return ""
}

// mExponents are the minor-unit exponents of the currencies the workload
// uses, written out here rather than looked up.
var mExponents = map[string]int64{"USD": 2, "EUR": 2, "JPY": 0}

// mConvert is base minor units of currency from converted at rate into
// minor units of currency to, exactly.
func mConvert(base int64, from, to, rate string) *big.Rat {
	r, ok := new(big.Rat).SetString(rate)
	if !ok {
		panic("model: bad rate " + rate)
	}
	x := new(big.Rat).Mul(new(big.Rat).SetInt64(base), r)
	ten := func(e int64) *big.Rat {
		return new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(e), nil))
	}
	x.Mul(x, ten(mExponents[to]))
	return x.Quo(x, ten(mExponents[from]))
}

// convProblem reports whether lines disagree with the rate in fx: per
// currency the exchange accounts' net (credits minus debits) must be
// non-zero, the two nets of opposite signs, and the quote net within one
// minor unit of the base net converted. Lines that move nothing through
// exchange accounts pass unless required.
func (m *model) convProblem(fx *mFX, lines []mLine, pending, required bool) bool {
	nets := map[string]*big.Int{fx.base: new(big.Int), fx.quote: new(big.Int)}
	for _, ln := range lines {
		if !m.accts[ln.acct].exchange {
			continue
		}
		v := big.NewInt(ln.posted)
		if pending {
			v = big.NewInt(ln.pending)
		}
		if ln.dir == Debit {
			v.Neg(v)
		}
		nets[ln.cur].Add(nets[ln.cur], v)
	}
	b, q := nets[fx.base], nets[fx.quote]
	switch {
	case b.Sign() == 0 && q.Sign() == 0 && !required:
		return false
	case b.Sign() == 0 || q.Sign() == 0 || b.Sign() == q.Sign():
		return true
	case !b.IsInt64() || !q.IsInt64():
		return true
	}
	d := mConvert(new(big.Int).Abs(b).Int64(), fx.base, fx.quote, fx.rate)
	d.Sub(d, new(big.Rat).SetInt(new(big.Int).Abs(q)))
	return d.Abs(d).Cmp(big.NewRat(1, 1)) >= 0
}

// accountAt describes account a as of journal entry seq in the same form
// as accountState, from the model's own histories.
func (a *mAcct) at(seq uint64) string {
	var row mRow
	n := 0
	for _, r := range a.rows {
		if r.seq <= seq {
			row, n = r, n+1
		}
	}
	st := mStatus{status: StatusOpen}
	for _, s := range a.life {
		if s.seq <= seq {
			st = s
		}
	}
	od, cr, eff := a.initOD, a.initCR, uint64(0)
	for _, l := range a.limits {
		if l.seq <= seq {
			if l.kind == LimitOverdraft {
				od = l.amount
			} else {
				cr = l.amount
			}
			eff = l.seq + 1
		}
	}
	return describeAccount(a.id, a.cur, a.normal, a.exchange, a.meta, uint64(n), [4]int64{row.dp, row.cp, row.dq, row.cq},
		st.status, st.reason, st.at, od, cr, eff)
}

func describeAccount(id, cur string, normal Direction, exchange bool, meta map[string]string, version uint64, totals [4]int64,
	status AccountStatus, reason string, changed time.Time, od, cr int64, eff uint64) string {
	at := int64(0)
	if !changed.IsZero() {
		at = changed.UnixNano()
	}
	return fmt.Sprintf("%s %s %s exchange=%v %v v%d %v %s %q %d overdraft=%d credit=%d eff=%d", id, cur, normal, exchange, meta,
		version, totals, status, reason, at, od, cr, eff)
}

// accountState describes an account the ledger returned.
func accountState(s *AccountState) string {
	od, cr := mNoLimit, mNoLimit
	if s.Limits.Overdraft != nil {
		od = *s.Limits.Overdraft
	}
	if s.Limits.Credit != nil {
		cr = *s.Limits.Credit
	}
	b := s.Balance
	return describeAccount(s.ID, s.Currency, s.NormalBalance, s.Exchange, s.Metadata, s.Version,
		[4]int64{b.DebitsPosted, b.CreditsPosted, b.DebitsPending, b.CreditsPending},
		s.Status, s.StatusReason, s.StatusChangedAt, od, cr, s.Limits.EffectiveSeq)
}

func (m *model) snapshot(t *mTransfer) string {
	return describeTransfer(t.id, t.state, t.cur, t.postings, t.reverses, t.ref, t.meta, t.expiresAt, t.fx, t.bal, t.entries, t.reversals)
}

// describeTransfer gives a transfer the model holds and one the ledger
// returned the same form. Every comparison of a transfer goes through it,
// many per round, so it writes with strconv rather than fmt.
func describeTransfer(id string, state TransferState, cur string, ps []mPosting, reverses, ref string, meta map[string]string,
	expires time.Time, fx *mFX, bal *Balancing, entries []EntryRef, reversals []string) string {
	b := make([]byte, 0, 256)
	b = append(b, id...)
	b = append(b, ' ')
	b = append(b, state...)
	b = append(b, ' ')
	b = append(b, cur...)
	for _, p := range ps {
		b = append(b, " ["...)
		b = append(b, p.acct...)
		b = append(b, ' ')
		b = append(b, p.dir...)
		b = append(b, ' ')
		b = append(b, p.cur...)
		for _, v := range []int64{p.amount, p.posted, p.reversed} {
			b = append(b, ' ')
			b = strconv.AppendInt(b, v, 10)
		}
		b = append(b, ']')
	}
	b = append(b, " rev="...)
	b = append(b, reverses...)
	b = append(b, " ref="...)
	b = strconv.AppendQuote(b, ref)
	for _, k := range slices.Sorted(maps.Keys(meta)) {
		b = append(b, ' ')
		b = strconv.AppendQuote(b, k)
		b = append(b, '=')
		b = strconv.AppendQuote(b, meta[k])
	}
	b = append(b, " exp="...)
	if !expires.IsZero() {
		b = strconv.AppendInt(b, expires.UnixNano(), 10)
	}
	if fx != nil {
		b = append(b, " fx="...)
		b = append(b, fx.base...)
		b = append(b, '/')
		b = append(b, fx.quote...)
		b = append(b, '@')
		b = append(b, fx.rate...)
		b = append(b, ' ')
		b = strconv.AppendQuote(b, fx.source)
		b = append(b, ' ')
		b = strconv.AppendInt(b, fx.quotedAt.UnixNano(), 10)
		b = append(b, ' ')
		b = append(b, fx.quotedAt.Location().String()...)
	}
	if bal != nil {
		b = append(b, " bal="...)
		b = strconv.AppendBool(b, bal.Debit)
		b = append(b, '/')
		b = strconv.AppendBool(b, bal.Credit)
		b = append(b, ' ')
		b = strconv.AppendInt(b, bal.Requested, 10)
		b = append(b, ' ')
		b = strconv.AppendInt(b, bal.Amount, 10)
	}
	b = append(b, " entries="...)
	for _, e := range entries {
		b = strconv.AppendUint(b, e.Seq, 10)
		b = append(b, ':')
		b = append(b, e.Kind...)
		b = append(b, ' ')
	}
	b = append(b, "reversals="...)
	for _, r := range reversals {
		b = append(b, r...)
		b = append(b, ' ')
	}
	return string(b)
}

// lines turns amounts per posting of t into journal lines, each with its
// currency spelled out.
func (t *mTransfer) line(i int, dir Direction, posted, pending int64) mLine {
	p := t.postings[i]
	cur := p.cur
	if cur == "" {
		cur = t.cur
	}
	return mLine{acct: p.acct, dir: dir, cur: cur, posted: posted, pending: pending}
}

// pending returns the transfer id if it is pending, or why not.
func (m *model) pending(id string) (*mTransfer, Code) {
	t, ok := m.xfers[id]
	switch {
	case !ok:
		return nil, CodeNotFound
	case t.state == StateExpired:
		return nil, CodeExpired
	case t.state != StatePending:
		return nil, CodeInvalidState
	}
	return t, ""
}

// named reports whether an Amounts selector names amounts rather than
// taking everything there is.
func named(a Amounts) bool { return a.Amount > 0 || len(a.Postings) > 0 }

func (m *model) exec(c *Command, id string, ids ...string) mResult {
	switch {
	case c.CreateAccount != nil:
		s := c.CreateAccount
		if _, ok := m.accts[s.ID]; ok {
			return mFail(CodeAlreadyExists)
		}
		a := &mAcct{id: s.ID, cur: s.Currency, normal: s.NormalBalance, meta: mCopy(s.Metadata), initOD: mNoLimit, initCR: mNoLimit,
			exchange: s.Exchange, status: StatusOpen, dp: new(big.Int), cp: new(big.Int), dq: new(big.Int), cq: new(big.Int)}
		if s.NonNegative {
			a.initOD = 0
		}
		if s.NonPositive {
			a.initCR = 0
		}
		a.od, a.cr = a.initOD, a.initCR
		m.accts[s.ID] = a
		return mResult{item: -1, account: a.at(m.head)}
	case c.FreezeAccount != nil, c.UnfreezeAccount != nil, c.CloseAccount != nil:
		s, to := c.FreezeAccount, StatusFrozen
		if c.UnfreezeAccount != nil {
			s, to = c.UnfreezeAccount, StatusOpen
		} else if c.CloseAccount != nil {
			s, to = c.CloseAccount, StatusClosed
		}
		a, ok := m.accts[s.AccountID]
		switch {
		case !ok:
			return mFail(CodeNotFound)
		case a.status == StatusClosed:
			return mFail(CodeAccountClosed)
		case to == StatusFrozen && a.status != StatusOpen, to == StatusOpen && a.status != StatusFrozen:
			return mFail(CodeInvalidState)
		case to == StatusClosed && (a.dq.Sign() != 0 || a.cq.Sign() != 0 || a.dp.Cmp(a.cp) != 0):
			return mFail(CodePrecondition) // something still pending, or a posted balance
		}
		a.status = to
		a.life = append(a.life, mStatus{seq: m.head, at: m.now, status: to, reason: s.Reason})
		return mResult{item: -1, account: a.at(m.head)}
	case c.SetLimit != nil:
		s := c.SetLimit
		a, ok := m.accts[s.AccountID]
		switch {
		case !ok:
			return mFail(CodeNotFound)
		case a.status == StatusClosed:
			return mFail(CodeAccountClosed)
		}
		amount := mNoLimit
		if !s.Unlimited {
			amount = *s.Amount
		}
		if s.Kind == LimitOverdraft {
			a.od = amount
		} else {
			a.cr = amount
		}
		a.limits = append(a.limits, mLimit{seq: m.head, kind: s.Kind, amount: amount, reason: s.Reason})
		return mResult{item: -1, account: a.at(m.head)}
	case c.CreateTransfer != nil:
		return m.createTransfer(c.CreateTransfer, id)
	case c.CreateBatch != nil:
		// All or nothing: creating transfers only adds transfers and
		// replaces accounts (apply copies before it changes one), so
		// putting the maps and the head back undoes a failed batch.
		accts, xfers, order, head := maps.Clone(m.accts), maps.Clone(m.xfers), len(m.order), m.head
		res := mResult{item: -1}
		for i := range c.CreateBatch.Transfers {
			tid := fmt.Sprintf("~batch-%d", i) // a refused batch's record has no ids
			if i < len(ids) {
				tid = ids[i]
			}
			r := m.createTransfer(&c.CreateBatch.Transfers[i], tid)
			if r.code != "" {
				m.accts, m.xfers, m.order, m.head, m.journal = accts, xfers, m.order[:order], head, m.journal[:head]
				return mResult{code: r.code, item: i}
			}
			res.transfers = append(res.transfers, r.snapshot)
			res.cut = res.cut || r.cut
		}
		return res
	case c.Post != nil, c.Void != nil:
		tid := ""
		if c.Post != nil {
			tid = c.Post.TransferID
		} else {
			tid = c.Void.TransferID
		}
		t, code := m.pending(tid)
		if code != "" {
			return mFail(code)
		}
		amounts := make([]int64, len(t.postings))
		kind, to, conv := KindVoidPending, StateVoided, (*mFX)(nil)
		if c.Post != nil {
			kind, to = KindPostPending, StatePosted
			reserved := make([]int64, len(t.postings))
			for i, p := range t.postings {
				reserved[i] = p.amount
			}
			if amounts, code = m.choose(t, c.Post.Amounts, reserved); code != "" {
				return mFail(code)
			}
			if named(c.Post.Amounts) {
				conv = t.fx
			}
		}
		var lines []mLine
		for i, p := range t.postings {
			lines = append(lines, t.line(i, p.dir, amounts[i], -p.amount))
		}
		if code := m.apply(lines, kind, conv, false); code != "" {
			return mFail(code)
		}
		t.state = to
		if c.Post != nil {
			for i := range t.postings {
				t.postings[i].posted = amounts[i]
			}
		}
		t.entries = append(t.entries, EntryRef{Seq: m.head, Kind: kind})
		return mResult{item: -1, transfer: t.id, snapshot: m.snapshot(t)}
	case c.IncreasePending != nil:
		t, code := m.pending(c.IncreasePending.TransferID)
		if code != "" {
			return mFail(code)
		}
		if t.bal != nil {
			return mFail(CodeInvalidState) // a balancing hold's amount is what its limits allowed
		}
		unbounded := make([]int64, len(t.postings))
		for i := range unbounded {
			unbounded[i] = math.MaxInt64
		}
		more, code := m.choose(t, c.IncreasePending.Amounts, unbounded)
		if code != "" {
			return mFail(code)
		}
		var lines []mLine
		for i, p := range t.postings {
			if more[i] > 0 {
				lines = append(lines, t.line(i, p.dir, 0, more[i]))
			}
		}
		if code := m.apply(lines, KindIncreasePending, t.fx, false); code != "" {
			return mFail(code)
		}
		for i := range t.postings {
			t.postings[i].amount += more[i]
		}
		t.entries = append(t.entries, EntryRef{Seq: m.head, Kind: KindIncreasePending})
		return mResult{item: -1, transfer: t.id, snapshot: m.snapshot(t)}
	case c.ExtendPending != nil:
		t, code := m.pending(c.ExtendPending.TransferID)
		if code != "" {
			return mFail(code)
		}
		at := m.now.Add(time.Duration(c.ExtendPending.TimeoutSeconds) * time.Second)
		if !at.After(t.expiresAt) {
			return mFail(CodeInvalidState) // timeouts only move later
		}
		for _, p := range t.postings {
			a, ok := m.accts[p.acct]
			switch {
			case !ok:
				return mFail(CodeNotFound)
			case a.status == StatusClosed:
				return mFail(CodeAccountClosed)
			case a.status == StatusFrozen:
				return mFail(CodeAccountFrozen)
			}
		}
		// An entry with no lines: the journal moves on, no account does.
		m.head++
		m.journal = append(m.journal, mEntry{kind: KindExtendPending})
		t.expiresAt = at
		t.entries = append(t.entries, EntryRef{Seq: m.head, Kind: KindExtendPending})
		return mResult{item: -1, transfer: t.id, snapshot: m.snapshot(t)}
	case c.Reverse != nil:
		t, ok := m.xfers[c.Reverse.TransferID]
		if !ok {
			return mFail(CodeNotFound)
		}
		if t.reverses != "" || t.state != StatePosted {
			return mFail(CodeInvalidState)
		}
		left := make([]int64, len(t.postings))
		total := int64(0)
		for i, p := range t.postings {
			left[i] = p.posted - p.reversed
			total += left[i]
		}
		if total == 0 {
			return mFail(CodeInvalidState)
		}
		amounts, code := m.choose(t, c.Reverse.Amounts, left)
		if code != "" {
			return mFail(code)
		}
		r := &mTransfer{id: id, state: StatePosted, cur: t.cur, reverses: t.id, ref: c.Reverse.Reference, meta: mCopy(c.Reverse.Metadata)}
		var lines []mLine
		for i, p := range t.postings {
			if amounts[i] > 0 {
				d := Debit
				if p.dir == Debit {
					d = Credit
				}
				lines = append(lines, t.line(i, d, amounts[i], 0))
				r.postings = append(r.postings, mPosting{acct: p.acct, dir: d, cur: p.cur, amount: amounts[i], posted: amounts[i]})
				if p.cur != "" {
					r.fx = t.fx // sends back the other currency, so it converts back at the original's rate
				}
			}
		}
		var conv *mFX
		if named(c.Reverse.Amounts) {
			conv = t.fx
		}
		if code := m.apply(lines, KindReversal, conv, r.fx != nil); code != "" {
			return mFail(code)
		}
		r.seq = m.head
		r.entries = []EntryRef{{Seq: m.head, Kind: KindReversal}}
		for i := range t.postings {
			t.postings[i].reversed += amounts[i]
		}
		t.reversals = append(t.reversals, id)
		m.xfers[id] = r
		m.order = append(m.order, id)
		return mResult{item: -1, transfer: id, snapshot: m.snapshot(r), fx: r.fx != nil}
	}
	panic("empty command")
}

// createTransfer is create_transfer, on its own or as part of a batch.
func (m *model) createTransfer(s *TransferSpec, id string) mResult {
	for _, p := range s.Preconditions {
		a, ok := m.accts[p.Account]
		if !ok {
			return mFail(CodeNotFound)
		}
		if p.Version != nil && *p.Version != a.version {
			return mFail(CodePrecondition)
		}
		ip, _, dp, _ := a.sides()
		if p.PostedBalance != nil && new(big.Int).Sub(ip, dp).Cmp(big.NewInt(*p.PostedBalance)) != 0 {
			return mFail(CodePrecondition)
		}
	}
	kind := KindPosted
	if s.Pending {
		kind = KindPending
	}
	t := &mTransfer{id: id, state: StatePosted, cur: s.Currency, ref: s.Reference, meta: mCopy(s.Metadata)}
	if s.BalanceDebit || s.BalanceCredit {
		// The lesser of the request and each flagged side's room, an unset
		// limit counting as zero; the accounts' status and currency come
		// first.
		moved := big.NewInt(s.Postings[0].Amount)
		for _, p := range s.Postings {
			if p.Direction == Debit && !s.BalanceDebit || p.Direction == Credit && !s.BalanceCredit {
				continue
			}
			a, ok := m.accts[p.Account]
			switch {
			case !ok:
				return mFail(CodeNotFound)
			case a.status == StatusClosed:
				return mFail(CodeAccountClosed)
			case a.status == StatusFrozen:
				return mFail(CodeAccountFrozen)
			case a.cur != s.Currency:
				return mFail(CodeCurrencyMismatch)
			}
			room := a.room(p.Direction)
			if room.Sign() <= 0 {
				return mFail(CodeLimitExceeded)
			}
			if room.Cmp(moved) < 0 {
				moved = room
			}
		}
		t.bal = &Balancing{Debit: s.BalanceDebit, Credit: s.BalanceCredit, Requested: s.Postings[0].Amount, Amount: moved.Int64()}
	}
	for _, p := range s.Postings {
		amount := p.Amount
		if t.bal != nil {
			amount = t.bal.Amount
		}
		mp := mPosting{acct: p.Account, dir: p.Direction, cur: p.Currency, amount: amount}
		if !s.Pending {
			mp.posted = amount
		}
		if p.Currency != "" && t.fx == nil {
			t.fx = &mFX{base: s.Currency, quote: p.Currency, rate: s.FX.Rate, source: s.FX.Source, quotedAt: s.FX.QuotedAt}
		}
		t.postings = append(t.postings, mp)
	}
	var lines []mLine
	for i, p := range t.postings {
		lines = append(lines, t.line(i, p.dir, p.posted, p.amount-p.posted))
	}
	if code := m.apply(lines, kind, t.fx, t.fx != nil); code != "" {
		return mFail(code)
	}
	t.seq = m.head
	t.entries = []EntryRef{{Seq: m.head, Kind: kind}}
	if s.Pending {
		t.state = StatePending
		t.expiresAt = m.now.Add(time.Duration(s.TimeoutSeconds) * time.Second)
	}
	m.xfers[id] = t
	m.order = append(m.order, id)
	return mResult{item: -1, transfer: id, snapshot: m.snapshot(t), fx: t.fx != nil, cut: t.bal != nil && t.bal.Amount < t.bal.Requested}
}

// room is how much a posting on side d can move through the account
// without crossing its limits, an unset limit counting as zero.
func (a *mAcct) room(d Direction) *big.Int {
	if d == a.normal {
		return new(big.Int).Sub(big.NewInt(max(a.cr, 0)), a.ceiling())
	}
	return new(big.Int).Add(a.available(), big.NewInt(max(a.od, 0)))
}

func mCopy(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// choose turns an Amounts selector into one amount per posting, each at
// most limit[i], balanced in each currency.
func (m *model) choose(t *mTransfer, a Amounts, limit []int64) ([]int64, Code) {
	out := make([]int64, len(t.postings))
	if a.Amount > 0 {
		if len(t.postings) != 2 || a.Amount > limit[0] || a.Amount > limit[1] {
			return nil, CodeInvalid
		}
		out[0], out[1] = a.Amount, a.Amount
		return out, ""
	}
	if len(a.Postings) == 0 {
		copy(out, limit)
		return out, ""
	}
	for _, p := range a.Postings {
		found := false
		for i, tp := range t.postings {
			if tp.acct == p.Account {
				if p.Amount > limit[i] {
					return nil, CodeInvalid
				}
				out[i], found = p.Amount, true
			}
		}
		if !found {
			return nil, CodeInvalid
		}
	}
	type sides struct{ d, c *big.Int }
	sums := map[string]sides{}
	for i, p := range t.postings {
		s, ok := sums[p.cur]
		if !ok {
			s = sides{new(big.Int), new(big.Int)}
			sums[p.cur] = s
		}
		if p.dir == Debit {
			s.d.Add(s.d, big.NewInt(out[i]))
		} else {
			s.c.Add(s.c, big.NewInt(out[i]))
		}
	}
	for _, s := range sums {
		if s.d.Cmp(maxI64) > 0 || s.c.Cmp(maxI64) > 0 {
			return nil, CodeOverflow
		}
	}
	for _, s := range sums {
		if s.d.Cmp(s.c) != 0 {
			return nil, CodeInvalid
		}
	}
	return out, ""
}

// due lists pending transfers whose deadline has passed.
func (m *model) due() []string {
	var ids []string
	for id, t := range m.xfers {
		if t.state == StatePending && !t.expiresAt.After(m.now) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (m *model) expire(id string) {
	t := m.xfers[id]
	var lines []mLine
	for i, p := range t.postings {
		lines = append(lines, t.line(i, p.dir, 0, -p.amount))
	}
	if code := m.apply(lines, KindExpirePending, nil, false); code != "" {
		panic("model could not expire " + id)
	}
	t.state = StateExpired
	t.entries = append(t.entries, EntryRef{Seq: m.head, Kind: KindExpirePending})
}

type op struct {
	key  string
	cmd  Command
	hash [32]byte
	body []byte
}

func newOp(key string, c Command) op {
	body, _ := json.Marshal(c)
	return op{key: key, cmd: c, hash: sha256.Sum256(body), body: body}
}

// command returns a private copy, so concurrent Do calls never share specs.
func (o op) command() Command {
	var c Command
	if err := json.Unmarshal(o.body, &c); err != nil {
		panic(err)
	}
	return c
}

type gen struct {
	r     *rand.Rand
	m     *model
	n     int
	accts []string
	done  []op // ops from earlier rounds, for retries and key reuse
	fresh int  // accounts the generator has opened under new IDs
}

// Labels the generator puts on accounts and transfers, so every kind of
// listing filter has something to find. "missing" values match nothing.
var (
	genTiers   = []string{"gold", "silver"}
	genRegions = []string{"eu", "us"}
	genKinds   = []string{"payout", "fee", "refund"}
	genRefs    = []string{"ref-0", "ref-1", "ref-2", "ref-3", "ref-4"}
)

const maxFresh = 8

// modelSetup opens the accounts every model run starts with: every
// combination of normal side and limit in USD, a EUR and a JPY account, and
// an exchange account in each of the three currencies.
func modelSetup() []Command {
	return []Command{
		openAcct("a", "USD", Debit), openAcct("b", "USD", Credit), openAcct("c", "USD", Credit, "nonneg"),
		openAcct("d", "USD", Debit, "nonneg"), openAcct("e", "USD", Debit, "nonpos"), openAcct("f", "USD", Credit, "nonpos"),
		openAcct("g", "EUR", Credit), openAcct("j", "JPY", Credit),
		openAcct("xu", "USD", Credit, "exchange"), openAcct("xe", "EUR", Debit, "exchange"), openAcct("xj", "JPY", Debit, "exchange"),
	}
}

// newGen returns a generator over the accounts modelSetup opens, plus one
// that never exists.
func newGen(r *rand.Rand, m *model) *gen {
	return &gen{r: r, m: m, accts: []string{"a", "b", "c", "d", "e", "f", "g", "h", "j", "xu", "xe", "xj"}}
}

func (g *gen) labels() (string, map[string]string) {
	ref := ""
	if g.r.Intn(3) == 0 {
		ref = genRefs[g.r.Intn(len(genRefs))]
	}
	var meta map[string]string
	if g.r.Intn(3) == 0 {
		meta = map[string]string{"kind": genKinds[g.r.Intn(len(genKinds))]}
		if g.r.Intn(3) == 0 {
			meta["batch"] = fmt.Sprint(g.r.Intn(3))
		}
	}
	return ref, meta
}

// pickAccount returns an account, preferably one the model has in status
// pref, and sometimes one that does not exist.
func (g *gen) pickAccount(pref AccountStatus) string {
	if g.r.Intn(25) == 0 {
		return "acct_missing"
	}
	var match []string
	for _, id := range g.accts {
		if a, ok := g.m.accts[id]; ok && a.status == pref {
			match = append(match, id)
		}
	}
	if len(match) > 0 && g.r.Intn(4) != 0 {
		return match[g.r.Intn(len(match))]
	}
	return g.accts[g.r.Intn(len(g.accts))]
}

// lifecycle returns a freeze, unfreeze, close or set_limit command.
func (g *gen) lifecycle() Command {
	reason := ""
	if g.r.Intn(2) == 0 {
		reason = fmt.Sprintf("reason %d", g.r.Intn(100))
	}
	switch x := g.r.Intn(12); {
	case x < 3:
		return Command{FreezeAccount: &StatusSpec{AccountID: g.pickAccount(StatusOpen), Reason: reason}}
	case x < 6:
		return Command{UnfreezeAccount: &StatusSpec{AccountID: g.pickAccount(StatusFrozen), Reason: reason}}
	case x < 8:
		// Closing is final, so it aims at accounts the generator opened
		// itself, mostly at ones with nothing in them. The accounts every
		// run starts with are only tried while they hold a balance, so
		// the refusal is exercised without ending the workload.
		var empty, fresh, held []string
		for i, id := range g.accts {
			a, ok := g.m.accts[id]
			isEmpty := ok && a.dp.Cmp(a.cp) == 0 && a.dq.Sign() == 0 && a.cq.Sign() == 0
			switch {
			case i >= len(g.accts)-g.fresh:
				fresh = append(fresh, id)
				if isEmpty {
					empty = append(empty, id)
				}
			case ok && !isEmpty:
				held = append(held, id)
			}
		}
		id := "acct_missing"
		switch {
		case len(empty) > 0 && g.r.Intn(3) != 0:
			id = empty[g.r.Intn(len(empty))]
		case len(fresh) > 0 && g.r.Intn(3) != 0:
			id = fresh[g.r.Intn(len(fresh))]
		case len(held) > 0 && g.r.Intn(8) != 0:
			id = held[g.r.Intn(len(held))]
		}
		return Command{CloseAccount: &StatusSpec{AccountID: id, Reason: reason}}
	default:
		s := &LimitSpec{AccountID: g.pickAccount(StatusOpen), Kind: LimitOverdraft, Reason: reason}
		if g.r.Intn(2) == 0 {
			s.Kind = LimitCredit
		}
		switch y := g.r.Intn(10); {
		case y < 3:
			s.Unlimited = true
		case y < 4:
			v := int64(0)
			s.Amount = &v
		default:
			v := g.r.Int63n(3000) // often below what the account already uses
			s.Amount = &v
		}
		return Command{SetLimit: s}
	}
}

// openAccount reopens an existing ID or opens a new one with labels.
func (g *gen) openAccount() Command {
	cur := "USD"
	if g.r.Intn(5) == 0 {
		cur = "EUR"
	}
	if g.fresh == maxFresh || g.r.Intn(2) == 0 {
		return openAcct(g.accts[g.r.Intn(len(g.accts))], cur, Credit)
	}
	g.fresh++
	id := fmt.Sprintf("n%02d", g.fresh)
	g.accts = append(g.accts, id)
	s := &AccountSpec{ID: id, Currency: cur, NormalBalance: Credit, NonNegative: g.r.Intn(4) == 0}
	if g.r.Intn(2) == 0 {
		s.NormalBalance = Debit
	}
	if !s.NonNegative && g.r.Intn(5) == 0 {
		s.NonPositive = true
	}
	if g.r.Intn(3) != 0 {
		s.Metadata = map[string]string{"tier": genTiers[g.r.Intn(len(genTiers))]}
		if g.r.Intn(2) == 0 {
			s.Metadata["region"] = genRegions[g.r.Intn(len(genRegions))]
		}
	}
	return Command{CreateAccount: s}
}

func (g *gen) amount() int64 {
	switch g.r.Intn(20) {
	case 0:
		return math.MaxInt64/2 + g.r.Int63n(1000) // provokes overflow
	case 1:
		return 1
	default:
		return 1 + g.r.Int63n(300)
	}
}

func (g *gen) transferIDs() []string {
	ids := make([]string, 0, len(g.m.xfers))
	for id := range g.m.xfers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (g *gen) pickTransfer(pref TransferState) string {
	ids := g.transferIDs()
	if len(ids) == 0 || g.r.Intn(15) == 0 {
		return "tr_missing"
	}
	var match []string
	for _, id := range ids {
		if g.m.xfers[id].state == pref {
			match = append(match, id)
		}
	}
	if len(match) > 0 && g.r.Intn(5) != 0 {
		return match[g.r.Intn(len(match))]
	}
	return ids[g.r.Intn(len(ids))]
}

func (g *gen) amounts(id string) Amounts {
	t, ok := g.m.xfers[id]
	switch {
	case !ok || g.r.Intn(3) == 0:
		return Amounts{}
	case t.fx != nil && g.r.Intn(6) == 0:
		// The same amount on a debit in one currency and a credit in the
		// other: balanced in sum, not in each currency.
		var d, c []int
		for i, p := range t.postings {
			if p.dir == Debit && p.cur == "" {
				d = append(d, i)
			} else if p.dir == Credit && p.cur != "" {
				c = append(c, i)
			}
		}
		if len(d) == 0 || len(c) == 0 {
			return Amounts{}
		}
		i, j := d[g.r.Intn(len(d))], c[g.r.Intn(len(c))]
		v := 1 + g.r.Int63n(min(t.postings[i].amount, t.postings[j].amount))
		return Amounts{Postings: []PartialAmount{{Account: t.postings[i].acct, Amount: v}, {Account: t.postings[j].acct, Amount: v}}}
	case t.fx != nil && g.r.Intn(3) != 0 || g.r.Intn(4) == 0:
		of := make([]int64, len(t.postings))
		for i, p := range t.postings {
			of[i] = p.amount
			if t.state == StatePosted {
				of[i] = p.posted - p.reversed
			}
		}
		return g.scaled(t, of)
	case g.r.Intn(2) == 0:
		return Amounts{Amount: 1 + g.r.Int63n(t.postings[0].amount+2)}
	default:
		var ps []PartialAmount
		for _, p := range t.postings {
			if g.r.Intn(4) != 0 {
				ps = append(ps, PartialAmount{Account: p.acct, Amount: 1 + g.r.Int63n(p.amount+1)})
			}
		}
		if len(ps) == 0 {
			return Amounts{}
		}
		return Amounts{Postings: ps}
	}
}

// scaled names a quarter, half or three quarters of each amount in of,
// evened out so each currency balances. On a conversion that keeps the
// rate within rounding or misses it by a unit, so the model's rate check
// meets partial posts, increases and reversals that pass and fail it;
// sometimes it keeps only one currency's postings.
func (g *gen) scaled(t *mTransfer, of []int64) Amounts {
	num := 1 + g.r.Int63n(3)
	out := make([]int64, len(of))
	for i := range of {
		out[i] = of[i] * num / 4
	}
	curs := []string{""}
	if t.fx != nil {
		curs = append(curs, t.fx.quote)
	}
	drop := "-" // a currency to leave alone, "" for the transfer's own
	if len(curs) == 2 && g.r.Intn(4) == 0 {
		drop = curs[g.r.Intn(2)]
	}
	for _, cur := range curs {
		var d, c int64
		for i, p := range t.postings {
			if p.cur != cur {
				continue
			}
			if cur == drop {
				out[i] = 0
			}
			if p.dir == Debit {
				d += out[i]
			} else {
				c += out[i]
			}
		}
		for i, p := range t.postings {
			if p.cur == cur && (p.dir == Debit && d < c || p.dir == Credit && c < d) {
				out[i] += max(c-d, d-c)
				break
			}
		}
	}
	var ps []PartialAmount
	for i, p := range t.postings {
		if out[i] > 0 {
			ps = append(ps, PartialAmount{Account: p.acct, Amount: out[i]})
		}
	}
	if len(ps) == 0 {
		return Amounts{}
	}
	return Amounts{Postings: ps}
}

// genPairs are the conversions the generator makes, with a rate for each.
// JPY has no minor unit, so the rescaling goes both ways.
var genPairs = []struct{ base, quote, rate string }{
	{"USD", "EUR", "0.9234"}, {"EUR", "USD", "1.0831"}, {"USD", "JPY", "151.37"}, {"JPY", "USD", "0.0066225"}, {"EUR", "JPY", "163.9"},
}

// genExchange is the exchange account the run opens in each currency.
var genExchange = map[string]string{"USD": "xu", "EUR": "xe", "JPY": "xj"}

// holding returns an account the model has in currency cur that is not an
// exchange account and not one of not, or "" if there is none.
func (g *gen) holding(cur string, not ...string) string {
	var ids []string
	for _, id := range g.accts {
		if a, ok := g.m.accts[id]; ok && a.cur == cur && !a.exchange && !slices.Contains(not, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	return ids[g.r.Intn(len(ids))]
}

// conversion makes a multi-currency transfer: a customer sells base for
// quote through the exchange accounts, sometimes with a fee in the base
// currency, sometimes rounded the other way, sometimes off the rate or
// through an account that is not an exchange account.
func (g *gen) conversion() *TransferSpec {
	pair := genPairs[g.r.Intn(len(genPairs))]
	from, to := g.holding(pair.base), g.holding(pair.quote)
	xb, xq := genExchange[pair.base], genExchange[pair.quote]
	if from == "" || g.r.Intn(30) == 0 {
		from = "acct_missing"
	}
	if to == "" {
		to = "acct_missing_too"
	}
	if g.r.Intn(20) == 0 {
		if other := g.holding(pair.quote, to); other != "" {
			xq = other // no exchange account in the quote currency
		}
	}
	b := 1 + g.r.Int63n(5000)
	x := mConvert(b, pair.base, pair.quote, pair.rate)
	q := new(big.Int).Quo(x.Num(), x.Denom()).Int64() // rounded down
	if !x.IsInt() && g.r.Intn(2) == 0 {
		q++ // or up
	}
	switch g.r.Intn(8) {
	case 0:
		q += 1 + g.r.Int63n(3) // off the rate
	case 1:
		q -= 1 + g.r.Int63n(3)
	}
	q = max(q, 1)
	s := &TransferSpec{Currency: pair.base,
		FX: &FXSpec{Rate: pair.rate, Source: "ecb", QuotedAt: g.m.now.Add(-time.Duration(g.r.Intn(600)) * time.Second).UTC()}}
	fee := int64(0)
	if feeAcct := g.holding(pair.base, from); feeAcct != "" && g.r.Intn(4) == 0 {
		fee = 1 + g.r.Int63n(50)
		s.Postings = append(s.Postings, cr(feeAcct, fee))
	}
	s.Postings = append(s.Postings, dr(from, b+fee), cr(xb, b),
		PostingSpec{Account: xq, Direction: Debit, Amount: q, Currency: pair.quote},
		PostingSpec{Account: to, Direction: Credit, Amount: q, Currency: pair.quote})
	g.r.Shuffle(len(s.Postings), func(i, j int) { s.Postings[i], s.Postings[j] = s.Postings[j], s.Postings[i] })
	if g.r.Intn(3) == 0 {
		s.Pending, s.TimeoutSeconds = true, 1+g.r.Int63n(5)
	}
	s.Reference, s.Metadata = g.labels()
	return s
}

// transfer makes a create_transfer spec: mostly single-currency transfers
// of two to four postings, some balancing, some conversions.
func (g *gen) transfer() *TransferSpec {
	if g.r.Intn(6) == 0 {
		return g.conversion()
	}
	n := 2 + g.r.Intn(3)
	s := &TransferSpec{Currency: "USD"}
	if g.r.Intn(12) == 0 {
		s.Currency = "EUR"
	}
	// Mostly between open accounts in the transfer's currency, so frozen
	// and closed accounts and other currencies are refused often enough
	// without starving the rest.
	pool := g.accts
	if g.r.Intn(8) != 0 {
		var usable []string
		for _, id := range g.accts {
			if a, ok := g.m.accts[id]; !ok || a.status == StatusOpen && a.cur == s.Currency {
				usable = append(usable, id)
			}
		}
		if len(usable) >= n {
			pool = usable
		}
	}
	perm := g.r.Perm(len(pool))[:n]
	// Debits on the first legs, one credit leg carrying their total.
	total := int64(0)
	for _, i := range perm[:n-1] {
		a := g.amount()
		if total+a < 0 {
			a = 1
		}
		total += a
		s.Postings = append(s.Postings, dr(pool[i], a))
	}
	s.Postings = append(s.Postings, cr(pool[perm[n-1]], total))
	if n == 2 && g.r.Intn(25) == 0 {
		a := math.MaxInt64 - g.r.Int63n(100) // overflows any total already past 100
		s.Postings[0].Amount, s.Postings[1].Amount = a, a
	}
	if n == 2 && g.r.Intn(3) == 0 {
		// A balancing transfer, often a sweep asking for more than there is.
		switch g.r.Intn(3) {
		case 0:
			s.BalanceDebit = true
		case 1:
			s.BalanceCredit = true
		default:
			s.BalanceDebit, s.BalanceCredit = true, true
		}
		if g.r.Intn(2) == 0 {
			a := 1 + g.r.Int63n(5000)
			s.Postings[0].Amount, s.Postings[1].Amount = a, a
		}
	}
	g.r.Shuffle(len(s.Postings), func(i, j int) { s.Postings[i], s.Postings[j] = s.Postings[j], s.Postings[i] })
	if g.r.Intn(10) < 4 {
		s.Pending, s.TimeoutSeconds = true, 1+g.r.Int63n(5)
	}
	s.Reference, s.Metadata = g.labels()
	if g.r.Intn(8) == 0 {
		a := g.accts[g.r.Intn(len(g.accts))]
		if ma, ok := g.m.accts[a]; ok {
			v := ma.version
			if g.r.Intn(2) == 0 {
				v++
			}
			s.Preconditions = []Precondition{{Account: a, Version: &v}}
		}
	}
	return s
}

// batch makes an atomic batch of one to five transfers. Some fund an
// account and sweep it in the same batch, so a later transfer depends on
// an earlier one; any refusal anywhere must undo the lot.
func (g *gen) batch() *BatchSpec {
	b := &BatchSpec{}
	for range 1 + g.r.Intn(2) {
		b.Transfers = append(b.Transfers, *g.transfer())
	}
	if g.r.Intn(3) == 0 {
		from := g.holding("USD")
		mid := g.holding("USD", from)
		if to := g.holding("USD", from, mid); from != "" && mid != "" && to != "" {
			a := 1 + g.r.Int63n(400)
			sweep := 1 + g.r.Int63n(2000)
			b.Transfers = append(b.Transfers, TransferSpec{Currency: "USD", Postings: []PostingSpec{dr(from, a), cr(mid, a)}},
				TransferSpec{Currency: "USD", Postings: []PostingSpec{dr(mid, sweep), cr(to, sweep)}, BalanceDebit: true})
		}
	}
	g.r.Shuffle(len(b.Transfers)-1, func(i, j int) { b.Transfers[i], b.Transfers[j] = b.Transfers[j], b.Transfers[i] })
	return b
}

func (g *gen) next() op {
	g.n++
	k := fmt.Sprintf("op-%d", g.n)
	if g.r.Intn(100) < 8 {
		return newOp(k, g.lifecycle())
	}
	switch x := g.r.Intn(100); {
	case x < 4:
		return newOp(k, g.openAccount())
	case x < 38:
		return newOp(k, Command{CreateTransfer: g.transfer()})
	case x < 48:
		return newOp(k, Command{CreateBatch: g.batch()})
	case x < 59:
		id := g.pickTransfer(StatePending)
		return newOp(k, Command{Post: &PostSpec{TransferID: id, Amounts: g.amounts(id)}})
	case x < 66:
		return newOp(k, voidCmd(g.pickTransfer(StatePending)))
	case x < 72:
		id := g.pickTransfer(StatePending)
		a := Amounts{}
		if t, ok := g.m.xfers[id]; ok && g.r.Intn(3) != 0 {
			of := make([]int64, len(t.postings))
			for i, p := range t.postings {
				of[i] = min(p.amount, 400)
			}
			a = g.scaled(t, of)
		} else if g.r.Intn(2) == 0 {
			a = g.amounts(id)
		}
		if !named(a) {
			a = Amounts{Amount: 1 + g.r.Int63n(200)}
			if g.r.Intn(5) == 0 {
				a.Amount = math.MaxInt64 - g.r.Int63n(100) // overflows any total already past 100
			}
		}
		return newOp(k, Command{IncreasePending: &IncreaseSpec{TransferID: id, Amounts: a}})
	case x < 77:
		return newOp(k, Command{ExtendPending: &ExtendSpec{TransferID: g.pickTransfer(StatePending), TimeoutSeconds: 1 + g.r.Int63n(8)}})
	case x < 86:
		id := g.pickTransfer(StatePosted)
		rs := &ReverseSpec{TransferID: id, Amounts: g.amounts(id)}
		rs.Reference, rs.Metadata = g.labels()
		return newOp(k, Command{Reverse: rs})
	case x < 95 && len(g.done) > 0:
		return g.done[g.r.Intn(len(g.done))] // retry: same key, same request
	case len(g.done) > 0:
		prev := g.done[g.r.Intn(len(g.done))]
		o := newOp(prev.key, voidCmd("tr_different"))
		return o // same key, different request
	default:
		return newOp(k, voidCmd("tr_missing"))
	}
}

type actual struct {
	out Outcome
	err error
}

func codeOf(err error) Code {
	if err == nil {
		return ""
	}
	if le, ok := err.(*Error); ok {
		return le.Code
	}
	return Code("go:" + err.Error())
}

func transferSnapshot(tr *Transfer) string {
	ps := make([]mPosting, len(tr.Postings))
	for i, p := range tr.Postings {
		ps[i] = mPosting{acct: p.Account, dir: p.Direction, cur: p.Currency, amount: p.Amount, posted: p.Posted, reversed: p.Reversed}
	}
	var fx *mFX
	if tr.FX != nil {
		fx = &mFX{base: tr.FX.Base, quote: tr.FX.Quote, rate: tr.FX.Rate, source: tr.FX.Source, quotedAt: tr.FX.QuotedAt}
	}
	return describeTransfer(tr.ID, tr.State, tr.Currency, ps, tr.Reverses, tr.Reference, tr.Metadata, tr.ExpiresAt, fx, tr.Balancing,
		tr.Entries, tr.Reversals)
}

// readRecords returns the records from index from onwards in the log of
// data directory dir, and the index after the last one. The caller holds
// the ledger's dropMu, so no segment disappears while it reads.
func readRecords(t testing.TB, dir string, from uint64) ([]record, uint64) {
	t.Helper()
	return readRecordsFS(t, wal.OSFS{}, dir, from)
}

// readRecordsFS is readRecords on the file system fsys. It fails if the log
// no longer holds record from.
func readRecordsFS(t testing.TB, fsys wal.FS, dir string, from uint64) ([]record, uint64) {
	t.Helper()
	var recs []record
	first := uint64(0)
	next, err := wal.ReadFS(fsys, filepath.Join(dir, walDir), func(i uint64, p []byte) error {
		if first == 0 {
			first = i
		}
		if i < from {
			return nil
		}
		r, err := decodeRecord(p)
		if r.Type != recSnapshot { // markers say where a snapshot was taken, not what happened
			recs = append(recs, r)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if next < from {
		t.Fatalf("the log ends at record %d, before %d", next, from)
	}
	if from > 0 && first > from && next > from {
		t.Fatalf("the log starts at record %d, after %d", first, from)
	}
	return recs, next
}

func TestMatchesReferenceModel(t *testing.T) {
	seeds, rounds := 12, 120
	if testing.Short() {
		seeds, rounds = 4, 60
	}
	seen := map[string]int{}
	for seed := 1; seed <= seeds; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			if err := runModel(t, modelRun{seed: int64(seed), rounds: rounds, seen: seen}); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Logf("outcomes: %v", seen)
	// The workload must actually reach every outcome, or a passing run
	// proves little.
	for _, want := range []string{"ok:create_transfer", "ok:post", "ok:void", "ok:reverse", "expired", "crash", "replayed",
		string(CodeLimitExceeded), string(CodeOverflow), string(CodeNotFound), string(CodeInvalidState), string(CodeExpired),
		string(CodeCurrencyMismatch), string(CodePrecondition), string(CodeKeyReused), string(CodeInvalid), string(CodeAlreadyExists),
		"ok:create_account", "ok:freeze_account", "ok:unfreeze_account", "ok:close_account", "ok:set_limit",
		string(CodeAccountFrozen), string(CodeAccountClosed),
		"ok:create_batch", "batch-rolled-back", "ok:increase_pending", "ok:extend_pending",
		"balancing:ok", "balancing:cut", "balancing:no-room", "fx:ok", "fx:off-rate", "fx:reversed"} {
		if seen[want] == 0 {
			t.Errorf("the workload never produced %q (saw %v)", want, seen)
		}
	}
}

// recordDiffers compares what the log recorded for a command with what the
// model says it did: accepted or refused, the code, and for a batch the
// transfer that was refused.
func recordDiffers(rec record, res mResult) string {
	switch {
	case (rec.Type == recReject) != (res.code != ""), rec.Err != nil && rec.Err.Code != res.code:
		return fmt.Sprintf("ledger recorded %s %+v, model says %q", rec.Type, rec.Err, res.code)
	case rec.Err != nil && (rec.Err.Item == nil) != (res.item < 0), rec.Err != nil && rec.Err.Item != nil && *rec.Err.Item != res.item:
		return fmt.Sprintf("ledger refused item %v (%s), model refused item %d", rec.Err.Item, rec.Err.Message, res.item)
	case rec.Type == recCmd && len(rec.IDs) != len(res.transfers):
		return fmt.Sprintf("ledger recorded %d batch ids, model made %d transfers", len(rec.IDs), len(res.transfers))
	}
	return ""
}

// resultDiffers compares what the ledger returned with the model's result:
// the code, the refused item of a batch, and every transfer returned.
func resultDiffers(a actual, want mResult) string {
	if codeOf(a.err) != want.code {
		return fmt.Sprintf("got %v, model says %q", a.err, want.code)
	}
	if want.code != "" {
		le, _ := a.err.(*Error)
		if le == nil || (le.Item == nil) != (want.item < 0) || le.Item != nil && *le.Item != want.item {
			return fmt.Sprintf("got %+v, model refused item %d", a.err, want.item)
		}
		return ""
	}
	if want.transfer != "" && (a.out.Transfer == nil || transferSnapshot(a.out.Transfer) != want.snapshot) {
		return fmt.Sprintf("got transfer %+v,\nmodel returned %s", a.out.Transfer, want.snapshot)
	}
	if len(a.out.Transfers) != len(want.transfers) {
		return fmt.Sprintf("got %d transfers, model returned %d", len(a.out.Transfers), len(want.transfers))
	}
	for i, tr := range a.out.Transfers {
		if got := transferSnapshot(tr); got != want.transfers[i] {
			return fmt.Sprintf("batch transfer %d:\nledger %s\nmodel  %s", i, got, want.transfers[i])
		}
	}
	return ""
}

// noteOutcome counts what a command did, so a run can show it reached
// every outcome, including the ones only batches, balancing and
// conversions have.
func noteOutcome(seen map[string]int, c *Command, res mResult) {
	if res.code != "" {
		seen[string(res.code)]++
	} else {
		seen["ok:"+opName(c)]++
	}
	specs := []TransferSpec(nil)
	switch {
	case c.CreateTransfer != nil:
		specs = []TransferSpec{*c.CreateTransfer}
	case c.CreateBatch != nil:
		specs = c.CreateBatch.Transfers
		if res.code != "" && res.item > 0 {
			seen["batch-rolled-back"]++
		}
	}
	for i, s := range specs {
		done := res.code == "" || c.CreateBatch != nil && i < res.item
		refused := res.code != "" && (c.CreateBatch == nil || i == res.item)
		switch {
		case s.FX != nil && done:
			seen["fx:ok"]++
		case s.FX != nil && refused && res.code == CodeInvalid:
			seen["fx:off-rate"]++
		case (s.BalanceDebit || s.BalanceCredit) && done:
			seen["balancing:ok"]++
		case (s.BalanceDebit || s.BalanceCredit) && refused && res.code == CodeLimitExceeded:
			seen["balancing:no-room"]++
		}
	}
	if res.cut {
		seen["balancing:cut"]++
	}
	if res.fx && c.Reverse != nil {
		seen["fx:reversed"]++
	}
}

func opName(c *Command) string {
	switch {
	case c.CreateBatch != nil:
		return "create_batch"
	case c.IncreasePending != nil:
		return "increase_pending"
	case c.ExtendPending != nil:
		return "extend_pending"
	case c.CreateAccount != nil:
		return "create_account"
	case c.CreateTransfer != nil:
		return "create_transfer"
	case c.Post != nil:
		return "post"
	case c.Void != nil:
		return "void"
	case c.FreezeAccount != nil:
		return "freeze_account"
	case c.UnfreezeAccount != nil:
		return "unfreeze_account"
	case c.CloseAccount != nil:
		return "close_account"
	case c.SetLimit != nil:
		return "set_limit"
	}
	return "reverse"
}

// modelRun configures one run of the reference model. damage, when set,
// breaks the indexes after every round the way a bug in their upkeep would;
// the run then compares only the listings with the model and returns the
// first difference instead of failing.
type modelRun struct {
	seed   int64
	rounds int
	seen   map[string]int
	damage func(st *state)
}

func runModel(t *testing.T, cfg modelRun) error {
	seed, rounds, seen := cfg.seed, cfg.rounds, cfg.seen
	r := rand.New(rand.NewSource(seed))
	clock := newClock()
	ids := seqIDs()
	dir := t.TempDir()
	opts := Options{Dir: dir, Clock: clock.Now, NewID: ids, SweepInterval: -1, MaxBatch: 1 + r.Intn(16),
		SnapshotEvery: 1 + r.Intn(40), SegmentBytes: 256 + int64(r.Intn(4096))}
	l, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	// The test holds dropMu while it reads the log, so a snapshot finishing
	// in the background cannot drop segments it has not read yet. Drops
	// happen between rounds.
	held := false
	hold := func() { l.dropMu.Lock(); held = true }
	release := func() {
		if held {
			held = false
			l.dropMu.Unlock()
		}
	}
	defer func() { release(); l.Close() }()
	path := dir

	m := newModel(clock.Now())
	g := newGen(r, m)
	results := map[string]mResult{} // by idempotency key
	hashes := map[string][32]byte{}

	for i, c := range modelSetup() {
		o := newOp(fmt.Sprintf("setup-%d", i), c)
		if _, err := l.Do(context.Background(), o.command(), &Idem{Key: o.key, Hash: o.hash}); err != nil {
			t.Fatal(err)
		}
		m.exec(&c, "")
		results[o.key], hashes[o.key] = mResult{}, o.hash
	}
	hold()
	_, offset := readRecords(t, path, 0)
	release()

	for round := 0; round < rounds; round++ {
		hold()
		// Time passes; due reservations expire before anything else runs.
		if r.Intn(3) == 0 {
			clock.Advance(time.Duration(r.Intn(4)) * time.Second)
			m.now = clock.Now()
			if _, err := l.ExpireDue(context.Background()); err != nil {
				t.Fatal(err)
			}
			recs, end := readRecords(t, path, offset)
			offset = end
			var got []string
			for _, rec := range recs {
				if rec.Type != recExpire {
					t.Fatalf("round %d: unexpected %s record during expiry", round, rec.Type)
				}
				got = append(got, rec.ID)
			}
			sorted := slices.Sorted(slices.Values(got))
			if want := m.due(); fmt.Sprint(sorted) != fmt.Sprint(want) {
				t.Fatalf("round %d: expired %v, model expected %v", round, sorted, want)
			}
			// In log order, so each expiry gets the journal entry the
			// ledger gave it.
			for _, id := range got {
				m.expire(id)
				seen["expired"]++
			}
		}

		n := 1 + r.Intn(8)
		ops := make([]op, n)
		for i := range ops {
			ops[i] = g.next()
		}
		acts := make([]actual, n)
		var wg sync.WaitGroup
		for i := range ops {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				o := ops[i]
				out, err := l.Do(context.Background(), o.command(), &Idem{Key: o.key, Hash: o.hash})
				acts[i] = actual{out, err}
			}(i)
		}
		wg.Wait()

		// Every op carries a key, so even refusals are in the log, in the
		// order the writer applied them.
		recs, end := readRecords(t, path, offset)
		offset = end
		executed := map[string]bool{}
		for _, rec := range recs {
			if rec.Idem == nil {
				t.Fatalf("round %d: record without a key: %+v", round, rec)
			}
			k := rec.Idem.Key
			if executed[k] {
				t.Fatalf("round %d: key %s executed twice", round, k)
			}
			executed[k] = true
			var o *op
			for i := range ops {
				if ops[i].key == k && fmt.Sprintf("%x", ops[i].hash) == rec.Idem.Hash {
					o = &ops[i]
					break
				}
			}
			if o == nil {
				t.Fatalf("round %d: log has key %s that no operation sent", round, k)
			}
			c := o.command()
			if err := c.Validate(); err != nil {
				t.Fatalf("generator made an invalid command: %v", err)
			}
			res := m.exec(&c, rec.ID, rec.IDs...)
			if msg := recordDiffers(rec, res); msg != "" {
				t.Fatalf("round %d op %s (%s): %s", round, k, o.body, msg)
			}
			results[k], hashes[k] = res, o.hash
			noteOutcome(seen, &c, res)
		}
		for i, o := range ops {
			a := acts[i]
			want, known := results[o.key]
			switch {
			case hashes[o.key] != o.hash && known:
				if codeOf(a.err) != CodeKeyReused {
					t.Fatalf("round %d: key %s reused for a different request: got %v", round, o.key, a.err)
				}
				seen[string(CodeKeyReused)]++
				continue
			case !known:
				t.Fatalf("round %d: op %s has no result", round, o.key)
			}
			if msg := resultDiffers(a, want); msg != "" {
				t.Fatalf("round %d op %s (%s): %s", round, o.key, o.body, msg)
			}
			if a.out.Replayed {
				seen["replayed"]++
			}
			if !executed[o.key] && !a.out.Replayed {
				t.Fatalf("round %d op %s: a retry of an earlier request was not marked replayed", round, o.key)
			}
			if want.code == "" && want.account != "" {
				if a.out.Account == nil || accountState(a.out.Account) != want.account {
					t.Fatalf("round %d op %s: got account %+v, model returned %s", round, o.key, a.out.Account, want.account)
				}
			}
		}
		// Of the requests sharing a key executed this round, exactly one ran.
		for k := range executed {
			ran := 0
			for i, o := range ops {
				if o.key == k && hashes[k] == o.hash && !acts[i].out.Replayed {
					ran++
				}
			}
			if ran != 1 {
				t.Fatalf("round %d: %d requests with key %s executed", round, ran, k)
			}
		}
		for _, o := range ops {
			if hashes[o.key] == o.hash {
				g.done = append(g.done, o)
			}
		}
		if cfg.damage != nil {
			l.mu.Lock()
			cfg.damage(l.st)
			l.mu.Unlock()
			if err := checkListings(l, m, rand.New(rand.NewSource(int64(round)))); err != nil {
				return fmt.Errorf("round %d: %w", round, err)
			}
		} else {
			compare(t, round, l, m)
		}

		// Sometimes the machine crashes. Every acknowledged batch was fsynced,
		// so the log on disk is exactly what a crash would leave, plus
		// perhaps half of a batch that was being written.
		if r.Intn(10) == 0 {
			crashDir := t.TempDir()
			// Snapshot files survive the crash too, including one the
			// background writer may be halfway through. They are copied
			// before the log, as on a real disk a snapshot's log record is
			// durable before its file exists.
			copySnapshots(t, path, crashDir)
			copyLog(t, path, crashDir)
			if r.Intn(2) == 0 {
				torn := wal.Encode(nil, []byte(`{"t":"cmd","cmd":{"void":{"transfer_id":"x"}}}`))
				f, err := os.OpenFile(activeSegment(t, crashDir), os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				f.Write(torn[:1+r.Intn(len(torn)-1)])
				f.Close()
			}
			release()
			l.Close()
			opts.Dir = crashDir
			var err error
			if l, err = Open(opts); err != nil {
				t.Fatalf("round %d: recovery failed: %v", round, err)
			}
			hold()
			path = crashDir
			seen["crash"]++
			_, offset = readRecords(t, path, 0)
			if cfg.damage == nil {
				compare(t, round, l, m)
			}
		}
		release()
	}
	segs, _ := wal.SegmentPaths(filepath.Join(path, walDir))
	if l.log.FirstIndex() == 1 || len(segs) < 2 {
		t.Errorf("the workload never rolled and dropped segments: first record %d, %d segments", l.log.FirstIndex(), len(segs))
	}
	return nil
}

func compare(t *testing.T, round int, l *Ledger, m *model) {
	t.Helper()
	open := 0
	// Per currency, across all accounts: debits posted, credits posted,
	// debits pending, credits pending, from what the ledger returns.
	sums := map[string]*[4]big.Int{}
	for id, ma := range m.accts {
		a, err := l.Account(id, 0)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		b := a.Balance
		if sums[a.Currency] == nil {
			sums[a.Currency] = new([4]big.Int)
		}
		for i, v := range []int64{b.DebitsPosted, b.CreditsPosted, b.DebitsPending, b.CreditsPending} {
			sums[a.Currency][i].Add(&sums[a.Currency][i], big.NewInt(v))
		}
		got := []int64{b.DebitsPosted, b.CreditsPosted, b.DebitsPending, b.CreditsPending, b.Available}
		want := []int64{ma.dp.Int64(), ma.cp.Int64(), ma.dq.Int64(), ma.cq.Int64(), ma.available().Int64()}
		if fmt.Sprint(got) != fmt.Sprint(want) || a.Version != ma.version {
			t.Fatalf("round %d account %s: ledger %v v%d, model %v v%d", round, id, got, a.Version, want, ma.version)
		}
		if got, want := accountState(&a), ma.at(m.head); got != want {
			t.Fatalf("round %d account %s:\nledger %s\nmodel  %s", round, id, got, want)
		}
		// A point-in-time read: balance, version, status and limits as
		// of an earlier entry, from the model's own histories.
		if m.head > 0 {
			seq := 1 + (uint64(round)*2654435761+uint64(len(ma.rows))*40503+uint64(id[0]))%m.head
			a, err := l.Account(id, seq)
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			if got, want := accountState(&a), ma.at(seq); got != want {
				t.Fatalf("round %d account %s as of %d:\nledger %s\nmodel  %s", round, id, seq, got, want)
			}
		}
		_, hist, err := l.LimitHistory(id)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if got, want := describeLimits(hist), ma.describeLimits(); got != want {
			t.Fatalf("round %d account %s limit history:\nledger %s\nmodel  %s", round, id, got, want)
		}
	}
	// The property double entry exists for: in every currency on its own,
	// debits equal credits, posted and pending alike. Conversions never mix
	// one currency's units into another's totals.
	for cur, s := range sums {
		if s[0].Cmp(&s[1]) != 0 || s[2].Cmp(&s[3]) != 0 {
			t.Fatalf("round %d: %s debits and credits differ: posted %v and %v, pending %v and %v", round, cur,
				&s[0], &s[1], &s[2], &s[3])
		}
	}
	if err := checkListings(l, m, rand.New(rand.NewSource(int64(round)<<20+int64(m.head)))); err != nil {
		t.Fatalf("round %d: %v", round, err)
	}
	for id, mt := range m.xfers {
		tr, err := l.Transfer(id)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if got := transferSnapshot(tr); got != m.snapshot(mt) {
			t.Fatalf("round %d transfer %s:\nledger %s\nmodel  %s", round, id, got, m.snapshot(mt))
		}
		if mt.state == StatePending {
			open++
		}
	}
	if s := l.Stats(); s.Accounts != len(m.accts) || s.Transfers != len(m.xfers) || s.OpenPending != open {
		t.Fatalf("round %d: stats %+v, model has %d accounts %d transfers %d open", round, s, len(m.accts), len(m.xfers), open)
	}
	if v := allChecks(l); len(v) > 0 {
		t.Fatalf("round %d: invariants broken: %v", round, v)
	}
}
