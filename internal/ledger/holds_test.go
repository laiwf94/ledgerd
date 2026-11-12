package ledger

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func increaseCmd(id string, amount int64, parts ...PartialAmount) Command {
	return Command{IncreasePending: &IncreaseSpec{TransferID: id, Amounts: Amounts{Amount: amount, Postings: parts}}}
}

func extendCmd(id string, secs int64) Command {
	return Command{ExtendPending: &ExtendSpec{TransferID: id, TimeoutSeconds: secs}}
}

func kinds(tr *Transfer) []EntryKind {
	var out []EntryKind
	for _, r := range tr.Entries {
		out = append(out, r.Kind)
	}
	return out
}

func (e *env) entry(seq uint64) Entry {
	e.t.Helper()
	e.l.mu.RLock()
	defer e.l.mu.RUnlock()
	return e.l.st.entries[seq-1]
}

// A hotel hold grows twice, is posted in full, and every step is in the
// transfer's history and the journal.
func TestIncreaseHold(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 500), cr("alice", 500)))
	h := e.do(hold(3600, dr("alice", 100), cr("merchant", 100))).Transfer
	v := e.account("alice").Version

	up := e.do(increaseCmd(h.ID, 50)).Transfer
	if up.State != StatePending || up.Postings[0].Amount != 150 || up.Postings[1].Amount != 150 || !up.ExpiresAt.Equal(h.ExpiresAt) {
		t.Fatalf("after increase: %+v", up)
	}
	if got := kinds(up); !reflect.DeepEqual(got, []EntryKind{KindPending, KindIncreasePending}) {
		t.Fatalf("entries %v", got)
	}
	ent := e.entry(up.Entries[1].Seq)
	want := []Line{{Account: "alice", Direction: Debit, Pending: 50}, {Account: "merchant", Direction: Credit, Pending: 50}}
	if ent.Kind != KindIncreasePending || ent.TransferID != h.ID || !reflect.DeepEqual(ent.Lines, want) || !ent.ExpiresAt.IsZero() {
		t.Fatalf("entry %+v", ent)
	}
	e.wantBalance("alice", 500, -150, 350)
	e.wantBalance("merchant", 0, 150, 0)
	if a := e.account("alice"); a.Version != v+1 {
		t.Fatalf("version %d, want %d", a.Version, v+1)
	}
	// The earlier outcome is never modified.
	if h.Postings[0].Amount != 100 || len(h.Entries) != 1 {
		t.Fatalf("original changed: %+v", h)
	}
	e.do(increaseCmd(h.ID, 0, PartialAmount{"alice", 25}, PartialAmount{"merchant", 25}))
	e.wantBalance("alice", 500, -175, 325)
	lines, _, err := e.l.Statement("alice", 0, 100)
	if err != nil || len(lines) != 4 || lines[3].Kind != KindIncreasePending || lines[3].PendingChange != -25 {
		t.Fatalf("statement %+v %v", lines, err)
	}
	e.invariants()

	done := e.do(postCmd(h.ID, 0)).Transfer
	if done.Postings[0].Posted != 175 || done.State != StatePosted {
		t.Fatalf("posted %+v", done)
	}
	e.wantBalance("alice", 325, 0, 325)
	e.wantBalance("merchant", 175, 0, 175)
	e.invariants()
}

// An increase on a split hold names the postings it grows, balanced. A void
// releases everything the hold came to.
func TestIncreaseSplitHold(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 1000), cr("alice", 1000)))
	h := e.do(hold(60, dr("alice", 100), cr("merchant", 90), cr("bob", 10))).Transfer
	e.fail(increaseCmd(h.ID, 5), CodeInvalid) // amount needs two postings
	e.fail(increaseCmd(h.ID, 0, PartialAmount{"alice", 5}), CodeInvalid)
	e.fail(increaseCmd(h.ID, 0, PartialAmount{"alice", 5}, PartialAmount{"shop", 5}), CodeInvalid)
	up := e.do(increaseCmd(h.ID, 0, PartialAmount{"alice", 30}, PartialAmount{"merchant", 30})).Transfer
	if up.Postings[0].Amount != 130 || up.Postings[1].Amount != 120 || up.Postings[2].Amount != 10 {
		t.Fatalf("postings %+v", up.Postings)
	}
	// Postings that do not grow get no line.
	if ent := e.entry(up.Entries[1].Seq); len(ent.Lines) != 2 {
		t.Fatalf("lines %+v", ent.Lines)
	}
	e.wantBalance("bob", 0, 10, 0)
	e.do(voidCmd(h.ID))
	e.wantBalance("alice", 1000, 0, 1000)
	e.wantBalance("merchant", 0, 0, 0)
	e.wantBalance("bob", 0, 0, 0)
	e.invariants()
}

