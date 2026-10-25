package ledger

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000_000, 0).UTC()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func seqIDs() func() string {
	var n atomic.Int64
	return func() string { return fmt.Sprintf("tr_%06d", n.Add(1)) }
}

type env struct {
	t     testing.TB
	l     *Ledger
	clock *fakeClock
	dir   string
	opts  Options
}

func newEnv(t testing.TB, persist bool) *env {
	t.Helper()
	e := &env{t: t, clock: newClock()}
	if persist {
		e.dir = t.TempDir()
	}
	e.opts = Options{Dir: e.dir, Clock: e.clock.Now, NewID: seqIDs(), SweepInterval: -1}
	e.open()
	return e
}

func (e *env) open() {
	e.t.Helper()
	l, err := Open(e.opts)
	if err != nil {
		e.t.Fatal(err)
	}
	e.l = l
	e.t.Cleanup(func() { l.Close() })
}

func (e *env) restart() {
	e.t.Helper()
	if err := e.l.Close(); err != nil {
		e.t.Fatal(err)
	}
	e.open()
}

func (e *env) do(c Command) Outcome {
	e.t.Helper()
	out, err := e.l.Do(context.Background(), c, nil)
	if err != nil {
		e.t.Fatalf("command %+v failed: %v", c, err)
	}
	return out
}

func (e *env) fail(c Command, code Code) *Error {
	e.t.Helper()
	_, err := e.l.Do(context.Background(), c, nil)
	var le *Error
	if !errors.As(err, &le) || le.Code != code {
		e.t.Fatalf("command %+v: err = %v, want code %s", c, err, code)
	}
	return le
}

