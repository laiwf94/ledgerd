package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
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
	amount, posted, reversed int64
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
}

type mResult struct {
	code     Code
	transfer string // created or changed transfer
	snapshot string // the transfer as returned, for comparison with replays
	account  string // the account as returned by account commands
}

type model struct {
	accts map[string]*mAcct
	xfers map[string]*mTransfer
	order []string // transfer IDs in creation order
	head  uint64   // journal entries written
	now   time.Time
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
	posted, pending int64
}

// apply checks every line in order (exists, status, currency, overflow,
// limits) and applies all of them as one journal entry, or none. release
// marks an entry that only gives back reservations (a void or an expiry),
// which a frozen account still allows.
func (m *model) apply(cur string, lines []mLine, release bool) Code {
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
		if a.cur != cur {
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
	m.head++
	for _, n := range next {
		n.rows = append(n.rows[:len(n.rows):len(n.rows)], mRow{seq: m.head, dp: n.dp.Int64(), cp: n.cp.Int64(), dq: n.dq.Int64(), cq: n.cq.Int64()})
		m.accts[n.id] = n
	}
	return ""
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
	return describeAccount(a.id, a.cur, a.normal, a.meta, uint64(n), [4]int64{row.dp, row.cp, row.dq, row.cq},
		st.status, st.reason, st.at, od, cr, eff)
}

func describeAccount(id, cur string, normal Direction, meta map[string]string, version uint64, totals [4]int64,
	status AccountStatus, reason string, changed time.Time, od, cr int64, eff uint64) string {
	at := int64(0)
	if !changed.IsZero() {
		at = changed.UnixNano()
	}
	return fmt.Sprintf("%s %s %s %v v%d %v %s %q %d overdraft=%d credit=%d eff=%d", id, cur, normal, meta, version, totals,
		status, reason, at, od, cr, eff)
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
	return describeAccount(s.ID, s.Currency, s.NormalBalance, s.Metadata, s.Version,
		[4]int64{b.DebitsPosted, b.CreditsPosted, b.DebitsPending, b.CreditsPending},
		s.Status, s.StatusReason, s.StatusChangedAt, od, cr, s.Limits.EffectiveSeq)
}

func (m *model) snapshot(t *mTransfer) string {
	return fmt.Sprintf("%s %s %s %v %s %q %v", t.id, t.state, t.cur, t.postings, t.reverses, t.ref, t.meta)
}

func (m *model) exec(c *Command, id string) mResult {
	switch {
	case c.CreateAccount != nil:
		s := c.CreateAccount
		if _, ok := m.accts[s.ID]; ok {
			return mResult{code: CodeAlreadyExists}
		}
		a := &mAcct{id: s.ID, cur: s.Currency, normal: s.NormalBalance, meta: mCopy(s.Metadata), initOD: mNoLimit, initCR: mNoLimit,
			status: StatusOpen, dp: new(big.Int), cp: new(big.Int), dq: new(big.Int), cq: new(big.Int)}
		if s.NonNegative {
			a.initOD = 0
		}
		if s.NonPositive {
			a.initCR = 0
		}
		a.od, a.cr = a.initOD, a.initCR
		m.accts[s.ID] = a
		return mResult{account: a.at(m.head)}
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
			return mResult{code: CodeNotFound}
		case a.status == StatusClosed:
			return mResult{code: CodeAccountClosed}
		case to == StatusFrozen && a.status != StatusOpen, to == StatusOpen && a.status != StatusFrozen:
			return mResult{code: CodeInvalidState}
		case to == StatusClosed && (a.dq.Sign() != 0 || a.cq.Sign() != 0 || a.dp.Cmp(a.cp) != 0):
			return mResult{code: CodePrecondition} // something still pending, or a posted balance
		}
		a.status = to
		a.life = append(a.life, mStatus{seq: m.head, at: m.now, status: to, reason: s.Reason})
		return mResult{account: a.at(m.head)}
	case c.SetLimit != nil:
		s := c.SetLimit
		a, ok := m.accts[s.AccountID]
		switch {
		case !ok:
			return mResult{code: CodeNotFound}
		case a.status == StatusClosed:
			return mResult{code: CodeAccountClosed}
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
		return mResult{account: a.at(m.head)}
	case c.CreateTransfer != nil:
		s := c.CreateTransfer
		for _, p := range s.Preconditions {
			a, ok := m.accts[p.Account]
			if !ok {
				return mResult{code: CodeNotFound}
			}
			if p.Version != nil && *p.Version != a.version {
				return mResult{code: CodePrecondition}
			}
			ip, _, dp, _ := a.sides()
			if p.PostedBalance != nil && new(big.Int).Sub(ip, dp).Cmp(big.NewInt(*p.PostedBalance)) != 0 {
				return mResult{code: CodePrecondition}
			}
		}
		t := &mTransfer{id: id, state: StatePosted, cur: s.Currency, ref: s.Reference, meta: mCopy(s.Metadata)}
		var lines []mLine
		for _, p := range s.Postings {
			ln := mLine{acct: p.Account, dir: p.Direction}
			mp := mPosting{acct: p.Account, dir: p.Direction, amount: p.Amount}
			if s.Pending {
				ln.pending = p.Amount
			} else {
				ln.posted, mp.posted = p.Amount, p.Amount
			}
			lines = append(lines, ln)
			t.postings = append(t.postings, mp)
		}
		if code := m.apply(s.Currency, lines, false); code != "" {
			return mResult{code: code}
		}
		t.seq = m.head
		if s.Pending {
			t.state = StatePending
			t.expiresAt = m.now.Add(time.Duration(s.TimeoutSeconds) * time.Second)
		}
		m.xfers[id] = t
		m.order = append(m.order, id)
		return mResult{transfer: id, snapshot: m.snapshot(t)}
	case c.Post != nil, c.Void != nil:
		tid := ""
		if c.Post != nil {
			tid = c.Post.TransferID
		} else {
			tid = c.Void.TransferID
		}
		t, ok := m.xfers[tid]
		if !ok {
			return mResult{code: CodeNotFound}
		}
		if t.state == StateExpired {
			return mResult{code: CodeExpired}
		}
		if t.state != StatePending {
			return mResult{code: CodeInvalidState}
		}
		amounts := make([]int64, len(t.postings))
		if c.Post != nil {
			reserved := make([]int64, len(t.postings))
			for i, p := range t.postings {
				reserved[i] = p.amount
			}
			var code Code
			if amounts, code = m.choose(t, c.Post.Amounts, reserved); code != "" {
				return mResult{code: code}
			}
		}
		var lines []mLine
		for i, p := range t.postings {
			lines = append(lines, mLine{acct: p.acct, dir: p.dir, posted: amounts[i], pending: -p.amount})
		}
		if code := m.apply(t.cur, lines, c.Void != nil); code != "" {
			return mResult{code: code}
		}
		if c.Post != nil {
			t.state = StatePosted
			for i := range t.postings {
				t.postings[i].posted = amounts[i]
			}
		} else {
			t.state = StateVoided
		}
		return mResult{transfer: t.id, snapshot: m.snapshot(t)}
	case c.Reverse != nil:
		t, ok := m.xfers[c.Reverse.TransferID]
		if !ok {
			return mResult{code: CodeNotFound}
		}
		if t.reverses != "" || t.state != StatePosted {
			return mResult{code: CodeInvalidState}
		}
		left := make([]int64, len(t.postings))
		total := int64(0)
		for i, p := range t.postings {
			left[i] = p.posted - p.reversed
			total += left[i]
		}
		if total == 0 {
			return mResult{code: CodeInvalidState}
		}
		amounts, code := m.choose(t, c.Reverse.Amounts, left)
		if code != "" {
			return mResult{code: code}
		}
		r := &mTransfer{id: id, state: StatePosted, cur: t.cur, reverses: t.id, ref: c.Reverse.Reference, meta: mCopy(c.Reverse.Metadata)}
		var lines []mLine
		for i, p := range t.postings {
			if amounts[i] > 0 {
				d := Debit
				if p.dir == Debit {
					d = Credit
				}
				lines = append(lines, mLine{acct: p.acct, dir: d, posted: amounts[i]})
				r.postings = append(r.postings, mPosting{acct: p.acct, dir: d, amount: amounts[i], posted: amounts[i]})
			}
		}
		if code := m.apply(t.cur, lines, false); code != "" {
			return mResult{code: code}
		}
		r.seq = m.head
		for i := range t.postings {
			t.postings[i].reversed += amounts[i]
		}
		m.xfers[id] = r
		m.order = append(m.order, id)
		return mResult{transfer: id, snapshot: m.snapshot(r)}
	}
	panic("empty command")
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
	var d, c int64
	for i, p := range t.postings {
		if p.dir == Debit {
			d += out[i]
		} else {
			c += out[i]
		}
	}
	if d != c {
		return nil, CodeInvalid
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
	for _, p := range t.postings {
		lines = append(lines, mLine{acct: p.acct, dir: p.dir, pending: -p.amount})
	}
	if code := m.apply(t.cur, lines, true); code != "" {
		panic("model could not expire " + id)
	}
	t.state = StateExpired
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

func (g *gen) next() op {
	g.n++
	k := fmt.Sprintf("op-%d", g.n)
	if g.r.Intn(100) < 8 {
		return newOp(k, g.lifecycle())
	}
	switch x := g.r.Intn(100); {
	case x < 4:
		return newOp(k, g.openAccount())
	case x < 50:
		n := 2 + g.r.Intn(3)
		// Mostly between accounts that are open, so frozen and closed
		// accounts are refused often enough without starving the rest.
		pool := g.accts
		if g.r.Intn(8) != 0 {
			var usable []string
			for _, id := range g.accts {
				if a, ok := g.m.accts[id]; !ok || a.status == StatusOpen {
					usable = append(usable, id)
				}
			}
			if len(usable) >= n {
				pool = usable
			}
		}
		perm := g.r.Perm(len(pool))[:n]
		s := &TransferSpec{Currency: "USD"}
		if g.r.Intn(12) == 0 {
			s.Currency = "EUR"
		}
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
		return newOp(k, Command{CreateTransfer: s})
	case x < 66:
		id := g.pickTransfer(StatePending)
		return newOp(k, Command{Post: &PostSpec{TransferID: id, Amounts: g.amounts(id)}})
	case x < 74:
		return newOp(k, voidCmd(g.pickTransfer(StatePending)))
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
		ps[i] = mPosting{acct: p.Account, dir: p.Direction, amount: p.Amount, posted: p.Posted, reversed: p.Reversed}
	}
	return fmt.Sprintf("%s %s %s %v %s %q %v", tr.ID, tr.State, tr.Currency, ps, tr.Reverses, tr.Reference, tr.Metadata)
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
		string(CodeAccountFrozen), string(CodeAccountClosed)} {
		if seen[want] == 0 {
			t.Errorf("the workload never produced %q (saw %v)", want, seen)
		}
	}
}

func opName(c *Command) string {
	switch {
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
	g := &gen{r: r, m: m, accts: []string{"a", "b", "c", "d", "e", "f", "g", "h"}}
	results := map[string]mResult{} // by idempotency key
	hashes := map[string][32]byte{}

	// Accounts with every combination of normal side and limit, plus EUR.
	setup := []Command{
		openAcct("a", "USD", Debit), openAcct("b", "USD", Credit), openAcct("c", "USD", Credit, "nonneg"),
		openAcct("d", "USD", Debit, "nonneg"), openAcct("e", "USD", Debit, "nonpos"), openAcct("f", "USD", Credit, "nonpos"),
		openAcct("g", "EUR", Credit),
	}
	for i, c := range setup {
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
			res := m.exec(&c, rec.ID)
			if (rec.Type == recReject) != (res.code != "") || (rec.Err != nil && rec.Err.Code != res.code) {
				t.Fatalf("round %d op %s (%s): ledger recorded %s %+v, model says %q", round, k, o.body, rec.Type, rec.Err, res.code)
			}
			results[k], hashes[k] = res, o.hash
			if res.code == "" {
				seen["ok:"+opName(&c)]++
			} else {
				seen[string(res.code)]++
			}
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
			if codeOf(a.err) != want.code {
				t.Fatalf("round %d op %s (%s): got %v, model says %q", round, o.key, o.body, a.err, want.code)
			}
			if a.out.Replayed {
				seen["replayed"]++
			}
			if !executed[o.key] && !a.out.Replayed {
				t.Fatalf("round %d op %s: a retry of an earlier request was not marked replayed", round, o.key)
			}
			if want.code == "" && want.transfer != "" {
				if a.out.Transfer == nil || transferSnapshot(a.out.Transfer) != want.snapshot {
					t.Fatalf("round %d op %s: got transfer %+v, model returned %s", round, o.key, a.out.Transfer, want.snapshot)
				}
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
	for id, ma := range m.accts {
		a, err := l.Account(id, 0)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		b := a.Balance
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
