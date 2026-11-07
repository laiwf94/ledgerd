package ledger

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func freezeCmd(id, reason string) Command {
	return Command{FreezeAccount: &StatusSpec{AccountID: id, Reason: reason}}
}

func unfreezeCmd(id, reason string) Command {
	return Command{UnfreezeAccount: &StatusSpec{AccountID: id, Reason: reason}}
}

func closeCmd(id, reason string) Command {
	return Command{CloseAccount: &StatusSpec{AccountID: id, Reason: reason}}
}

func (e *env) wantStatus(id string, want AccountStatus, reason string) {
	e.t.Helper()
	a := e.account(id)
	if a.Status != want || a.StatusReason != reason {
		e.t.Fatalf("%s: status %q reason %q, want %q %q", id, a.Status, a.StatusReason, want, reason)
	}
}

func TestStatusCommandsAreValidated(t *testing.T) {
	for name, c := range map[string]Command{
		"bad id":         freezeCmd("", ""),
		"bad id close":   closeCmd("a b", ""),
		"bad id open":    unfreezeCmd("-x", ""),
		"long reason":    freezeCmd("a", strings.Repeat("x", MaxReason+1)),
		"control reason": closeCmd("a", "line\nbreak"),
		"two operations": {FreezeAccount: &StatusSpec{AccountID: "a"}, CloseAccount: &StatusSpec{AccountID: "a"}},
	} {
		if err := c.Validate(); err == nil || err.Code != CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	longest := freezeCmd("a", strings.Repeat("é", MaxReason/2))
	if err := longest.Validate(); err != nil {
		t.Fatalf("longest reason refused: %v", err)
	}
}

func TestNewAccountsAreOpen(t *testing.T) {
	e := newEnv(t, false)
	out := e.do(openAcct("a", "USD", Debit))
	if out.Account.Status != StatusOpen || out.Account.StatusReason != "" || !out.Account.StatusChangedAt.IsZero() {
		t.Fatalf("new account: %+v", out.Account)
	}
	e.wantStatus("a", StatusOpen, "")
	e.fail(freezeCmd("nope", ""), CodeNotFound)
	e.fail(unfreezeCmd("nope", ""), CodeNotFound)
	e.fail(closeCmd("nope", ""), CodeNotFound)
	e.fail(unfreezeCmd("a", ""), CodeInvalidState)
}

// A frozen account takes no new money movement in either direction, while
// reservations it already holds can still be released: voided by request or
// expired by timeout. Posting them waits for an unfreeze.
func TestFreezeRules(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	fund := e.do(move(dr("bank", 1000), cr("alice", 1000))).Transfer.ID
	toPost := e.do(hold(600, dr("alice", 100), cr("merchant", 100))).Transfer.ID
	toVoid := e.do(hold(600, dr("alice", 50), cr("merchant", 50))).Transfer.ID
	toExpire := e.do(hold(5, dr("alice", 20), cr("merchant", 20))).Transfer.ID
	e.do(hold(600, dr("bank", 1), cr("alice", 1)))
	versionBefore := e.account("alice").Version

	e.clock.Advance(time.Second)
	at := e.clock.Now()
	out := e.do(freezeCmd("alice", "fraud review"))
	if a := out.Account; a.Status != StatusFrozen || a.StatusReason != "fraud review" || !a.StatusChangedAt.Equal(at) || a.AsOfSeq != e.l.Head() {
		t.Fatalf("freeze outcome: %+v", a)
	}
	if out.Account.Version != versionBefore {
		t.Fatalf("freezing changed the version from %d to %d; it is not a journal entry", versionBefore, out.Account.Version)
	}
	e.wantStatus("alice", StatusFrozen, "fraud review")
	head := e.l.Head()

	e.fail(freezeCmd("alice", ""), CodeInvalidState)
	for name, c := range map[string]Command{
		"debit":         move(dr("alice", 1), cr("bob", 1)),
		"credit":        move(dr("bank", 1), cr("alice", 1)),
		"new hold":      hold(60, dr("alice", 1), cr("bob", 1)),
		"hold to it":    hold(60, dr("bank", 1), cr("alice", 1)),
		"post its hold": postCmd(toPost, 0),
		"partial post":  postCmd(toPost, 10),
		"reversal":      reverseCmd(fund, 1),
	} {
		if err := e.fail(c, CodeAccountFrozen); !strings.Contains(err.Message, "alice") {
			t.Fatalf("%s: message %q does not name the account", name, err.Message)
		}
	}
	if e.l.Head() != head {
		t.Fatal("a refused command wrote an entry")
	}
	// Other accounts are unaffected.
	e.do(move(dr("bank", 5), cr("bob", 5)))

	// Releasing reservations stays possible.
	e.do(voidCmd(toVoid))
	e.clock.Advance(10 * time.Second)
	if n, err := e.l.ExpireDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	if tr, _ := e.l.Transfer(toExpire); tr.State != StateExpired {
		t.Fatalf("hold is %s", tr.State)
	}
	e.wantBalance("alice", 1000, 1-100, 1000-100)
	e.invariants()

	// Closing needs the account emptied, frozen or not.
	e.fail(closeCmd("alice", ""), CodePrecondition)

	e.clock.Advance(time.Second)
	out = e.do(unfreezeCmd("alice", "cleared"))
	if a := out.Account; a.Status != StatusOpen || a.StatusReason != "cleared" || !a.StatusChangedAt.Equal(e.clock.Now()) {
		t.Fatalf("unfreeze outcome: %+v", a)
	}
	e.fail(unfreezeCmd("alice", ""), CodeInvalidState)
	e.do(postCmd(toPost, 0))
	e.do(reverseCmd(fund, 1))
	e.do(move(dr("alice", 1), cr("bob", 1)))
	// Freezing again works, and keeps the whole history.
	e.do(freezeCmd("alice", "second look"))
	e.l.mu.RLock()
	life := append([]statusChange(nil), e.l.st.accounts["alice"].life...)
	e.l.mu.RUnlock()
	if len(life) != 3 || life[0].status != StatusFrozen || life[1].status != StatusOpen || life[2].status != StatusFrozen ||
		life[0].seq != head || life[1].seq < life[0].seq || life[2].seq <= life[1].seq {
		t.Fatalf("history %+v", life)
	}
	e.invariants()
}

// Closing needs a zero posted balance and no open reservations on either
// side, and is final.
func TestCloseRules(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	fund := e.do(move(dr("bank", 300), cr("alice", 300))).Transfer.ID

	if err := e.fail(closeCmd("alice", ""), CodePrecondition); !strings.Contains(err.Message, "300") {
		t.Fatalf("message %q", err.Message)
	}
	e.do(move(dr("alice", 300), cr("bob", 300)))
	in := e.do(hold(600, dr("bob", 10), cr("alice", 10))).Transfer.ID
	if err := e.fail(closeCmd("alice", ""), CodePrecondition); !strings.Contains(err.Message, "reservations") {
		t.Fatalf("message %q", err.Message)
	}
	e.do(voidCmd(in))
	out := e.do(hold(600, dr("bank", 7), cr("alice", 7))).Transfer.ID
	e.fail(closeCmd("alice", ""), CodePrecondition)
	e.do(voidCmd(out))

	// Posted totals are not zero, but they cancel: that is a zero balance.
	if a := e.account("alice"); a.Balance.CreditsPosted != 300 || a.Balance.Posted != 0 {
		t.Fatalf("alice %+v", a.Balance)
	}
	res := e.do(closeCmd("alice", "customer request"))
	if res.Account.Status != StatusClosed || res.Account.StatusReason != "customer request" {
		t.Fatalf("close outcome %+v", res.Account)
	}
	head := e.l.Head()
	for _, c := range []Command{
		move(dr("bank", 1), cr("alice", 1)),
		move(dr("alice", 1), cr("bank", 1)),
		hold(60, dr("bank", 1), cr("alice", 1)),
		reverseCmd(fund, 1),
		freezeCmd("alice", ""),
		unfreezeCmd("alice", ""),
		closeCmd("alice", ""),
	} {
		e.fail(c, CodeAccountClosed)
	}
	if e.l.Head() != head {
		t.Fatal("a refused command wrote an entry")
	}
	// It can still be read, listed and its statement fetched.
	e.wantStatus("alice", StatusClosed, "customer request")
	page, err := e.l.ListAccounts(AccountFilter{}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range page.Accounts {
		if a.ID == "alice" {
			found = a.Status == StatusClosed
		}
	}
	if !found {
		t.Fatal("closed account not listed with its status")
	}
	if lines, _, err := e.l.Statement("alice", 0, 100); err != nil || len(lines) != 6 {
		t.Fatalf("statement %d lines, %v", len(lines), err)
	}

	// A frozen account can be closed once empty, and a new, never used
	// account can be closed at once.
	e.do(move(dr("bank", 40), cr("bob", 40)))
	e.do(freezeCmd("bob", ""))
	e.fail(closeCmd("bob", ""), CodePrecondition)
	e.do(openAcct("spare", "USD", Credit))
	e.do(closeCmd("spare", ""))
	e.wantStatus("spare", StatusClosed, "")
	e.invariants()
}

// A point-in-time read reports the status the account had then: a change
// made at head h covers every read as of h or later.
func TestStatusAsOf(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 1), cr("bob", 1)))
	e.do(move(dr("bank", 10), cr("alice", 10)))
	beforeFreeze := e.l.Head()
	e.do(freezeCmd("alice", "r1"))
	e.do(move(dr("bank", 1), cr("bob", 1)))
	frozenAt := e.l.Head()
	e.do(unfreezeCmd("alice", "r2"))
	e.do(move(dr("alice", 10), cr("bob", 10)))
	e.do(move(dr("bank", 1), cr("bob", 1)))
	e.do(closeCmd("alice", "r3"))
	e.do(move(dr("bank", 1), cr("bob", 1)))

	for _, c := range []struct {
		seq    uint64
		want   AccountStatus
		reason string
	}{
		{beforeFreeze - 1, StatusOpen, ""},
		{beforeFreeze, StatusFrozen, "r1"},
		{frozenAt, StatusOpen, "r2"},
		{frozenAt + 1, StatusOpen, "r2"},
		{frozenAt + 2, StatusClosed, "r3"},
		{e.l.Head(), StatusClosed, "r3"},
	} {
		a, err := e.l.Account("alice", c.seq)
		if err != nil {
			t.Fatal(err)
		}
		if a.Status != c.want || a.StatusReason != c.reason {
			t.Fatalf("as of %d: %s %q, want %s %q", c.seq, a.Status, a.StatusReason, c.want, c.reason)
		}
	}
	e.invariants()
}