func (e *env) account(id string) AccountState {
	e.t.Helper()
	a, err := e.l.Account(id, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) balance(id string) (posted, pending, available int64) {
	e.t.Helper()
	b := e.account(id).Balance
	return b.Posted, b.Pending, b.Available
}

func (e *env) wantBalance(id string, posted, pending, available int64) {
	e.t.Helper()
	p, q, a := e.balance(id)
	if p != posted || q != pending || a != available {
		e.t.Fatalf("%s: posted/pending/available = %d/%d/%d, want %d/%d/%d", id, p, q, a, posted, pending, available)
	}
}

func (e *env) invariants() {
	e.t.Helper()
	if v := e.l.CheckInvariants(); len(v) > 0 {
		e.t.Fatalf("invariants broken: %v", v)
	}
}

func openAcct(id, cur string, normal Direction, flags ...string) Command {
	s := &AccountSpec{ID: id, Currency: cur, NormalBalance: normal}
	for _, f := range flags {
		switch f {
		case "nonneg":
			s.NonNegative = true
		case "nonpos":
			s.NonPositive = true
		}
	}
	return Command{CreateAccount: s}
}

func dr(a string, n int64) PostingSpec { return PostingSpec{Account: a, Direction: Debit, Amount: n} }
func cr(a string, n int64) PostingSpec { return PostingSpec{Account: a, Direction: Credit, Amount: n} }

func move(legs ...PostingSpec) Command {
	return Command{CreateTransfer: &TransferSpec{Currency: "USD", Postings: legs}}
}

func hold(timeout int64, legs ...PostingSpec) Command {
	return Command{CreateTransfer: &TransferSpec{Currency: "USD", Postings: legs, Pending: true, TimeoutSeconds: timeout}}
}

func postCmd(id string, amount int64, parts ...PartialAmount) Command {
	return Command{Post: &PostSpec{TransferID: id, Amounts: Amounts{Amount: amount, Postings: parts}}}
}

func voidCmd(id string) Command { return Command{Void: &VoidSpec{TransferID: id}} }

func reverseCmd(id string, amount int64, parts ...PartialAmount) Command {
	return Command{Reverse: &ReverseSpec{TransferID: id, Amounts: Amounts{Amount: amount, Postings: parts}}}
}

// standard opens a funding source (a credit-normal equity account allowed to
// go anywhere), two customer wallets that may not go negative, and a merchant.
func (e *env) standard() {
	e.do(openAcct("bank", "USD", Debit))
	e.do(openAcct("equity", "USD", Credit))
	e.do(openAcct("alice", "USD", Credit, "nonneg"))
	e.do(openAcct("bob", "USD", Credit, "nonneg"))
	e.do(openAcct("merchant", "USD", Credit, "nonneg"))
}

func TestAccounts(t *testing.T) {
	e := newEnv(t, false)
	out := e.do(Command{CreateAccount: &AccountSpec{ID: "alice", Currency: "JPY", NormalBalance: Credit,
		NonNegative: true, Metadata: map[string]string{"tier": "gold"}}})
	a := out.Account
	if a.ID != "alice" || a.Exponent != 0 || !a.NonNegative || a.Metadata["tier"] != "gold" || a.Version != 0 || !a.CreatedAt.Equal(e.clock.Now()) {
		t.Fatalf("created %+v", a)
	}
	e.fail(openAcct("alice", "USD", Credit), CodeAlreadyExists)
	if _, err := e.l.Account("nobody", 0); err == nil {
		t.Fatal("missing account found")
	}
	long := strings.Repeat("x", 501)
	manyKeys := map[string]string{}
	for i := 0; i <= MaxMetadataKeys; i++ {
		manyKeys[fmt.Sprint(i)] = "v"
	}
	bad := map[string]*AccountSpec{
		"bad id":        {ID: "-alice", Currency: "USD", NormalBalance: Debit},
		"empty id":      {ID: "", Currency: "USD", NormalBalance: Debit},
		"long id":       {ID: strings.Repeat("a", 65), Currency: "USD", NormalBalance: Debit},
		"id charset":    {ID: "a/b", Currency: "USD", NormalBalance: Debit},
		"currency":      {ID: "a", Currency: "usd", NormalBalance: Debit},
		"no minor unit": {ID: "a", Currency: "XAU", NormalBalance: Debit},
		"normal":        {ID: "a", Currency: "USD", NormalBalance: "up"},
		"both flags":    {ID: "a", Currency: "USD", NormalBalance: Debit, NonNegative: true, NonPositive: true},
		"meta value":    {ID: "a", Currency: "USD", NormalBalance: Debit, Metadata: map[string]string{"k": long}},
		"meta key":      {ID: "a", Currency: "USD", NormalBalance: Debit, Metadata: map[string]string{"": "v"}},
		"meta control":  {ID: "a", Currency: "USD", NormalBalance: Debit, Metadata: map[string]string{"k": "a\nb"}},
		"meta keys":     {ID: "a", Currency: "USD", NormalBalance: Debit, Metadata: manyKeys},
		"meta utf8":     {ID: "a", Currency: "USD", NormalBalance: Debit, Metadata: map[string]string{"k": "\xff"}},
	}
	for name, s := range bad {
		if err := (&Command{CreateAccount: s}).Validate(); err == nil || err.Code != CodeInvalid {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := (&Command{}).Validate(); err == nil {
		t.Error("empty command accepted")
	}
	if err := (&Command{Void: &VoidSpec{TransferID: "a"}, Post: &PostSpec{TransferID: "a"}}).Validate(); err == nil {
		t.Error("two operations accepted")
	}
}

func TestSinglePhaseTransfers(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	out := e.do(move(dr("bank", 10_000), cr("equity", 10_000)))
	tr := out.Transfer
	if tr.State != StatePosted || tr.Postings[0].Posted != 10_000 || len(tr.Entries) != 1 || tr.Entries[0].Kind != KindPosted {
		t.Fatalf("transfer %+v", tr)
	}
	// Debit-normal bank goes up with a debit; credit-normal equity with a credit.
	e.wantBalance("bank", 10_000, 0, 10_000)
	e.wantBalance("equity", 10_000, 0, 10_000)
	// A three-way split: alice pays 1000, merchant gets 970, fees 30.
	e.do(openAcct("fees", "USD", Credit))
	e.do(move(dr("equity", 5000), cr("alice", 5000)))
	e.do(move(dr("alice", 1000), cr("merchant", 970), cr("fees", 30)))
	e.wantBalance("alice", 4000, 0, 4000)
	e.wantBalance("merchant", 970, 0, 970)
	e.wantBalance("fees", 30, 0, 30)
	if v := e.account("alice").Version; v != 2 {
		t.Fatalf("alice version %d", v)
	}

	invalid := map[string]Command{
		"unbalanced":   move(dr("bank", 10), cr("equity", 9)),
		"one posting":  move(dr("bank", 10)),
		"zero":         move(dr("bank", 0), cr("equity", 0)),
		"negative":     move(dr("bank", -5), cr("equity", -5)),
		"same account": move(dr("bank", 5), cr("bank", 5)),
		"direction":    move(PostingSpec{Account: "bank", Direction: "up", Amount: 5}, cr("equity", 5)),
		"bad account":  move(dr("b ank", 5), cr("equity", 5)),
		"sum overflow": move(dr("bank", math.MaxInt64), dr("alice", 1), cr("equity", 1)),
		"currency":     {CreateTransfer: &TransferSpec{Currency: "XYZ", Postings: []PostingSpec{dr("bank", 1), cr("equity", 1)}}},
		"timeout on posted": {CreateTransfer: &TransferSpec{Currency: "USD", TimeoutSeconds: 5,
			Postings: []PostingSpec{dr("bank", 1), cr("equity", 1)}}},
		"reference": {CreateTransfer: &TransferSpec{Currency: "USD", Reference: strings.Repeat("r", 129),
			Postings: []PostingSpec{dr("bank", 1), cr("equity", 1)}}},
	}
	many := make([]PostingSpec, MaxPostings+1)
	for i := range many {
		many[i] = cr(fmt.Sprintf("a%d", i), 1)
	}
	invalid["too many postings"] = move(many...)
	for name, c := range invalid {
		if err := c.Validate(); err == nil || err.Code != CodeInvalid {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	e.fail(move(dr("bank", 5), cr("ghost", 5)), CodeNotFound)
	e.do(openAcct("euro", "EUR", Credit))
	e.fail(move(dr("bank", 5), cr("euro", 5)), CodeCurrencyMismatch)
	if e.l.Head() != 3 {
		t.Fatalf("rejected transfers must not write entries; head %d", e.l.Head())
	}
	e.invariants()
}

func TestBalanceLimits(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 1000), cr("alice", 1000)))
	err := e.fail(move(dr("alice", 1001), cr("bob", 1001)), CodeLimitExceeded)
	if !strings.Contains(err.Message, "alice") {
		t.Fatalf("message %q", err.Message)
	}
	e.do(move(dr("alice", 1000), cr("bob", 1000))) // exactly to zero is fine
	e.wantBalance("alice", 0, 0, 0)

	// A non-positive account: a debit-normal "payable" that can only hold credits.
	e.do(openAcct("payable", "USD", Debit, "nonpos"))
	e.fail(move(dr("payable", 1), cr("equity", 1)), CodeLimitExceeded)
	e.do(move(dr("equity", 50), cr("payable", 50)))
	e.wantBalance("payable", -50, 0, -50)
	e.do(move(dr("payable", 50), cr("equity", 50))) // back to zero is fine
	e.fail(hold(60, dr("payable", 1), cr("equity", 1)), CodeLimitExceeded)
	e.invariants()
}

func TestOverflowIsRejected(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", math.MaxInt64), cr("equity", math.MaxInt64)))
	err := e.fail(move(dr("bank", 1), cr("equity", 1)), CodeOverflow)
	if !strings.Contains(err.Message, "bank") {
		t.Fatalf("message %q", err.Message)
	}
	// Pending plus posted on one side may not exceed an int64 either.
	e.do(openAcct("x", "USD", Debit))
	e.do(openAcct("y", "USD", Credit))
	e.do(hold(60, dr("x", math.MaxInt64-10), cr("y", math.MaxInt64-10)))
	e.fail(move(dr("x", 11), cr("y", 11)), CodeOverflow)
	e.invariants()
}

func TestTwoPhaseTransfers(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 10_000), cr("alice", 10_000)))

	// Authorize 3000: alice's available drops, posted does not.
	auth := e.do(hold(3600, dr("alice", 3000), cr("merchant", 3000))).Transfer
	if auth.State != StatePending || !auth.ExpiresAt.Equal(e.clock.Now().Add(time.Hour)) {
		t.Fatalf("auth %+v", auth)
	}
	e.wantBalance("alice", 10_000, -3000, 7000)
	e.wantBalance("merchant", 0, 3000, 0)
	// The reservation counts against the limit.
	e.fail(move(dr("alice", 7001), cr("bob", 7001)), CodeLimitExceeded)

	// Capture 2500 of it; the other 500 is released.
	cap := e.do(postCmd(auth.ID, 2500)).Transfer
	if cap.State != StatePosted || cap.Postings[0].Posted != 2500 || cap.Postings[0].Amount != 3000 || len(cap.Entries) != 2 {
		t.Fatalf("captured %+v", cap)
	}
	e.wantBalance("alice", 7500, 0, 7500)
	e.wantBalance("merchant", 2500, 0, 2500)
	e.fail(postCmd(auth.ID, 0), CodeInvalidState)
	e.fail(voidCmd(auth.ID), CodeInvalidState)

	// Void releases everything.
	v := e.do(hold(60, dr("alice", 1000), cr("merchant", 1000))).Transfer
	e.do(voidCmd(v.ID))
	e.wantBalance("alice", 7500, 0, 7500)
	e.fail(postCmd(v.ID, 0), CodeInvalidState)
	e.fail(voidCmd("tr_nope"), CodeNotFound)
	e.fail(postCmd("tr_nope", 0), CodeNotFound)

	// Multi-leg partial capture must balance and stay within each leg.
	e.do(openAcct("fees", "USD", Credit))
	m := e.do(hold(60, dr("alice", 1000), cr("merchant", 970), cr("fees", 30))).Transfer
	e.fail(postCmd(m.ID, 500), CodeInvalid) // amount needs exactly two postings
	e.fail(postCmd(m.ID, 0, PartialAmount{"alice", 500}, PartialAmount{"merchant", 480}), CodeInvalid)
	e.fail(postCmd(m.ID, 0, PartialAmount{"alice", 1001}, PartialAmount{"merchant", 1001}), CodeInvalid)
	e.fail(postCmd(m.ID, 0, PartialAmount{"bob", 5}), CodeInvalid)
	e.do(postCmd(m.ID, 0, PartialAmount{"alice", 500}, PartialAmount{"merchant", 485}, PartialAmount{"fees", 15}))
	e.wantBalance("alice", 7000, 0, 7000)
	e.wantBalance("fees", 15, 0, 15)

	// Full capture by default, and a two-posting amount above the reservation.
	f := e.do(hold(60, dr("alice", 100), cr("merchant", 100))).Transfer
	e.fail(postCmd(f.ID, 101), CodeInvalid)
	e.do(postCmd(f.ID, 0))
	e.wantBalance("alice", 6900, 0, 6900)
	e.invariants()

	bad := []Command{
		postCmd("tr_1", -1),
		postCmd("tr_1", 5, PartialAmount{"alice", 5}),
		postCmd("tr_1", 0, PartialAmount{"alice", 0}),
		postCmd("tr_1", 0, PartialAmount{"alice", 1}, PartialAmount{"alice", 1}),
		postCmd("tr_1", 0, PartialAmount{"al ice", 1}),
		postCmd("", 0),
		voidCmd("bad id"),
		hold(MaxTimeout.Milliseconds(), dr("a", 1), cr("b", 1)),
		hold(-1, dr("a", 1), cr("b", 1)),
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	c := hold(0, dr("a", 1), cr("b", 1))
	if err := c.Validate(); err != nil || c.CreateTransfer.TimeoutSeconds != int64(DefaultTimeout/time.Second) {
		t.Fatalf("default timeout not applied: %v %d", err, c.CreateTransfer.TimeoutSeconds)
	}
}

