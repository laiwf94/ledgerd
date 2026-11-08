package ledger

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
)

func limitCmd(id string, kind LimitKind, amount int64, reason string) Command {
	return Command{SetLimit: &LimitSpec{AccountID: id, Kind: kind, Amount: &amount, Reason: reason}}
}

func noLimitCmd(id string, kind LimitKind, reason string) Command {
	return Command{SetLimit: &LimitSpec{AccountID: id, Kind: kind, Unlimited: true, Reason: reason}}
}

// wantLimits checks an account's limits; -1 means none.
func wantLimits(t testing.TB, what string, got Limits, overdraft, credit int64) {
	t.Helper()
	if limitPtr(overdraft) == nil != (got.Overdraft == nil) || limitPtr(credit) == nil != (got.Credit == nil) ||
		(got.Overdraft != nil && *got.Overdraft != overdraft) || (got.Credit != nil && *got.Credit != credit) {
		t.Fatalf("%s: limits %s, want overdraft %d credit %d", what, fmtLimits(got), overdraft, credit)
	}
}

func fmtLimits(l Limits) string {
	f := func(p *int64) string {
		if p == nil {
			return "none"
		}
		return fmt.Sprint(*p)
	}
	return fmt.Sprintf("overdraft %s credit %s from %d", f(l.Overdraft), f(l.Credit), l.EffectiveSeq)
}