// Status changes in the same batch as transfers apply in order: a transfer
// queued behind a freeze is refused, one queued ahead of it is not, and the
// invariant checker confirms no entry touched the account while it was
// frozen.
func TestFreezeRacesTransfers(t *testing.T) {
	e := newEnv(t, false)
	e.opts.MaxBatch = 16
	e.l.Close()
	e.open()
	e.standard()
	e.do(move(dr("bank", 1_000_000), cr("alice", 1_000_000)))
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[Code]int{}
	stop := make(chan struct{})
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				c := move(dr("alice", 1), cr("bob", 1))
				if i%2 == 1 {
					c = move(dr("bank", 1), cr("alice", 1))
				}
				_, err := e.l.Do(ctx, c, nil)
				mu.Lock()
				codes[codeOf(err)]++
				mu.Unlock()
			}
		}(w)
	}
	for i := 0; i < 60; i++ {
		e.do(freezeCmd("alice", fmt.Sprint(i)))
		e.do(unfreezeCmd("alice", ""))
	}
	close(stop)
	wg.Wait()
	if codes[""] == 0 || codes[CodeAccountFrozen] == 0 {
		t.Fatalf("outcomes %v: need both accepted and refused transfers", codes)
	}
	for c := range codes {
		if c != "" && c != CodeAccountFrozen {
			t.Fatalf("unexpected outcome %s (%v)", c, codes)
		}
	}
	e.invariants()
}