// An increase is checked against the limits like a new hold, on both sides.
func TestIncreaseIsCheckedAgainstLimits(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 100), cr("alice", 100)))
	h := e.do(hold(60, dr("alice", 80), cr("merchant", 80))).Transfer
	e.fail(increaseCmd(h.ID, 21), CodeLimitExceeded)
	e.do(increaseCmd(h.ID, 20))
	e.wantBalance("alice", 100, -100, 0)
	e.fail(increaseCmd(h.ID, 1), CodeLimitExceeded)
	e.do(limitCmd("alice", LimitOverdraft, 30, "card"))
	e.do(increaseCmd(h.ID, 30))
	e.fail(increaseCmd(h.ID, 1), CodeLimitExceeded)
	// Incoming pending counts against the credit limit.
	e.do(limitCmd("merchant", LimitCredit, 140, ""))
	e.do(limitCmd("alice", LimitOverdraft, 100, ""))
	e.fail(increaseCmd(h.ID, 11), CodeLimitExceeded)
	e.do(increaseCmd(h.ID, 10))
	e.wantBalance("merchant", 0, 140, 0)
	// Refusals change nothing.
	if tr, _ := e.l.Transfer(h.ID); tr.Postings[0].Amount != 140 || len(tr.Entries) != 4 {
		t.Fatalf("hold %+v", tr)
	}
	e.invariants()
}

func TestIncreaseRefusals(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 1000), cr("alice", 1000)))
	e.fail(increaseCmd("tr_missing", 1), CodeNotFound)
	posted := e.do(move(dr("alice", 1), cr("bob", 1))).Transfer
	e.fail(increaseCmd(posted.ID, 1), CodeInvalidState)
	voided := e.do(hold(60, dr("alice", 1), cr("bob", 1))).Transfer
	e.do(voidCmd(voided.ID))
	e.fail(increaseCmd(voided.ID, 1), CodeInvalidState)
	short := e.do(hold(5, dr("alice", 1), cr("bob", 1))).Transfer
	e.clock.Advance(5 * time.Second)
	e.fail(increaseCmd(short.ID, 1), CodeExpired)
	bal := e.do(pendingBalancing(true, false, dr("alice", 10), cr("bob", 10))).Transfer
	if le := e.fail(increaseCmd(bal.ID, 1), CodeInvalidState); !strings.Contains(le.Message, "balancing hold") {
		t.Fatalf("message %q", le.Message)
	}
	h := e.do(hold(60, dr("alice", 1), cr("merchant", 1))).Transfer
	e.do(freezeCmd("merchant", ""))
	e.fail(increaseCmd(h.ID, 1), CodeAccountFrozen)
	e.do(unfreezeCmd("merchant", ""))
	e.do(increaseCmd(h.ID, 1))
	// Validation, before the ledger sees the command.
	for _, c := range []Command{
		increaseCmd(h.ID, 0),
		increaseCmd(h.ID, -1),
		increaseCmd("", 1),
		increaseCmd(h.ID, 1, PartialAmount{"alice", 1}),
		increaseCmd(h.ID, 0, PartialAmount{"alice", 0}),
		increaseCmd(h.ID, 0, PartialAmount{"alice", 1}, PartialAmount{"alice", 1}),
	} {
		if err := c.Validate(); err == nil || err.Code != CodeInvalid {
			t.Fatalf("%+v: %v", c.IncreasePending, err)
		}
	}
	e.invariants()
}