func TestLimitCommandsAreValidated(t *testing.T) {
	neg := int64(-1)
	one := int64(1)
	for name, c := range map[string]Command{
		"bad id":          limitCmd("", LimitCredit, 1, ""),
		"bad kind":        limitCmd("a", "debit", 1, ""),
		"no kind":         limitCmd("a", "", 1, ""),
		"negative":        {SetLimit: &LimitSpec{AccountID: "a", Kind: LimitOverdraft, Amount: &neg}},
		"neither":         {SetLimit: &LimitSpec{AccountID: "a", Kind: LimitOverdraft}},
		"both":            {SetLimit: &LimitSpec{AccountID: "a", Kind: LimitOverdraft, Amount: &one, Unlimited: true}},
		"long reason":     limitCmd("a", LimitCredit, 1, strings.Repeat("x", MaxReason+1)),
		"control reason":  noLimitCmd("a", LimitCredit, "tab\there"),
		"two operations":  {SetLimit: &LimitSpec{AccountID: "a", Kind: LimitCredit, Amount: &one}, FreezeAccount: &StatusSpec{AccountID: "a"}},
		"unlimited wrong": {SetLimit: &LimitSpec{AccountID: "a b", Kind: LimitCredit, Unlimited: true}},
	} {
		if err := c.Validate(); err == nil || err.Code != CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, c := range []Command{limitCmd("a", LimitOverdraft, 0, ""), limitCmd("a", LimitCredit, math.MaxInt64, ""), noLimitCmd("a", LimitOverdraft, "")} {
		if err := c.Validate(); err != nil {
			t.Fatalf("%+v refused: %v", c.SetLimit, err)
		}
	}
}

// Accounts start with the limits their flags give, and set_limit changes
// them without writing a journal entry or changing the version.
func TestLimitsStartFromFlags(t *testing.T) {
	e := newEnv(t, false)
	for _, c := range []struct {
		id                string
		flag              string
		overdraft, credit int64
	}{{"plain", "", -1, -1}, {"wallet", "nonneg", 0, -1}, {"payable", "nonpos", -1, 0}} {
		out := e.do(openAcct(c.id, "USD", Credit, c.flag))
		wantLimits(t, c.id, out.Account.Limits, c.overdraft, c.credit)
		if out.Account.Limits.EffectiveSeq != 0 {
			t.Fatalf("%s: effective seq %d", c.id, out.Account.Limits.EffectiveSeq)
		}
		cur, changes, err := e.l.LimitHistory(c.id)
		if err != nil || len(changes) != 0 {
			t.Fatalf("%s: %+v %v", c.id, changes, err)
		}
		wantLimits(t, c.id, cur, c.overdraft, c.credit)
	}
	e.do(openAcct("bank", "USD", Debit))
	e.do(move(dr("bank", 5), cr("wallet", 5)))
	head, version := e.l.Head(), e.account("wallet").Version
	e.clock.Advance(time.Minute)
	out := e.do(limitCmd("wallet", LimitOverdraft, 300, "approved line"))
	wantLimits(t, "after set", out.Account.Limits, 300, -1)
	if out.Account.Limits.EffectiveSeq != head+1 || out.Account.Version != version || e.l.Head() != head || out.Account.AsOfSeq != head {
		t.Fatalf("set_limit: %+v, head %d", out.Account, e.l.Head())
	}
	e.do(limitCmd("wallet", LimitCredit, 1000, ""))
	cur, changes, err := e.l.LimitHistory("wallet")
	if err != nil || len(changes) != 2 {
		t.Fatalf("%+v %v", changes, err)
	}
	wantLimits(t, "history", cur, 300, 1000)
	c := changes[0]
	if c.Kind != LimitOverdraft || c.Amount == nil || *c.Amount != 300 || c.EffectiveSeq != head+1 || c.Reason != "approved line" || !c.Time.Equal(e.clock.Now()) {
		t.Fatalf("change %+v", c)
	}
	e.do(noLimitCmd("wallet", LimitOverdraft, "lifted"))
	wantLimits(t, "removed", e.account("wallet").Limits, -1, 1000)
	if _, changes, _ := e.l.LimitHistory("wallet"); len(changes) != 3 || changes[2].Amount != nil {
		t.Fatalf("removal not recorded: %+v", changes)
	}
	e.fail(limitCmd("nope", LimitCredit, 1, ""), CodeNotFound)
	if _, _, err := e.l.LimitHistory("nope"); codeOf(err) != CodeNotFound {
		t.Fatalf("history of a missing account: %v", err)
	}
	e.invariants()
}

// An overdraft limit lets the available balance go down to minus the limit,
// counting outgoing reservations; lowering it below what the account uses
// leaves the account over its limit, able only to move back.
func TestOverdraftLimit(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 100), cr("alice", 100)))
	e.fail(move(dr("alice", 101), cr("bob", 101)), CodeLimitExceeded)
	e.do(limitCmd("alice", LimitOverdraft, 500, ""))
	h := e.do(hold(600, dr("alice", 250), cr("bob", 250))).Transfer.ID
	err := e.fail(move(dr("alice", 351), cr("bob", 351)), CodeLimitExceeded)
	if !strings.Contains(err.Message, "overdraft limit of 500") || !strings.Contains(err.Message, "-501") {
		t.Fatalf("message %q", err.Message)
	}
	e.do(move(dr("alice", 350), cr("bob", 350))) // exactly to -500 available
	e.wantBalance("alice", -250, -250, -500)

	// Cut the line to 100 while 500 is used: nothing that moves further
	// out is allowed, but money in and the hold's release or posting are.
	e.do(limitCmd("alice", LimitOverdraft, 100, "cut"))
	e.fail(move(dr("alice", 1), cr("bob", 1)), CodeLimitExceeded)
	e.fail(hold(60, dr("alice", 1), cr("bob", 1)), CodeLimitExceeded)
	e.do(move(dr("bank", 50), cr("alice", 50)))
	e.do(postCmd(h, 200)) // posts 200, releases 50: available goes up
	e.wantBalance("alice", -400, 0, -400)
	e.fail(move(dr("alice", 1), cr("bob", 1)), CodeLimitExceeded)
	e.do(move(dr("bank", 300), cr("alice", 300)))
	e.wantBalance("alice", -100, 0, -100)
	e.fail(move(dr("alice", 1), cr("bob", 1)), CodeLimitExceeded) // -101 is past -100

	// Back to zero restores the non_negative rule's message.
	e.do(move(dr("bank", 100), cr("alice", 100)))
	e.do(limitCmd("alice", LimitOverdraft, 0, ""))
	if err := e.fail(move(dr("alice", 1), cr("bob", 1)), CodeLimitExceeded); !strings.Contains(err.Message, "may not go negative") {
		t.Fatalf("message %q", err.Message)
	}
	e.do(noLimitCmd("alice", LimitOverdraft, ""))
	e.do(move(dr("alice", 1_000_000), cr("bob", 1_000_000)))
	e.invariants()
}