// Freezes, unfreezes and closes are logged commands: a restart from the log
// alone and from a snapshot plus the log rebuilds the same history, and
// keyed requests replay their result or refusal.
func TestLifecycleSurvivesRestart(t *testing.T) {
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
	for _, id := range []string{"bank", "a", "b", "c"} {
		must(openAcct(id, "USD", Credit), nil)
	}
	must(move(dr("bank", 50), cr("a", 50)), nil)
	frozen := must(freezeCmd("a", "kyc"), key("s", "f1", "f1"))
	clock.Advance(time.Minute)
	must(closeCmd("b", "unused"), key("s", "c1", "c1"))
	must(freezeCmd("c", ""), nil)
	must(unfreezeCmd("c", "ok"), nil)
	if _, err := l.Do(ctx, closeCmd("a", ""), key("s", "c2", "c2")); codeOf(err) != CodePrecondition {
		t.Fatalf("close a: %v", err)
	}
	for i := 0; i < 6; i++ {
		must(move(dr("bank", 1), cr("c", 1)), nil)
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
		out, err := x.Do(ctx, freezeCmd("a", "kyc"), key("s", "f1", "f1"))
		if err != nil || !out.Replayed || out.Account.Status != StatusFrozen || out.Account.StatusReason != "kyc" ||
			!out.Account.StatusChangedAt.Equal(frozen.Account.StatusChangedAt) {
			t.Fatalf("replayed freeze: %v %+v", err, out.Account)
		}
		if out, err := x.Do(ctx, closeCmd("a", ""), key("s", "c2", "c2")); codeOf(err) != CodePrecondition || !out.Replayed {
			t.Fatalf("replayed refusal: %v", err)
		}
		if _, err := x.Do(ctx, move(dr("bank", 1), cr("b", 1)), nil); codeOf(err) != CodeAccountClosed {
			t.Fatalf("closed after restart: %v", err)
		}
		if _, err := x.Do(ctx, move(dr("a", 1), cr("bank", 1)), nil); codeOf(err) != CodeAccountFrozen {
			t.Fatalf("frozen after restart: %v", err)
		}
		if v := allChecks(x); len(v) > 0 {
			t.Fatal(v)
		}
	}
	// A clean shutdown snapshots the history too.
	l.Close()
	again := openAt(t, dir, clock, nil)
	sameState(t, "after close", again, openAt(t, copyDir(t, dir, false), clock, nil))
}