func TestPendingTransfersExpire(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	e.do(move(dr("equity", 1000), cr("alice", 1000)))
	a := e.do(hold(60, dr("alice", 400), cr("merchant", 400))).Transfer
	b := e.do(hold(120, dr("alice", 300), cr("merchant", 300))).Transfer
	e.wantBalance("alice", 1000, -700, 300)

	e.clock.Advance(59 * time.Second)
	if n, err := e.l.ExpireDue(context.Background()); err != nil || n != 0 {
		t.Fatalf("expired %d early, %v", n, err)
	}
	e.clock.Advance(time.Second) // exactly at the deadline
	if n, err := e.l.ExpireDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	e.wantBalance("alice", 1000, -300, 700)
	got, _ := e.l.Transfer(a.ID)
	if got.State != StateExpired || got.Entries[1].Kind != KindExpirePending {
		t.Fatalf("expired transfer %+v", got)
	}
	e.fail(postCmd(a.ID, 0), CodeExpired)
	e.fail(voidCmd(a.ID), CodeExpired)

	// A command arriving after the deadline sees the reservation already
	// released, even without an explicit sweep.
	e.clock.Advance(time.Minute)
	e.fail(postCmd(b.ID, 0), CodeExpired)
	e.wantBalance("alice", 1000, 0, 1000)
	e.invariants()

	// Expiry is in the log: a restart does not resurrect the reservations.
	e.restart()
	e.wantBalance("alice", 1000, 0, 1000)
	if s := e.l.Stats(); s.OpenPending != 0 {
		t.Fatalf("open pending after restart: %d", s.OpenPending)
	}
	e.invariants()
}