// A credit limit caps the balance including incoming reservations; the
// largest limit is no limit in practice.
func TestCreditLimit(t *testing.T) {
	e := newEnv(t, false)
	e.do(openAcct("card", "USD", Debit)) // what the cardholder owes us
	e.do(openAcct("merchant", "USD", Credit))
	e.do(limitCmd("card", LimitCredit, 1000, ""))
	h := e.do(hold(600, dr("card", 600), cr("merchant", 600))).Transfer.ID
	err := e.fail(move(dr("card", 401), cr("merchant", 401)), CodeLimitExceeded)
	if !strings.Contains(err.Message, "credit limit of 1000") || !strings.Contains(err.Message, "1001") {
		t.Fatalf("message %q", err.Message)
	}
	e.do(move(dr("card", 400), cr("merchant", 400)))
	e.do(move(dr("merchant", 100), cr("card", 100))) // a repayment
	e.do(limitCmd("card", LimitCredit, 0, "card blocked"))
	e.fail(move(dr("card", 1), cr("merchant", 1)), CodeLimitExceeded)
	e.do(voidCmd(h))
	e.wantBalance("card", 300, 0, 300)
	e.do(limitCmd("card", LimitCredit, math.MaxInt64, ""))
	e.do(move(dr("card", 1_000_000), cr("merchant", 1_000_000)))
	// Both limits at once bound the balance on both sides.
	e.do(openAcct("both", "USD", Credit))
	e.do(limitCmd("both", LimitOverdraft, 10, ""))
	e.do(limitCmd("both", LimitCredit, 20, ""))
	e.fail(move(dr("card", 21), cr("both", 21)), CodeLimitExceeded)
	e.do(move(dr("card", 20), cr("both", 20)))
	e.fail(move(dr("both", 31), cr("card", 31)), CodeLimitExceeded)
	e.do(move(dr("both", 30), cr("card", 30)))
	e.wantBalance("both", -10, 0, -10)
	e.invariants()
}

// Limits can change on a frozen account, never on a closed one; a batch
// applies a limit change and the transfers around it in order.
func TestLimitsAndStatus(t *testing.T) {
	e := newEnv(t, true)
	e.l.Close()
	g := &gate{ch: make(chan struct{})}
	e.opts.WrapFile = func(f wal.File) wal.File { return gatedFile{f, g} }
	e.open()
	e.standard()
	e.do(freezeCmd("alice", ""))
	e.do(limitCmd("alice", LimitOverdraft, 50, "while frozen"))
	e.do(unfreezeCmd("alice", ""))
	e.do(move(dr("alice", 50), cr("bob", 50)))
	e.do(closeCmd("merchant", ""))
	e.fail(limitCmd("merchant", LimitCredit, 1, ""), CodeAccountClosed)
	e.fail(noLimitCmd("merchant", LimitOverdraft, ""), CodeAccountClosed)

	// Hold the writer in an fsync, queue five commands in order, then let
	// them run as one batch: a spend refused, the limit raised, the same
	// spend accepted, the limit lowered, and a further spend refused.
	ctx := context.Background()
	g.armed.Store(true)
	blocker := make(chan error, 1)
	go func() { _, err := e.l.Do(ctx, move(dr("bank", 1), cr("equity", 1)), nil); blocker <- err }()
	for len(e.l.queue) > 0 || e.l.Head() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond) // the writer has taken the blocker and waits in fsync
	cmds := []Command{move(dr("bob", 200), cr("alice", 200)), limitCmd("bob", LimitOverdraft, 150, ""),
		move(dr("bob", 200), cr("alice", 200)), limitCmd("bob", LimitOverdraft, 0, ""), move(dr("bob", 1), cr("alice", 1))}
	results := make([]error, len(cmds))
	var wg sync.WaitGroup
	for i, c := range cmds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = e.l.Do(ctx, c, nil)
		}()
		for len(e.l.queue) < i+1 {
			time.Sleep(time.Millisecond)
		}
	}
	g.armed.Store(false)
	close(g.ch)
	wg.Wait()
	if err := <-blocker; err != nil {
		t.Fatal(err)
	}
	want := []Code{CodeLimitExceeded, "", "", "", CodeLimitExceeded}
	for i, err := range results {
		if codeOf(err) != want[i] {
			t.Fatalf("command %d: %v, want %q", i, err, want[i])
		}
	}
	e.wantBalance("bob", -150, 0, -150)
	e.invariants()
}