// Snapshots written before account status existed (format 1) still load,
// with every account open.
func TestVersionOneSnapshotsLoad(t *testing.T) {
	old := snapWriteVersion
	snapWriteVersion = 1
	t.Cleanup(func() { snapWriteVersion = old })
	dir, clock := writeSmallSnapshot(t, false)
	snapWriteVersion = old
	fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
	full := openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "version 1", fromSnap, full)
	a, err := fromSnap.Account("alice", 0)
	if err != nil || a.Status != StatusOpen {
		t.Fatalf("%+v %v", a, err)
	}
	out, err := fromSnap.Do(context.Background(), openAcct("late2", "EUR", Credit), key("", "acct", "acct"))
	if err != nil || !out.Replayed || out.Account.Status != StatusOpen {
		t.Fatalf("replayed from a version 1 key: %v %+v", err, out.Account)
	}
}

// A snapshot whose checksums are fine but whose status history breaks the
// rules is refused.
func TestSnapshotRefusesBadStatusHistory(t *testing.T) {
	for name, life := range map[string][]statusChange{
		"open to open":     {{seq: 1, status: StatusOpen}},
		"closed reopened":  {{seq: 1, status: StatusClosed}, {seq: 1, status: StatusOpen}},
		"twice frozen":     {{seq: 1, status: StatusFrozen}, {seq: 2, status: StatusFrozen}},
		"seq goes back":    {{seq: 2, status: StatusFrozen}, {seq: 1, status: StatusOpen}},
		"seq past head":    {{seq: 99, status: StatusFrozen}},
		"unknown status":   {{seq: 1, status: "sleeping"}},
		"closed to frozen": {{seq: 1, status: StatusClosed}, {seq: 1, status: StatusFrozen}},
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
			l.st.accounts["a"].life = life
			l.mu.Unlock()
			l.Close() // writes the snapshot with the damaged history
			nums := snapshotFiles(t, dir)
			if len(nums) != 1 {
				t.Fatalf("snapshots %v", nums)
			}
			if _, _, err := readSnapshot(l.opts.FS, filepath.Join(dir, snapName(nums[0]))); err == nil || !strings.Contains(err.Error(), "status") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// The invariant checker finds every kind of status damage.
func TestCheckerFindsStatusDamage(t *testing.T) {
	for _, c := range []struct {
		name string
		life func(head uint64) []statusChange
		want string
	}{
		{"entry while frozen", func(uint64) []statusChange { return []statusChange{{seq: 1, status: StatusFrozen}} }, "was frozen when entry"},
		{"entry after close", func(uint64) []statusChange {
			return []statusChange{{seq: 1, status: StatusClosed}}
		}, "was closed when entry"},
		{"closed with a balance", func(h uint64) []statusChange { return []statusChange{{seq: h, status: StatusClosed}} }, "is closed but holds"},
		{"bad transition", func(h uint64) []statusChange { return []statusChange{{seq: h, status: StatusOpen}} }, "goes from open"},
		{"out of order", func(h uint64) []statusChange {
			return []statusChange{{seq: h, status: StatusFrozen}, {seq: h - 1, status: StatusOpen}}
		}, "out of order"},
		{"past head", func(h uint64) []statusChange { return []statusChange{{seq: h + 1, status: StatusFrozen}} }, "out of order"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, false)
			e.standard()
			e.do(move(dr("bank", 10), cr("alice", 10)))
			e.do(move(dr("alice", 4), cr("bob", 4)))
			e.do(move(dr("bank", 1), cr("bob", 1)))
			e.l.mu.Lock()
			e.l.st.accounts["alice"].life = c.life(e.l.st.head())
			e.l.mu.Unlock()
			v := e.l.CheckInvariants()
			if len(v) == 0 || !strings.Contains(strings.Join(v, "; "), c.want) {
				t.Fatalf("violations %v, want %q", v, c.want)
			}
		})
	}
	// Releases are fine on a frozen account.
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 10), cr("alice", 10)))
	h := e.do(hold(600, dr("alice", 4), cr("bob", 4))).Transfer.ID
	e.do(freezeCmd("alice", ""))
	e.do(voidCmd(h))
	e.invariants()
}