func TestBackgroundSweep(t *testing.T) {
	e := newEnv(t, false)
	e.l.Close()
	e.opts.SweepInterval = 5 * time.Millisecond
	e.open()
	e.standard()
	e.do(move(dr("equity", 10), cr("alice", 10)))
	e.do(hold(1, dr("alice", 10), cr("merchant", 10)))
	e.clock.Advance(2 * time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for e.l.Stats().OpenPending != 0 {
		if time.Now().After(deadline) {
			t.Fatal("background sweep did not expire the reservation")
		}
		time.Sleep(time.Millisecond)
	}
	e.wantBalance("alice", 10, 0, 10)
}

func TestReversals(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 5000), cr("alice", 5000)))
	pay := e.do(move(dr("alice", 2000), cr("merchant", 2000))).Transfer

	r1 := e.do(Command{Reverse: &ReverseSpec{TransferID: pay.ID, Amounts: Amounts{Amount: 500}, Reference: "refund-1"}}).Transfer
	if r1.Reverses != pay.ID || r1.Postings[0].Direction != Credit || r1.Postings[0].Account != "alice" || r1.Reference != "refund-1" {
		t.Fatalf("reversal %+v", r1)
	}
	e.wantBalance("alice", 3500, 0, 3500)
	e.wantBalance("merchant", 1500, 0, 1500)
	orig, _ := e.l.Transfer(pay.ID)
	if orig.Postings[0].Reversed != 500 || len(orig.Reversals) != 1 || orig.Reversals[0] != r1.ID {
		t.Fatalf("original not linked: %+v", orig)
	}
	e.fail(reverseCmd(pay.ID, 1501), CodeInvalid)
	e.fail(reverseCmd(r1.ID, 0), CodeInvalidState) // a reversal can't be reversed
	e.do(reverseCmd(pay.ID, 0))                    // the rest
	e.wantBalance("alice", 5000, 0, 5000)
	e.fail(reverseCmd(pay.ID, 0), CodeInvalidState)
	e.fail(reverseCmd("tr_nope", 0), CodeNotFound)

	// A reversal is a transfer like any other: limits apply to it.
	p2 := e.do(move(dr("alice", 1000), cr("merchant", 1000))).Transfer
	e.do(move(dr("merchant", 1000), cr("bob", 1000))) // merchant pays out
	e.fail(reverseCmd(p2.ID, 0), CodeLimitExceeded)

	// Pending transfers can't be reversed; a captured one reverses what posted.
	h := e.do(hold(60, dr("alice", 800), cr("merchant", 800))).Transfer
	e.fail(reverseCmd(h.ID, 0), CodeInvalidState)
	e.do(postCmd(h.ID, 600))
	e.fail(reverseCmd(h.ID, 601), CodeInvalid)
	rr := e.do(reverseCmd(h.ID, 0)).Transfer
	if rr.Postings[0].Amount != 600 {
		t.Fatalf("reversed %+v", rr)
	}

	// Partial reversal of a multi-leg transfer.
	e.do(openAcct("fees", "USD", Credit))
	m := e.do(move(dr("alice", 1000), cr("merchant", 970), cr("fees", 30))).Transfer
	rv := e.do(reverseCmd(m.ID, 0, PartialAmount{"alice", 30}, PartialAmount{"fees", 30})).Transfer
	if len(rv.Postings) != 2 {
		t.Fatalf("partial reversal should only touch two accounts: %+v", rv)
	}
	e.wantBalance("fees", 0, 0, 0)
	e.invariants()
}