// Point-in-time reads report the limits in force at that sequence.
func TestLimitsAsOf(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 10), cr("bob", 10)))
	e.do(move(dr("bank", 10), cr("alice", 10)))
	s1 := e.l.Head()
	e.do(limitCmd("alice", LimitOverdraft, 40, ""))
	e.do(move(dr("alice", 30), cr("bob", 30)))
	s2 := e.l.Head()
	e.do(limitCmd("alice", LimitCredit, 900, ""))
	e.do(limitCmd("alice", LimitOverdraft, 20, ""))
	e.do(move(dr("bank", 1), cr("alice", 1)))
	s3 := e.l.Head()
	for _, c := range []struct {
		seq               uint64
		overdraft, credit int64
		from              uint64
	}{{s1 - 1, 0, -1, 0}, {s1, 40, -1, s1 + 1}, {s2, 20, 900, s2 + 1}, {s3, 20, 900, s2 + 1}} {
		a, err := e.l.Account("alice", c.seq)
		if err != nil {
			t.Fatal(err)
		}
		wantLimits(t, fmt.Sprint("as of ", c.seq), a.Limits, c.overdraft, c.credit)
		if a.Limits.EffectiveSeq != c.from {
			t.Fatalf("as of %d: effective %d, want %d", c.seq, a.Limits.EffectiveSeq, c.from)
		}
	}
}

// Many clients spending against one overdraft limit while it is raised and
// lowered never take the account past the limit in force: every refusal is
// limit_exceeded, and the invariant checker then proves every entry against
// the limit of its time.
func TestLimitRacesTransfers(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(limitCmd("alice", LimitOverdraft, 100, ""))
	ctx := context.Background()
	var wg sync.WaitGroup
	var spent, refused atomic.Int64
	stop := make(chan struct{})
	for c := 0; c < 4; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := e.l.Do(ctx, move(dr("alice", 7), cr("bob", 7)), nil)
				switch codeOf(err) {
				case "":
					spent.Add(7)
				case CodeLimitExceeded:
					refused.Add(1)
				default:
					t.Errorf("spend: %v", err)
					return
				}
			}
		}()
	}
	r := rand.New(rand.NewSource(1))
	// Keep changing the limit until both outcomes happened plenty.
	for i := 0; i < 60 || spent.Load() < 1000 || refused.Load() < 20; i++ {
		if i == 100_000 {
			t.Fatalf("spent %d, refused %d after %d limit changes", spent.Load(), refused.Load(), i)
		}
		e.do(limitCmd("alice", LimitOverdraft, int64(r.Intn(400)), ""))
		if i%5 == 0 {
			e.do(move(dr("bank", 20), cr("alice", 20)))
		}
	}
	close(stop)
	wg.Wait()
	if spent.Load() == 0 || refused.Load() == 0 {
		t.Fatalf("spent %d, refused %d: the race did not happen", spent.Load(), refused.Load())
	}
	e.invariants()
}