// Totals never overflow: not the account's, and not the sum of the
// increase's own amounts.
func TestIncreaseOverflow(t *testing.T) {
	e := newEnv(t, false)
	e.do(openAcct("a", "USD", Debit))
	e.do(openAcct("b", "USD", Debit))
	e.do(openAcct("c", "USD", Credit))
	h := e.do(hold(60, dr("a", math.MaxInt64-10), cr("c", math.MaxInt64-10))).Transfer
	e.fail(increaseCmd(h.ID, 11), CodeOverflow)
	e.do(increaseCmd(h.ID, 10))
	e.do(voidCmd(h.ID))
	s := e.do(hold(60, dr("a", 1), dr("b", 1), cr("c", 2))).Transfer
	if le := e.fail(increaseCmd(s.ID, 0, PartialAmount{"a", math.MaxInt64}, PartialAmount{"b", math.MaxInt64}, PartialAmount{"c", 2}), CodeOverflow); !strings.Contains(le.Message, "add up") {
		t.Fatalf("message %q", le.Message)
	}
	e.invariants()
}

// An extension moves the timeout later without touching a balance or a
// version; the old timeout no longer fires.
func TestExtendHold(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 100), cr("alice", 100)))
	h := e.do(hold(60, dr("alice", 40), cr("merchant", 40))).Transfer
	before := e.account("alice")
	e.clock.Advance(30 * time.Second)
	ext := e.do(extendCmd(h.ID, 120)).Transfer
	want := e.clock.Now().Add(120 * time.Second)
	if !ext.ExpiresAt.Equal(want) || ext.State != StatePending {
		t.Fatalf("expires %v, want %v", ext.ExpiresAt, want)
	}
	if got := kinds(ext); !reflect.DeepEqual(got, []EntryKind{KindPending, KindExtendPending}) {
		t.Fatalf("entries %v", got)
	}
	ent := e.entry(ext.Entries[1].Seq)
	if ent.Kind != KindExtendPending || len(ent.Lines) != 0 || !ent.ExpiresAt.Equal(want) || ent.TransferID != h.ID || ent.Currency != "USD" {
		t.Fatalf("entry %+v", ent)
	}
	if after := e.account("alice"); after.Version != before.Version || after.Balance != before.Balance {
		t.Fatalf("account changed: %+v", after)
	}
	if lines, _, _ := e.l.Statement("alice", 0, 100); len(lines) != 2 {
		t.Fatalf("statement has %d lines", len(lines))
	}
	e.invariants()

	// Past the old timeout, nothing expires.
	e.clock.Advance(31 * time.Second)
	if n, err := e.l.ExpireDue(context.Background()); err != nil || n != 0 {
		t.Fatalf("expired %d %v", n, err)
	}
	// It can be extended again, then increased, and still expires on the
	// newest timeout with everything released.
	ext2 := e.do(extendCmd(h.ID, 600)).Transfer
	e.do(increaseCmd(h.ID, 10))
	e.clock.Advance(599 * time.Second)
	if n, _ := e.l.ExpireDue(context.Background()); n != 0 {
		t.Fatalf("expired early")
	}
	e.clock.Advance(time.Second)
	if n, err := e.l.ExpireDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("expired %d %v", n, err)
	}
	got, _ := e.l.Transfer(h.ID)
	if got.State != StateExpired || !got.ExpiresAt.Equal(ext2.ExpiresAt) ||
		!reflect.DeepEqual(kinds(got), []EntryKind{KindPending, KindExtendPending, KindExtendPending, KindIncreasePending, KindExpirePending}) {
		t.Fatalf("expired hold %+v", got)
	}
	e.wantBalance("alice", 100, 0, 100)
	e.invariants()
}

