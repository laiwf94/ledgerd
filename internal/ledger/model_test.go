package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
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

type mAcct struct {
	id             string
	cur            string
	normal         Direction
	nonneg, nonpos bool
	dp, cp, dq, cq *big.Int
	version        uint64
}

type mPosting struct {
	acct                     string
	dir                      Direction
	amount, posted, reversed int64
}

type mTransfer struct {
	id        string
	state     TransferState
	cur       string
	postings  []mPosting
	expiresAt time.Time
	reverses  string
}

type mResult struct {
	code     Code
	transfer string // created or changed transfer
	snapshot string // the transfer as returned, for comparison with replays
}

type model struct {
	accts map[string]*mAcct
	xfers map[string]*mTransfer
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

// apply checks every line in order (exists, currency, overflow, limits) and
// applies all of them, or none.
func (m *model) apply(cur string, lines []mLine) Code {
	next := make([]*mAcct, len(lines))
	for i, ln := range lines {
		a, ok := m.accts[ln.acct]
		if !ok {
			return CodeNotFound
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
		if a.nonneg {
			after := n.available()
			if after.Sign() < 0 && after.Cmp(a.available()) < 0 {
				return CodeLimitExceeded
			}
		}
		if a.nonpos {
			after := n.ceiling()
			if after.Sign() > 0 && after.Cmp(a.ceiling()) > 0 {
				return CodeLimitExceeded
			}
		}
		n.version++
		next[i] = n
	}
	for _, n := range next {
		m.accts[n.id] = n
	}
	return ""
}

func (m *model) snapshot(t *mTransfer) string {
	return fmt.Sprintf("%s %s %s %v %s", t.id, t.state, t.cur, t.postings, t.reverses)
}

func (m *model) exec(c *Command, id string) mResult {
	switch {
	case c.CreateAccount != nil:
		s := c.CreateAccount
		if _, ok := m.accts[s.ID]; ok {
			return mResult{code: CodeAlreadyExists}
		}
		m.accts[s.ID] = &mAcct{id: s.ID, cur: s.Currency, normal: s.NormalBalance, nonneg: s.NonNegative, nonpos: s.NonPositive,
			dp: new(big.Int), cp: new(big.Int), dq: new(big.Int), cq: new(big.Int)}
		return mResult{}
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
		t := &mTransfer{id: id, state: StatePosted, cur: s.Currency}
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
		if code := m.apply(s.Currency, lines); code != "" {
			return mResult{code: code}
		}
		if s.Pending {
			t.state = StatePending
			t.expiresAt = m.now.Add(time.Duration(s.TimeoutSeconds) * time.Second)
		}
		m.xfers[id] = t
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
		if code := m.apply(t.cur, lines); code != "" {
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
		r := &mTransfer{id: id, state: StatePosted, cur: t.cur, reverses: t.id}
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
		if code := m.apply(t.cur, lines); code != "" {
			return mResult{code: code}
		}
		for i := range t.postings {
			t.postings[i].reversed += amounts[i]
		}
		m.xfers[id] = r
		return mResult{transfer: id, snapshot: m.snapshot(r)}
	}
	panic("empty command")
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
	if code := m.apply(t.cur, lines); code != "" {
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
	switch x := g.r.Intn(100); {
	case x < 4:
		cur := "USD"
		if g.r.Intn(3) == 0 {
			cur = "EUR"
		}
		return newOp(k, openAcct(g.accts[g.r.Intn(len(g.accts))], cur, Credit))
	case x < 50:
		n := 2 + g.r.Intn(3)
		perm := g.r.Perm(len(g.accts))[:n]
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
			s.Postings = append(s.Postings, dr(g.accts[i], a))
		}
		s.Postings = append(s.Postings, cr(g.accts[perm[n-1]], total))
		g.r.Shuffle(len(s.Postings), func(i, j int) { s.Postings[i], s.Postings[j] = s.Postings[j], s.Postings[i] })
		if g.r.Intn(10) < 4 {
			s.Pending, s.TimeoutSeconds = true, 1+g.r.Int63n(5)
		}
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
		return newOp(k, Command{Reverse: &ReverseSpec{TransferID: id, Amounts: g.amounts(id)}})
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
	return fmt.Sprintf("%s %s %s %v %s", tr.ID, tr.State, tr.Currency, ps, tr.Reverses)
}

func readRecords(t testing.TB, path string, from int64) ([]record, int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recs []record
	end, err := wal.Replay(bytes.NewReader(data[from:]), func(p []byte) error {
		r, err := decodeRecord(p)
		if r.Type != recSnapshot { // markers say where a snapshot was taken, not what happened
			recs = append(recs, r)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return recs, from + end
}

func TestMatchesReferenceModel(t *testing.T) {
	seeds, rounds := 12, 120
	if testing.Short() {
		seeds, rounds = 4, 60
	}
	seen := map[string]int{}
	for seed := 1; seed <= seeds; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) { runModel(t, int64(seed), rounds, seen) })
	}
	t.Logf("outcomes: %v", seen)
	// The workload must actually reach every outcome, or a passing run
	// proves little.
	for _, want := range []string{"ok:create_transfer", "ok:post", "ok:void", "ok:reverse", "expired", "crash", "replayed",
		string(CodeLimitExceeded), string(CodeOverflow), string(CodeNotFound), string(CodeInvalidState), string(CodeExpired),
		string(CodeCurrencyMismatch), string(CodePrecondition), string(CodeKeyReused), string(CodeInvalid), string(CodeAlreadyExists)} {
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
	}
	return "reverse"
}

func runModel(t *testing.T, seed int64, rounds int, seen map[string]int) {
	r := rand.New(rand.NewSource(seed))
	clock := newClock()
	ids := seqIDs()
	dir := t.TempDir()
	opts := Options{Dir: dir, Clock: clock.Now, NewID: ids, SweepInterval: -1, MaxBatch: 1 + r.Intn(16),
		SnapshotEvery: 1 + r.Intn(40)}
	l, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { l.Close() }()
	path := filepath.Join(dir, logName)
	_, offset := readRecords(t, path, 0)

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
	_, offset = readRecords(t, path, 0)

	for round := 0; round < rounds; round++ {
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
			sort.Strings(got)
			if want := m.due(); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("round %d: expired %v, model expected %v", round, got, want)
			}
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
		compare(t, round, l, m)

		// Sometimes the machine crashes. Every acknowledged batch was fsynced,
		// so the log on disk is exactly what a crash would leave, plus
		// perhaps half of a batch that was being written.
		if r.Intn(10) == 0 {
			crashDir := t.TempDir()
			// Snapshot files survive the crash too, including one the
			// background writer may be halfway through. They are copied
			// before the log, as on a real disk a snapshot's log record is
			// durable before its file exists.
			copySnapshots(t, filepath.Dir(path), crashDir)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if r.Intn(2) == 0 {
				torn := wal.Encode(nil, []byte(`{"t":"cmd","cmd":{"void":{"transfer_id":"x"}}}`))
				data = append(data, torn[:1+r.Intn(len(torn)-1)]...)
			}
			if err := os.WriteFile(filepath.Join(crashDir, logName), data, 0o600); err != nil {
				t.Fatal(err)
			}
			l.Close()
			opts.Dir = crashDir
			if l, err = Open(opts); err != nil {
				t.Fatalf("round %d: recovery failed: %v", round, err)
			}
			path = filepath.Join(crashDir, logName)
			seen["crash"]++
			_, offset = readRecords(t, path, 0)
			compare(t, round, l, m)
		}
	}
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
	if v := l.CheckInvariants(); len(v) > 0 {
		t.Fatalf("round %d: invariants broken: %v", round, v)
	}
}