// Limit changes and their keyed results survive a restart from the log
// alone and from snapshot plus log, and the limits are still enforced.
func TestLimitsSurviveRestart(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	l := openAt(t, dir, clock, func(o *Options) { o.NewID, o.SnapshotEvery = seqIDs(), 4 })
	ctx := context.Background()
	must := func(c Command, idem *Idem) Outcome {
		t.Helper()
		out, err := l.Do(ctx, c, idem)
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		return out
	}
	must(openAcct("bank", "USD", Debit), nil)
	must(openAcct("a", "USD", Credit, "nonneg"), nil)
	set := must(limitCmd("a", LimitOverdraft, 30, "line"), key("s", "l1", "l1"))
	must(move(dr("a", 30), cr("bank", 30)), nil)
	clock.Advance(time.Minute)
	must(limitCmd("a", LimitOverdraft, 10, "cut"), nil)
	must(limitCmd("bank", LimitCredit, 100, ""), nil)
	for i := 0; i < 6; i++ {
		must(move(dr("bank", 1), cr("a", 1)), nil)
	}
	waitSnapshots(l)
	if len(snapshotFiles(t, dir)) == 0 {
		t.Fatal("no snapshot was written")
	}
	fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
	full := openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "snapshot", fromSnap, full)
	sameState(t, "live", l, full)
	for _, x := range []*Ledger{fromSnap, full} {
		out, err := x.Do(ctx, limitCmd("a", LimitOverdraft, 30, "line"), key("s", "l1", "l1"))
		if err != nil || !out.Replayed || fmtLimits(out.Account.Limits) != fmtLimits(set.Account.Limits) {
			t.Fatalf("replayed limit: %v %+v", err, out.Account)
		}
		if _, err := x.Do(ctx, move(dr("a", 1), cr("bank", 1)), nil); codeOf(err) != CodeLimitExceeded {
			t.Fatalf("cut limit after restart: %v", err)
		}
		if _, err := x.Do(ctx, move(dr("bank", 125), cr("a", 125)), nil); codeOf(err) != CodeLimitExceeded {
			t.Fatalf("credit limit after restart: %v", err)
		}
		cur, changes, err := x.LimitHistory("a")
		if err != nil || len(changes) != 2 || changes[1].Reason != "cut" {
			t.Fatalf("history after restart: %+v %v", changes, err)
		}
		wantLimits(t, "after restart", cur, 10, -1)
		if v := allChecks(x); len(v) > 0 {
			t.Fatal(v)
		}
	}
	l.Close()
	again := openAt(t, dir, clock, nil)
	sameState(t, "after close", again, openAt(t, copyDir(t, dir, false), clock, nil))
}

// Snapshots written before limit changes existed (format 2) still load, with
// every account at the limits its flags give.
func TestVersionTwoSnapshotsLoad(t *testing.T) {
	old := snapWriteVersion
	snapWriteVersion = 2
	t.Cleanup(func() { snapWriteVersion = old })
	dir, clock := writeSmallSnapshot(t, 2)
	snapWriteVersion = old
	fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
	full := openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "version 2", fromSnap, full)
	a, err := fromSnap.Account("alice", 0)
	if err != nil || a.Status != StatusFrozen {
		t.Fatalf("%+v %v", a, err)
	}
	wantLimits(t, "alice", a.Limits, 0, -1)
	out, err := fromSnap.Do(context.Background(), openAcct("late2", "EUR", Credit), key("", "acct", "acct"))
	if err != nil || !out.Replayed || out.Account.Limits.Overdraft != nil || out.Account.Limits.Credit != nil {
		t.Fatalf("replayed from a version 2 key: %v %+v", err, out.Account)
	}
}