func TestExtendRefusals(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 100), cr("alice", 100)))
	h := e.do(hold(600, dr("alice", 1), cr("merchant", 1))).Transfer
	e.fail(extendCmd(h.ID, 600), CodeInvalidState) // same timeout
	e.fail(extendCmd(h.ID, 60), CodeInvalidState)  // earlier
	e.clock.Advance(time.Second)
	e.do(extendCmd(h.ID, 600))
	e.fail(extendCmd("tr_missing", 60), CodeNotFound)
	e.do(freezeCmd("alice", ""))
	e.fail(extendCmd(h.ID, 900), CodeAccountFrozen)
	e.do(unfreezeCmd("alice", ""))
	e.do(postCmd(h.ID, 0))
	e.fail(extendCmd(h.ID, 900), CodeInvalidState)
	short := e.do(hold(5, dr("alice", 1), cr("merchant", 1))).Transfer
	e.clock.Advance(5 * time.Second)
	e.fail(extendCmd(short.ID, 60), CodeExpired)
	for _, c := range []Command{extendCmd(h.ID, 0), extendCmd(h.ID, -1), extendCmd(h.ID, maxTimeoutSecs+1), extendCmd("", 60)} {
		if err := c.Validate(); err == nil || err.Code != CodeInvalid {
			t.Fatalf("%+v: %v", c.ExtendPending, err)
		}
	}
	if c := extendCmd(h.ID, maxTimeoutSecs); c.Validate() != nil {
		t.Fatal("the longest timeout is refused")
	}
	e.invariants()
}

// Increases and extensions are logged commands: a restart from the log alone
// and from a snapshot give the same state, keyed retries return the
// original result, the timeout schedule survives, and verify reports sound.
func TestHoldChangesSurviveRestart(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	ctx := context.Background()
	e.do(move(dr("equity", 1000), cr("alice", 1000)))
	h := e.do(hold(60, dr("alice", 100), cr("merchant", 100))).Transfer
	inc, err := e.l.Do(ctx, increaseCmd(h.ID, 50), key("s", "inc", "inc"))
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(10 * time.Second)
	ext, err := e.l.Do(ctx, extendCmd(h.ID, 300), key("s", "ext", "ext"))
	if err != nil {
		t.Fatal(err)
	}
	// A keyed refusal is kept too.
	if _, err := e.l.Do(ctx, increaseCmd(h.ID, 10_000), key("s", "big", "big")); codeOf(err) != CodeLimitExceeded {
		t.Fatalf("big increase: %v", err)
	}
	e.restart()
	for _, c := range []struct {
		cmd  Command
		k    string
		want *Transfer
	}{{increaseCmd(h.ID, 50), "inc", inc.Transfer}, {extendCmd(h.ID, 300), "ext", ext.Transfer}} {
		again, err := e.l.Do(ctx, c.cmd, key("s", c.k, c.k))
		if err != nil || !again.Replayed || !reflect.DeepEqual(again.Transfer, c.want) {
			t.Fatalf("%s replay: %+v %v", c.k, again.Transfer, err)
		}
	}
	if out, err := e.l.Do(ctx, increaseCmd(h.ID, 10_000), key("s", "big", "big")); codeOf(err) != CodeLimitExceeded || !out.Replayed {
		t.Fatalf("refusal replay: %v", err)
	}
	got, err := e.l.Transfer(h.ID)
	if err != nil || !reflect.DeepEqual(got, ext.Transfer) {
		t.Fatalf("hold after restart: %+v %v", got, err)
	}
	e.invariants()

	dir := e.dir
	e.l.Close()
	rep, err := Verify(dir)
	if err != nil || rep.Status != StatusSound {
		t.Fatalf("verify: %+v %v", rep, err)
	}
	fromLog := openAt(t, copyDir(t, dir, false), e.clock, nil)
	fromSnap := openAt(t, copyDir(t, dir, true), e.clock, nil)
	sameState(t, "hold changes", fromSnap, fromLog)
	// The old timeout does not fire after a restart from either; the new one
	// does.
	for _, l := range []*Ledger{fromSnap, fromLog} {
		e.clock.Advance(0)
		if n, err := l.ExpireDue(ctx); err != nil || n != 0 {
			t.Fatalf("expired %d %v", n, err)
		}
	}
	e.clock.Advance(300 * time.Second)
	for _, l := range []*Ledger{fromSnap, fromLog} {
		if n, err := l.ExpireDue(ctx); err != nil || n != 1 {
			t.Fatalf("expired %d %v", n, err)
		}
		if v := allChecks(l); len(v) > 0 {
			t.Fatal(v)
		}
	}
	sameState(t, "after expiry", fromSnap, fromLog)
}