// Status changes made while the checker scans do not confuse it: it checks
// the status each account had at the head it froze.
func TestAuditSeesStatusAsOfItsHead(t *testing.T) {
	withAuditChunk(t, 1)
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 100), cr("alice", 100)))
	e.do(move(dr("bank", 100), cr("bob", 100)))
	n := 0
	e.l.auditStep = func(string) {
		n++
		switch n {
		case 1:
			// Empty alice and close her: the view still holds her
			// balance, and must still see her open.
			e.do(move(dr("alice", 100), cr("merchant", 100)))
			e.do(closeCmd("alice", ""))
		case 2:
			e.do(freezeCmd("bob", ""))
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

// A random walk over every life-cycle command and transfer kind agrees with
// the rules at every step, and the result survives a restart.
func TestRandomLifecycle(t *testing.T) {
	counts := map[string]int{} // outcomes across all seeds
	for seed := int64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			r := rand.New(rand.NewSource(seed))
			e := newEnv(t, true)
			e.opts.SnapshotEvery = 1 + r.Intn(20)
			e.l.Close()
			e.open()
			ids := []string{"a", "b", "c", "d"}
			e.do(openAcct("bank", "USD", Debit))
			for _, id := range ids {
				e.do(openAcct(id, "USD", Credit))
			}
			ctx := context.Background()
			status := map[string]AccountStatus{}
			var holds, posted []string
			for i := 0; i < 1000; i++ {
				id := ids[r.Intn(len(ids))]
				before := e.account(id)
				var c Command
				op := r.Intn(9)
				if op == 2 && r.Intn(4) != 0 {
					op = 3 // closing is final, so keep it rare
				}
				switch op {
				case 0:
					c = freezeCmd(id, "")
				case 1:
					c = unfreezeCmd(id, "")
				case 2:
					c = closeCmd(id, "")
				case 3, 4:
					c = move(dr("bank", 1+int64(r.Intn(5))), cr(id, 1+int64(r.Intn(5))))
					c.CreateTransfer.Postings[1].Amount = c.CreateTransfer.Postings[0].Amount
				case 5:
					c = move(dr(id, before.Balance.Posted), cr("bank", before.Balance.Posted))
				case 6:
					c = hold(int64(1+r.Intn(40)), dr("bank", 1), cr(id, 1))
				case 7:
					if len(holds) == 0 {
						continue
					}
					k := r.Intn(len(holds))
					if r.Intn(2) == 0 {
						c = voidCmd(holds[k])
					} else {
						c = postCmd(holds[k], 0)
					}
				case 8:
					if len(posted) == 0 {
						continue
					}
					c = reverseCmd(posted[r.Intn(len(posted))], 0)
				}
				out, err := e.l.Do(ctx, c, nil)
				code := codeOf(err)
				counts[fmt.Sprintf("%d:%s", op, code)]++
				st := status[id]
				if st == "" {
					st = StatusOpen
				}
				switch op {
				case 0, 1, 2:
					to := []AccountStatus{StatusFrozen, StatusOpen, StatusClosed}[op]
					var want Code
					switch {
					case st == StatusClosed:
						want = CodeAccountClosed
					case st == to:
						want = CodeInvalidState
					case to == StatusClosed && (before.Balance.Posted != 0 || before.Balance.DebitsPending != 0 || before.Balance.CreditsPending != 0):
						want = CodePrecondition
					}
					if code != want {
						t.Fatalf("step %d: %s on %s account %s (%+v): %v, want %q", i, to, st, id, before.Balance, err, want)
					}
					if err == nil {
						status[id] = to
					}
				case 3, 4, 5, 6:
					if before.Balance.Posted <= 0 && op == 5 {
						if code != CodeInvalid {
							t.Fatalf("step %d: empty move: %v", i, err)
						}
						break
					}
					want := map[AccountStatus]Code{StatusOpen: "", StatusFrozen: CodeAccountFrozen, StatusClosed: CodeAccountClosed}[st]
					if code != want {
						t.Fatalf("step %d: transfer on %s account: %v", i, st, err)
					}
				}
				if err == nil && out.Transfer != nil {
					switch out.Transfer.State {
					case StatePending:
						holds = append(holds, out.Transfer.ID)
					case StatePosted:
						if out.Transfer.Reverses == "" {
							posted = append(posted, out.Transfer.ID)
						}
					}
				}
				if r.Intn(10) == 0 {
					e.clock.Advance(5 * time.Second)
					if _, err := e.l.ExpireDue(ctx); err != nil {
						t.Fatal(err)
					}
				}
			}
			e.invariants()
			for _, id := range ids {
				want := status[id]
				if want == "" {
					want = StatusOpen
				}
				if got := e.account(id).Status; got != want {
					t.Fatalf("%s is %s, want %s", id, got, want)
				}
			}
			before := copyDir(t, e.dir, false)
			e.restart()
			sameState(t, "restart", e.l, openAt(t, before, e.clock, nil))
		})
	}
	for _, k := range []string{"0:", "1:", "2:", "2:failed_precondition", "3:account_frozen", "3:account_closed", "7:account_frozen", "8:account_frozen"} {
		if counts[k] == 0 && counts[strings.Replace(k, "3:", "4:", 1)] == 0 {
			t.Fatalf("outcome %s never happened: %v", k, counts)
		}
	}
}