func TestPreconditions(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 100), cr("alice", 100)))
	a := e.account("alice")
	v, bal := a.Version, a.Balance.Posted
	spend := func(ver *uint64, posted *int64) Command {
		c := move(dr("alice", 10), cr("bob", 10))
		c.CreateTransfer.Preconditions = []Precondition{{Account: "alice", Version: ver, PostedBalance: posted}}
		return c
	}
	e.do(spend(&v, &bal))
	e.fail(spend(&v, nil), CodePrecondition) // version moved on
	e.fail(spend(nil, &bal), CodePrecondition)
	v2, b2 := v+1, bal-10
	e.do(spend(&v2, &b2))
	c := move(dr("alice", 1), cr("bob", 1))
	c.CreateTransfer.Preconditions = []Precondition{{Account: "ghost", Version: &v}}
	e.fail(c, CodeNotFound)
	for _, pre := range [][]Precondition{
		{{Account: "alice"}},
		{{Account: "a b", Version: &v}},
		{{Account: "alice", Version: &v}, {Account: "alice", Version: &v}},
		make([]Precondition, MaxPreconditions+1),
	} {
		c := move(dr("alice", 1), cr("bob", 1))
		c.CreateTransfer.Preconditions = pre
		if err := c.Validate(); err == nil {
			t.Errorf("preconditions %v accepted", pre)
		}
	}
}