// A snapshot whose checksums are fine but whose limit history is malformed
// is refused.
func TestSnapshotRefusesBadLimitHistory(t *testing.T) {
	for name, limits := range map[string][]limitChange{
		"unknown kind":  {{seq: 1, kind: "debit", amount: 5}},
		"seq goes back": {{seq: 2, kind: LimitCredit, amount: 5}, {seq: 1, kind: LimitCredit, amount: 5}},
		"seq past head": {{seq: 99, kind: LimitOverdraft, amount: 5}},
		"bad amount":    {{seq: 1, kind: LimitOverdraft, amount: -2}},
	} {
		t.Run(name, func(t *testing.T) {
			clock := newClock()
			dir := t.TempDir()
			l := openAt(t, dir, clock, func(o *Options) { o.SnapshotEvery = 1000 })
			ctx := context.Background()
			l.Do(ctx, openAcct("a", "USD", Credit), nil)
			l.Do(ctx, openAcct("b", "USD", Debit), nil)
			l.Do(ctx, move(dr("b", 1), cr("a", 1)), nil)
			l.Do(ctx, move(dr("b", 1), cr("a", 1)), nil)
			l.mu.Lock()
			l.st.accounts["a"].limits = limits
			l.mu.Unlock()
			l.Close()
			nums := snapshotFiles(t, dir)
			if len(nums) != 1 {
				t.Fatalf("snapshots %v", nums)
			}
			if _, _, err := readSnapshot(l.opts.FS, filepath.Join(dir, snapName(nums[0]))); err == nil || !strings.Contains(err.Error(), "limit history") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// The invariant checker judges each entry by the limit in force when it was
// written: a limit lowered below what an account uses is no violation, but
// an entry that broke the limit of its time is, and so is a malformed
// history.
func TestCheckerFindsLimitDamage(t *testing.T) {
	setup := func(t *testing.T) *env {
		e := newEnv(t, false)
		e.standard()
		e.do(limitCmd("alice", LimitOverdraft, 100, ""))
		e.do(move(dr("alice", 80), cr("bob", 80)))
		e.do(limitCmd("alice", LimitOverdraft, 10, "cut"))
		e.do(move(dr("bank", 5), cr("alice", 5)))
		e.invariants() // over the new limit, legitimately
		return e
	}
	for _, c := range []struct {
		name   string
		damage func(st *state, a *account, head uint64)
		want   string
	}{
		{"entry past the limit of its time", func(st *state, a *account, _ uint64) {
			a.limits = append([]limitChange{{seq: 0, kind: LimitOverdraft, amount: 50}}, a.limits[1:]...)
		}, "entry 1 broke the limit in force on account alice"},
		{"credit limit on an entry", func(st *state, a *account, _ uint64) {
			st.accounts["bob"].limits = []limitChange{{seq: 0, kind: LimitCredit, amount: 3}}
		}, "account bob has a credit limit of 3"},
		{"unknown kind", func(st *state, a *account, _ uint64) { a.limits[0].kind = "debit" }, "limit history"},
		{"out of order", func(st *state, a *account, _ uint64) { a.limits[0].seq = a.limits[1].seq + 1 }, "limit history"},
		{"past head", func(st *state, a *account, h uint64) { a.limits[1].seq = h + 1 }, "limit history"},
		{"totals past the limit", func(st *state, a *account, _ uint64) { a.bal.DebitsPosted += 1000 }, "is past its limit"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			e.l.mu.Lock()
			c.damage(e.l.st, e.l.st.accounts["alice"], e.l.st.head())
			e.l.mu.Unlock()
			v := e.l.CheckInvariants()
			if len(v) == 0 || !strings.Contains(strings.Join(v, "; "), c.want) {
				t.Fatalf("violations %v, want %q", v, c.want)
			}
		})
	}
}

// Limit changes made while the checker scans do not confuse it: it checks
// the limits each account had at the head it froze.
func TestAuditSeesLimitsAsOfItsHead(t *testing.T) {
	withAuditChunk(t, 1)
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 100), cr("alice", 100)))
	n := 0
	e.l.auditStep = func(string) {
		n++
		if n == 1 {
			e.do(limitCmd("alice", LimitOverdraft, 500, ""))
			e.do(move(dr("alice", 400), cr("bob", 400)))
			e.do(limitCmd("alice", LimitOverdraft, 0, ""))
			e.do(limitCmd("bob", LimitCredit, 1, ""))
		}
	}
	_, v := e.l.Audit()
	e.l.auditStep = nil
	if len(v) > 0 {
		t.Fatalf("violations: %v", v)
	}
	if n < 2 {
		t.Fatalf("checker paused %d times", n)
	}
	e.invariants()
}