// An increase that fails inside a group commit leaves the commands around it
// as if it had never been there.
func TestHoldChangesInsideOneTxn(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 100), cr("alice", 100)))
	h := e.do(hold(60, dr("alice", 50), cr("merchant", 50))).Transfer
	tx := newTxn(e.l.st, e.clock.Now())
	if _, err := tx.exec(&Command{IncreasePending: &IncreaseSpec{TransferID: h.ID, Amounts: Amounts{Amount: 10}}}, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.exec(&Command{IncreasePending: &IncreaseSpec{TransferID: h.ID, Amounts: Amounts{Amount: 41}}}, "", nil); err == nil || err.Code != CodeLimitExceeded {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := tx.exec(&Command{ExtendPending: &ExtendSpec{TransferID: h.ID, TimeoutSeconds: 30}}, "", nil); err == nil || err.Code != CodeInvalidState {
		t.Fatalf("shorter: %v", err)
	}
	if _, err := tx.exec(&Command{ExtendPending: &ExtendSpec{TransferID: h.ID, TimeoutSeconds: 90}}, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.exec(&Command{IncreasePending: &IncreaseSpec{TransferID: h.ID, Amounts: Amounts{Amount: 40}}}, "", nil); err != nil {
		t.Fatal(err)
	}
	e.l.mu.Lock()
	tx.commit()
	e.l.mu.Unlock()
	got, _ := e.l.Transfer(h.ID)
	if got.Postings[0].Amount != 100 || len(got.Entries) != 4 || !got.ExpiresAt.Equal(e.clock.Now().Add(90*time.Second)) {
		t.Fatalf("hold %+v", got)
	}
	e.wantBalance("alice", 100, -100, 0)
	e.invariants()
}

// The checker refuses holds and journal entries that the hold commands
// could not have written.
func TestCheckerFindsHoldDamage(t *testing.T) {
	type damage struct {
		name  string
		apply func(st *state, h string)
		want  string
	}
	setTransfer := func(st *state, id string, f func(*Transfer)) {
		c := st.transfers[id].clone()
		f(c)
		st.transfers[id] = c
	}
	entryOf := func(st *state, id string, k EntryKind) *Entry {
		for _, r := range st.transfers[id].Entries {
			if r.Kind == k {
				return &st.entries[r.Seq-1]
			}
		}
		panic("no entry")
	}
	for _, c := range []damage{
		{"amount above what was reserved", func(st *state, h string) {
			setTransfer(st, h, func(tr *Transfer) { tr.Postings[0].Amount++; tr.Postings[1].Amount++ })
			st.accounts["alice"].bal.DebitsPending++
			st.accounts["merchant"].bal.CreditsPending++
		}, "holds 151 but its entries reserved 150"},
		{"timeout not the extension's", func(st *state, h string) {
			setTransfer(st, h, func(tr *Transfer) { tr.ExpiresAt = tr.ExpiresAt.Add(time.Second) })
		}, "but its last extension set"},
		{"extension with a line", func(st *state, h string) {
			e := entryOf(st, h, KindExtendPending)
			e.Lines = []Line{{Account: "alice", Direction: Debit}, {Account: "merchant", Direction: Credit}}
		}, "only an extend_pending entry has a timeout"},
		{"extension without a timeout", func(st *state, h string) {
			entryOf(st, h, KindExtendPending).ExpiresAt = time.Time{}
		}, "only an extend_pending entry has a timeout"},
		{"timeout on another kind", func(st *state, h string) {
			entryOf(st, h, KindIncreasePending).ExpiresAt = time.Unix(1, 0)
		}, "only an extend_pending entry has a timeout"},
		{"increase that posts", func(st *state, h string) {
			e := entryOf(st, h, KindIncreasePending)
			e.Lines = append([]Line(nil), e.Lines...)
			e.Lines[0].Posted = 1
			e.Lines[1].Posted = 1
		}, "increases a hold but moves 1 posted"},
		{"wrong kind listed", func(st *state, h string) {
			setTransfer(st, h, func(tr *Transfer) { tr.Entries[1].Kind = KindExtendPending })
		}, "but it is increase_pending"},
		{"entry beyond the journal", func(st *state, h string) {
			setTransfer(st, h, func(tr *Transfer) { tr.Entries = append(tr.Entries, EntryRef{Seq: 1 << 40, Kind: KindExtendPending}) })
		}, "which is not in the journal"},
		{"extension that moves the timeout back", func(st *state, h string) {
			var last *Entry
			for _, r := range st.transfers[h].Entries {
				if r.Kind == KindExtendPending {
					if last != nil {
						st.entries[r.Seq-1].ExpiresAt = last.ExpiresAt
					}
					last = &st.entries[r.Seq-1]
				}
			}
		}, "does not move its timeout later"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, false)
			e.standard()
			e.do(move(dr("equity", 500), cr("alice", 500)))
			h := e.do(hold(60, dr("alice", 100), cr("merchant", 100))).Transfer.ID
			e.do(increaseCmd(h, 50))
			e.do(extendCmd(h, 120))
			e.clock.Advance(time.Second)
			e.do(extendCmd(h, 120))
			e.invariants()
			e.l.mu.Lock()
			c.apply(e.l.st, h)
			e.l.mu.Unlock()
			if v := strings.Join(e.l.CheckInvariants(), "; "); !strings.Contains(v, c.want) {
				t.Fatalf("violations %q, want %q", v, c.want)
			}
		})
	}
	// Changes to a transfer that is not an open hold.
	for _, c := range []struct {
		name string
		k    EntryKind
	}{{"increase on a posted transfer", KindIncreasePending}, {"extension after a void", KindExtendPending}} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, false)
			e.standard()
			e.do(move(dr("equity", 500), cr("alice", 500)))
			var id string
			if c.k == KindIncreasePending {
				id = e.do(move(dr("alice", 1), cr("merchant", 1))).Transfer.ID
			} else {
				id = e.do(hold(60, dr("alice", 1), cr("merchant", 1))).Transfer.ID
				e.do(voidCmd(id))
			}
			e.l.mu.Lock()
			st := e.l.st
			seq := st.head() + 1
			ent := Entry{Seq: seq, Kind: c.k, TransferID: id, Currency: "USD"}
			if c.k == KindExtendPending {
				ent.ExpiresAt = time.Unix(2_000_000_000, 0)
			}
			st.entries = append(st.entries, ent)
			tr := st.transfers[id].clone()
			tr.Entries = append(tr.Entries, EntryRef{Seq: seq, Kind: c.k})
			st.transfers[id] = tr
			e.l.mu.Unlock()
			if v := strings.Join(e.l.CheckInvariants(), "; "); !strings.Contains(v, "while it is not an open hold") {
				t.Fatalf("violations %q", v)
			}
		})
	}
}