func key(scope, k string, body string) *Idem {
	return &Idem{Scope: scope, Key: k, Hash: sha256.Sum256([]byte(body))}
}

func TestIdempotency(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	ctx := context.Background()
	fund := move(dr("equity", 100), cr("alice", 100))
	first, err := e.l.Do(ctx, fund, key("k1", "fund-1", "a"))
	if err != nil || first.Replayed {
		t.Fatal(err)
	}
	again, err := e.l.Do(ctx, fund, key("k1", "fund-1", "a"))
	if err != nil || !again.Replayed || again.Transfer != first.Transfer {
		t.Fatalf("retry: %+v, %v", again, err)
	}
	e.wantBalance("alice", 100, 0, 100) // executed once

	// The same key for a different request is refused.
	_, err = e.l.Do(ctx, fund, key("k1", "fund-1", "b"))
	if le, _ := err.(*Error); le == nil || le.Code != CodeKeyReused {
		t.Fatalf("reuse: %v", err)
	}
	// Keys are scoped: another caller may use the same string.
	other, err := e.l.Do(ctx, fund, key("k2", "fund-1", "b"))
	if err != nil || other.Replayed {
		t.Fatalf("other scope: %+v %v", other, err)
	}

	// An error is a result too: the overdraft stays an overdraft on retry,
	// even after alice is funded and the ledger restarts.
	over := move(dr("alice", 1000), cr("bob", 1000))
	_, err = e.l.Do(ctx, over, key("k1", "pay-1", "c"))
	if le, _ := err.(*Error); le == nil || le.Code != CodeLimitExceeded {
		t.Fatalf("overdraft: %v", err)
	}
	e.do(move(dr("equity", 5000), cr("alice", 5000)))
	e.restart()
	out, err := e.l.Do(ctx, over, key("k1", "pay-1", "c"))
	if le, _ := err.(*Error); le == nil || le.Code != CodeLimitExceeded || !out.Replayed {
		t.Fatalf("overdraft retry after restart: %+v %v", out, err)
	}
	out, err = e.l.Do(ctx, fund, key("k1", "fund-1", "a"))
	if err != nil || !out.Replayed || out.Transfer.ID != first.Transfer.ID {
		t.Fatalf("success retry after restart: %+v %v", out, err)
	}

	// After the retention window the key is forgotten and runs again.
	e.clock.Advance(DefaultIdempotencyTTL)
	out, err = e.l.Do(ctx, fund, key("k1", "fund-1", "a"))
	if err != nil || out.Replayed || out.Transfer.ID == first.Transfer.ID {
		t.Fatalf("after TTL: %+v %v", out, err)
	}
	// Validation failures are not recorded: fixing the request with the
	// same key works (Stripe does the same).
	_, err = e.l.Do(ctx, move(dr("alice", 1), cr("bob", 2)), key("k1", "fix-me", "d"))
	if le, _ := err.(*Error); le == nil || le.Code != CodeInvalid {
		t.Fatal(err)
	}
	if _, err := e.l.Do(ctx, move(dr("alice", 1), cr("bob", 1)), key("k1", "fix-me", "e")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*Idem{{Key: ""}, {Key: strings.Repeat("k", 256)}, {Key: "a\x01"}, {Key: "k", Scope: strings.Repeat("s", 65)}} {
		if _, err := e.l.Do(ctx, fund, bad); err == nil {
			t.Errorf("idempotency key %q accepted", bad.Key)
		}
	}
	e.invariants()
}

func TestIdempotencyCompactionDropsExpiredKeys(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	ctx := context.Background()
	e.l.Do(ctx, move(dr("equity", 1), cr("alice", 1)), key("", "old-ok", "a"))
	e.l.Do(ctx, move(dr("alice", 9), cr("bob", 9)), key("", "old-err", "b"))
	e.clock.Advance(DefaultIdempotencyTTL + time.Second)
	e.l.Do(ctx, move(dr("equity", 1), cr("alice", 1)), key("", "new-ok", "c"))
	before := e.l.Stats().LogBytes
	e.restart()
	s := e.l.Stats()
	if s.IdempotencyKeys != 1 || s.LogBytes >= before {
		t.Fatalf("after compaction: %+v (log was %d bytes)", s, before)
	}
	e.wantBalance("alice", 2, 0, 2)
	e.restart() // compacting a compacted log is stable
	e.wantBalance("alice", 2, 0, 2)
}

// gate blocks fsync while armed, to hold a batch in flight.
type gate struct {
	ch    chan struct{}
	armed atomic.Bool
}

type gatedFile struct {
	wal.File
	g *gate
}

func (f gatedFile) Sync() error {
	if f.g.armed.Load() {
		<-f.g.ch
	}
	return f.File.Sync()
}

func TestConcurrentSameKey(t *testing.T) {
	e := newEnv(t, true)
	e.l.Close()
	gf := &gate{ch: make(chan struct{})}
	e.opts.WrapFile = func(f wal.File) wal.File { return gatedFile{f, gf} }
	e.open()
	e.standard()
	ctx := context.Background()
	gf.armed.Store(true)
	cmd := move(dr("equity", 10), cr("alice", 10))
	k := key("", "same", "x")
	type res struct {
		out Outcome
		err error
	}
	first := make(chan res, 1)
	go func() { o, err := e.l.Do(ctx, cmd, k); first <- res{o, err} }()
	// Wait until the first request holds the key.
	for {
		e.l.idemMu.Lock()
		n := len(e.l.inflight)
		e.l.idemMu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// A different request with the key is refused at once.
	if _, err := e.l.Do(ctx, cmd, key("", "same", "y")); err == nil || err.(*Error).Code != CodeKeyReused {
		t.Fatalf("different body while in flight: %v", err)
	}
	// The same request gives up with "in progress" when its deadline passes.
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	_, err := e.l.Do(short, cmd, k)
	cancel()
	if le, _ := err.(*Error); le == nil || le.Code != CodeInProgress {
		t.Fatalf("in progress: %v", err)
	}
	// Others wait and get the original result.
	waiters := make(chan res, 8)
	for i := 0; i < 8; i++ {
		go func() { o, err := e.l.Do(ctx, cmd, k); waiters <- res{o, err} }()
	}
	time.Sleep(20 * time.Millisecond)
	gf.armed.Store(false)
	close(gf.ch)
	r := <-first
	if r.err != nil {
		t.Fatal(r.err)
	}
	for i := 0; i < 8; i++ {
		w := <-waiters
		if w.err != nil || w.out.Transfer.ID != r.out.Transfer.ID || !w.out.Replayed {
			t.Fatalf("waiter got %+v %v", w.out, w.err)
		}
	}
	e.wantBalance("alice", 10, 0, 10)
}

func TestQueries(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 1000), cr("alice", 1000)))              // seq 1
	h := e.do(hold(60, dr("alice", 300), cr("bob", 300))).Transfer // seq 2
	e.do(move(dr("alice", 100), cr("bob", 100)))                   // seq 3
	e.do(postCmd(h.ID, 200))                                       // seq 4

	for _, c := range []struct {
		asOf                       uint64
		posted, pending, available int64
		version                    uint64
	}{{1, 1000, 0, 1000, 1}, {2, 1000, -300, 700, 2}, {3, 900, -300, 600, 3}, {4, 700, 0, 700, 4}} {
		a, err := e.l.Account("alice", c.asOf)
		if err != nil || a.Balance.Posted != c.posted || a.Balance.Pending != c.pending || a.Balance.Available != c.available || a.Version != c.version || a.AsOfSeq != c.asOf {
			t.Fatalf("as of %d: %+v %v", c.asOf, a, err)
		}
	}
	b, _ := e.l.Account("bob", 1) // before bob's first entry
	if b.Balance.Posted != 0 || b.Version != 0 {
		t.Fatalf("bob as of 1: %+v", b)
	}
	if _, err := e.l.Account("alice", 99); err == nil {
		t.Fatal("future as_of accepted")
	}

	lines, next, err := e.l.Statement("alice", 0, 3)
	if err != nil || len(lines) != 3 || next != 3 {
		t.Fatalf("page 1: %d lines, next %d, %v", len(lines), next, err)
	}
	if lines[0].PostedChange != 1000 || lines[1].PendingChange != -300 || lines[2].PostedChange != -100 || lines[2].Balance.Posted != 900 {
		t.Fatalf("lines %+v", lines)
	}
	lines, next, _ = e.l.Statement("alice", next, 3)
	if len(lines) != 1 || next != 0 || lines[0].Kind != KindPostPending || lines[0].PostedChange != -200 || lines[0].PendingChange != 300 || lines[0].Balance.Posted != 700 {
		t.Fatalf("page 2: %+v next %d", lines, next)
	}
	if _, _, err := e.l.Statement("alice", 0, 0); err == nil {
		t.Fatal("limit 0 accepted")
	}
	if _, _, err := e.l.Statement("ghost", 0, 10); err == nil {
		t.Fatal("missing account")
	}
	if _, err := e.l.Transfer("tr_missing"); err == nil {
		t.Fatal("missing transfer")
	}

	_, mv, err := e.l.Movements("alice", time.Time{}, e.clock.Now().Add(time.Second))
	if err != nil || len(mv) != 3 || mv[0].Amount != 1000 || mv[1].Amount != -100 || mv[2].Amount != -200 {
		t.Fatalf("movements %+v %v", mv, err)
	}
	if _, mv, _ := e.l.Movements("alice", e.clock.Now().Add(time.Second), e.clock.Now().Add(time.Hour)); len(mv) != 0 {
		t.Fatalf("out-of-range movements %+v", mv)
	}
	if _, _, err := e.l.Movements("ghost", time.Time{}, time.Now()); err == nil {
		t.Fatal("missing account")
	}
}

func TestGeneratedIDsAreValidated(t *testing.T) {
	e := newEnv(t, false)
	e.l.Close()
	e.opts.NewID = func() string { return "bad id" }
	e.open()
	e.standard()
	e.fail(move(dr("bank", 1), cr("equity", 1)), CodeInvalid)
}

func TestLargestCommandFitsInARecord(t *testing.T) {
	meta := map[string]string{}
	for i := 0; i < MaxMetadataKeys; i++ {
		meta[fmt.Sprintf("%s%02d", strings.Repeat("k", MaxMetadataKey-2), i)] = strings.Repeat(" ", MaxMetadataValue/3)
	}
	spec := &TransferSpec{Currency: "USD", Pending: true, TimeoutSeconds: 1, Reference: strings.Repeat(" ", MaxReference/3), Metadata: meta}
	for i := 0; i < MaxPostings; i++ {
		spec.Postings = append(spec.Postings, PostingSpec{Account: strings.Repeat("a", MaxIDLength-2) + fmt.Sprintf("%02d", i), Direction: Debit, Amount: math.MaxInt64})
		v := uint64(math.MaxUint64)
		b := int64(math.MinInt64)
		if i < MaxPreconditions {
			spec.Preconditions = append(spec.Preconditions, Precondition{Account: spec.Postings[i].Account, Version: &v, PostedBalance: &b})
		}
	}
	p, err := encodeRecord(record{Type: recCmd, Time: time.Now(), Seq: math.MaxUint64, ID: strings.Repeat("t", 64),
		Cmd: &Command{CreateTransfer: spec}, Idem: &idemField{Scope: strings.Repeat("s", MaxScope), Key: strings.Repeat("\"", MaxIdempotencyKey), Hash: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p) > 256<<10 {
		t.Fatalf("largest command encodes to %d bytes; the log allows 1 MiB per record and this should stay far below", len(p))
	}
}