// Version 5 snapshots, written before hold changes, still load, and may not
// hold the new entry kinds.
func TestVersionFiveSnapshotsLoad(t *testing.T) {
	old := snapWriteVersion
	snapWriteVersion = 5
	t.Cleanup(func() { snapWriteVersion = old })
	dir, clock := writeSmallSnapshot(t, 5)
	snapWriteVersion = old
	fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
	full := openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "version 5", fromSnap, full)

	for _, k := range []EntryKind{KindIncreasePending, KindExtendPending} {
		w := &snapWriter{ver: 5}
		w.str(string(k))
		r := &snapReader{ver: 5, p: w.buf, ids: map[string]string{}, syms: map[string]string{}}
		if r.kind(); r.err == nil {
			t.Fatalf("version 5 accepted %s", k)
		}
		r = &snapReader{ver: 6, p: w.buf, ids: map[string]string{}, syms: map[string]string{}}
		if got := r.kind(); r.err != nil || got != k {
			t.Fatalf("version 6: %v %v", got, r.err)
		}
	}
}

// extendedHold finds the transfer that has been extended, for fixtures that
// do not know its generated ID.
func extendedHold(t testing.TB, l *Ledger) string {
	t.Helper()
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, e := range l.st.entries {
		if e.Kind == KindExtendPending {
			return e.TransferID
		}
	}
	t.Fatal("no extended hold")
	return ""
}
