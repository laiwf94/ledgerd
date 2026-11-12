package ledger

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
)

// balancing builds a two-posting USD transfer that balances on the debit
// side, the credit side or both.
func balancing(debit, credit bool, legs ...PostingSpec) Command {
	return Command{CreateTransfer: &TransferSpec{Currency: "USD", Postings: legs, BalanceDebit: debit, BalanceCredit: credit}}
}

func pendingBalancing(debit, credit bool, legs ...PostingSpec) Command {
	c := balancing(debit, credit, legs...)
	c.CreateTransfer.Pending, c.CreateTransfer.TimeoutSeconds = true, 600
	return c
}

// wantBalanced checks a balancing transfer's record and that both postings
// moved the amount it reports.
func wantBalanced(t *testing.T, tr *Transfer, debit, credit bool, requested, amount int64) {
	t.Helper()
	want := &Balancing{Debit: debit, Credit: credit, Requested: requested, Amount: amount}
	if !reflect.DeepEqual(tr.Balancing, want) {
		t.Fatalf("transfer %s balancing %+v, want %+v", tr.ID, tr.Balancing, want)
	}
	for _, p := range tr.Postings {
		posted := amount
		if tr.State == StatePending {
			posted = 0
		}
		if p.Amount != amount || p.Posted != posted {
			t.Fatalf("transfer %s posting %+v, want amount %d posted %d", tr.ID, p, amount, posted)
		}
	}
}

// A balance_debit transfer takes what the debit account has, never more than
// asked for; a non_negative account has an overdraft limit of zero, and a
// set overdraft limit is used in full.
func TestBalancingDebitClampsToAvailable(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 100), cr("alice", 100)))

	tr := e.do(balancing(true, false, dr("alice", 250), cr("bob", 250))).Transfer
	wantBalanced(t, tr, true, false, 250, 100)
	e.wantBalance("alice", 0, 0, 0)
	e.wantBalance("bob", 100, 0, 100)

	err := e.fail(balancing(true, false, dr("alice", 1), cr("bob", 1)), CodeLimitExceeded)
	if !strings.Contains(err.Message, "nothing to balance against") {
		t.Fatalf("message %q", err.Message)
	}

	// Less than the room: the amount asked for moves, and the record says
	// nothing was cut.
	e.do(move(dr("equity", 50), cr("alice", 50)))
	tr = e.do(balancing(true, false, dr("alice", 30), cr("bob", 30))).Transfer
	wantBalanced(t, tr, true, false, 30, 30)
	e.wantBalance("alice", 20, 0, 20)

	// Exactly the room.
	e.do(limitCmd("alice", LimitOverdraft, 40, ""))
	tr = e.do(balancing(true, false, dr("alice", 60), cr("bob", 60))).Transfer
	wantBalanced(t, tr, true, false, 60, 60)
	e.wantBalance("alice", -40, 0, -40)

	// Over the limit after it was cut: no room, refused.
	e.do(limitCmd("alice", LimitOverdraft, 10, "cut"))
	e.fail(balancing(true, false, dr("alice", 5), cr("bob", 5)), CodeLimitExceeded)
	e.invariants()
}

// With no limit set on the side a line pushes, the limit counts as zero, as
// in TigerBeetle: a balancing debit never takes an account below zero, and a
// balancing credit never takes it above zero, on either kind of normal
// balance. A set limit replaces the zero.
func TestBalancingCountsUnsetLimitsAsZero(t *testing.T) {
	t.Run("credit-normal debit", func(t *testing.T) {
		e := newEnv(t, false)
		e.standard()
		e.do(move(dr("equity", 100), cr("alice", 100))) // equity is at -100 with no limits
		e.fail(balancing(true, false, dr("equity", 1), cr("alice", 1)), CodeLimitExceeded)
		e.do(move(dr("bank", 500), cr("equity", 500)))
		wantBalanced(t, e.do(balancing(true, false, dr("equity", 1000), cr("alice", 1000))).Transfer, true, false, 1000, 400)
		e.wantBalance("equity", 0, 0, 0)
	})
	t.Run("debit-normal debit", func(t *testing.T) {
		e := newEnv(t, false)
		e.standard()
		e.do(move(dr("bank", 500), cr("equity", 500)))
		// bank is debit-normal: a debit raises it, so the credit limit
		// bounds it, and with none set it may not rise above zero.
		e.fail(balancing(true, false, dr("bank", 1), cr("equity", 1)), CodeLimitExceeded)
		e.do(limitCmd("bank", LimitCredit, 2000, ""))
		wantBalanced(t, e.do(balancing(true, false, dr("bank", 5000), cr("equity", 5000))).Transfer, true, false, 5000, 1500)
		e.wantBalance("bank", 2000, 0, 2000)
		// A plain transfer is not held to the zero: bank still has no
		// limit that refuses it once the credit limit is lifted.
		e.do(noLimitCmd("bank", LimitCredit, ""))
		e.do(move(dr("bank", 1), cr("equity", 1)))
		e.fail(balancing(true, false, dr("bank", 1), cr("equity", 1)), CodeLimitExceeded)
	})
	t.Run("debit-normal credit repays a loan", func(t *testing.T) {
		e := newEnv(t, false)
		e.standard()
		e.do(openAcct("loan", "USD", Debit))
		e.do(move(dr("loan", 700), cr("alice", 700)))     // lent 700
		e.do(move(dr("equity", 2000), cr("alice", 2000))) // alice has 2,700
		tr := e.do(balancing(false, true, dr("alice", 1000), cr("loan", 1000))).Transfer
		wantBalanced(t, tr, false, true, 1000, 700) // repays exactly what is owed
		e.wantBalance("loan", 0, 0, 0)
		e.wantBalance("alice", 2000, 0, 2000)
		e.fail(balancing(false, true, dr("alice", 1), cr("loan", 1)), CodeLimitExceeded)
	})
	t.Run("credit-normal credit", func(t *testing.T) {
		e := newEnv(t, false)
		e.standard()
		// A credit-normal account with debits on it, such as a payable
		// that was overpaid: a balancing credit brings it back to zero.
		e.do(openAcct("payable", "USD", Credit))
		e.do(move(dr("payable", 80), cr("alice", 80)))
		e.do(move(dr("equity", 500), cr("alice", 500)))
		wantBalanced(t, e.do(balancing(false, true, dr("alice", 300), cr("payable", 300))).Transfer, false, true, 300, 80)
		e.wantBalance("payable", 0, 0, 0)
		// With a credit limit, up to the limit.
		e.do(limitCmd("payable", LimitCredit, 25, ""))
		wantBalanced(t, e.do(balancing(false, true, dr("alice", 300), cr("payable", 300))).Transfer, false, true, 300, 25)
	})
	e := newEnv(t, false)
	e.standard()
	e.invariants()
}

// With both flags the smaller room wins, whichever side it is on.
func TestBalancingBothSides(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 300), cr("alice", 300)))
	e.do(limitCmd("merchant", LimitCredit, 120, ""))
	e.do(move(dr("equity", 50), cr("merchant", 50)))

	wantBalanced(t, e.do(balancing(true, true, dr("alice", 500), cr("merchant", 500))).Transfer, true, true, 500, 70)
	e.wantBalance("merchant", 120, 0, 120)
	e.wantBalance("alice", 230, 0, 230)

	e.do(limitCmd("merchant", LimitCredit, 1000, ""))
	wantBalanced(t, e.do(balancing(true, true, dr("alice", 500), cr("merchant", 500))).Transfer, true, true, 500, 230)
	e.wantBalance("alice", 0, 0, 0)

	// Balancing only the debit side does not protect the credit side: the
	// cut amount must still fit the credit account's limit.
	e.do(move(dr("equity", 900), cr("alice", 900)))
	e.fail(balancing(true, false, dr("alice", 900), cr("merchant", 900)), CodeLimitExceeded)
	e.invariants()
}

// A balancing hold reserves the cut amount, and open reservations count
// against the room of later balancing transfers on both sides.
func TestBalancingPending(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("equity", 100), cr("alice", 100)))

	h1 := e.do(pendingBalancing(true, false, dr("alice", 80), cr("bob", 80))).Transfer
	wantBalanced(t, h1, true, false, 80, 80)
	h2 := e.do(pendingBalancing(true, false, dr("alice", 80), cr("bob", 80))).Transfer
	wantBalanced(t, h2, true, false, 80, 20)
	e.wantBalance("alice", 100, -100, 0)
	e.fail(pendingBalancing(true, false, dr("alice", 1), cr("bob", 1)), CodeLimitExceeded)

	e.do(voidCmd(h1.ID))
	h3 := e.do(pendingBalancing(true, false, dr("alice", 500), cr("bob", 500))).Transfer
	wantBalanced(t, h3, true, false, 500, 80)
	posted := e.do(postCmd(h2.ID, 0)).Transfer
	if posted.Balancing == nil || posted.Balancing.Amount != 20 || posted.Postings[0].Posted != 20 {
		t.Fatalf("posted balancing hold %+v", posted)
	}
	e.wantBalance("alice", 80, -80, 0)
	e.wantBalance("bob", 20, 80, 20)

	// Incoming reservations count against a credit limit.
	e.do(limitCmd("merchant", LimitCredit, 50, ""))
	e.do(move(dr("equity", 500), cr("alice", 500)))
	wantBalanced(t, e.do(pendingBalancing(false, true, dr("alice", 90), cr("merchant", 90))).Transfer, false, true, 90, 50)
	e.fail(pendingBalancing(false, true, dr("alice", 1), cr("merchant", 1)), CodeLimitExceeded)
	e.invariants()
}

// A balancing transfer is refused for the same reasons as any other, with
// the same codes, before its room is looked at.
func TestBalancingRefusals(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(openAcct("eur", "EUR", Credit))
	e.fail(balancing(true, false, dr("nobody", 5), cr("bob", 5)), CodeNotFound)
	e.fail(balancing(false, true, dr("alice", 5), cr("nobody", 5)), CodeNotFound)
	e.fail(balancing(true, false, dr("eur", 5), cr("bob", 5)), CodeCurrencyMismatch)
	e.do(freezeCmd("alice", "review"))
	e.fail(balancing(true, false, dr("alice", 5), cr("bob", 5)), CodeAccountFrozen) // and alice has no room either
	e.do(closeCmd("bob", ""))
	e.fail(balancing(false, true, dr("equity", 5), cr("bob", 5)), CodeAccountClosed)

	for name, c := range map[string]Command{
		"three postings": {CreateTransfer: &TransferSpec{Currency: "USD", BalanceDebit: true,
			Postings: []PostingSpec{dr("a", 2), cr("b", 1), cr("c", 1)}}},
		"credit, three postings": {CreateTransfer: &TransferSpec{Currency: "USD", BalanceCredit: true,
			Postings: []PostingSpec{dr("a", 1), dr("b", 1), cr("c", 2)}}},
	} {
		if err := c.Validate(); err == nil || err.Code != CodeInvalid || !strings.Contains(err.Message, "two-posting") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if c := balancing(true, true, dr("a", 1), cr("b", 1)); c.Validate() != nil {
		t.Fatal(c.Validate())
	}
	e.invariants()
}

// The room is computed without overflowing at the ends of the int64 range.
func TestBalancingRoomSaturates(t *testing.T) {
	a := &account{Account: Account{ID: "a", Currency: "USD", NormalBalance: Credit}}
	a.limits = []limitChange{{kind: LimitOverdraft, amount: math.MaxInt64}, {kind: LimitCredit, amount: math.MaxInt64}}
	a.bal = Balance{CreditsPosted: math.MaxInt64}
	if got := a.room(Debit); got != math.MaxInt64 {
		t.Fatalf("room for a debit = %d", got)
	}
	a.bal = Balance{DebitsPosted: math.MaxInt64}
	if got := a.room(Credit); got != math.MaxInt64 {
		t.Fatalf("room for a credit = %d", got)
	}
	a.limits = nil
	if got := a.room(Credit); got != math.MaxInt64 {
		t.Fatalf("room for a credit without limits = %d", got)
	}
	a.bal = Balance{CreditsPosted: math.MaxInt64}
	if got := a.room(Credit); got != -math.MaxInt64 {
		t.Fatalf("room past the limit = %d", got)
	}
	if satAdd(math.MinInt64, -1) != math.MinInt64 || satSub(math.MinInt64, 1) != math.MinInt64 ||
		satAdd(math.MaxInt64, 1) != math.MaxInt64 || satSub(math.MaxInt64, -1) != math.MaxInt64 {
		t.Fatal("saturation")
	}

	// End to end: an amount near the largest is cut to the room exactly.
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", math.MaxInt64-1), cr("alice", math.MaxInt64-1)))
	wantBalanced(t, e.do(balancing(true, false, dr("alice", math.MaxInt64), cr("bob", math.MaxInt64))).Transfer, true, false, math.MaxInt64, math.MaxInt64-1)
	e.invariants()
}

// Inside a batch a balancing transfer sees what the transfers before it did,
// and a balancing transfer with no room refuses the whole batch.
func TestBalancingInBatch(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	before := fingerprint(t, e.l)
	_, err := e.l.Do(context.Background(), batchCmd(
		move(dr("equity", 60), cr("alice", 60)),
		balancing(true, false, dr("alice", 1000), cr("bob", 1000)),
		balancing(true, false, dr("alice", 1000), cr("merchant", 1000))), nil)
	if itemOf(err) != 2 || !strings.Contains(err.Error(), "nothing to balance against") {
		t.Fatalf("batch: %v", err)
	}
	if got := fingerprint(t, e.l); got != before {
		t.Fatal("a refused batch changed the ledger")
	}
	out := e.do(batchCmd(
		move(dr("equity", 60), cr("alice", 60)),
		balancing(true, false, dr("alice", 40), cr("bob", 40)),
		balancing(true, false, dr("alice", 1000), cr("merchant", 1000))))
	wantBalanced(t, out.Transfers[1], true, false, 40, 40)
	wantBalanced(t, out.Transfers[2], true, false, 1000, 20)
	e.wantBalance("alice", 0, 0, 0)
	e.invariants()
}

// The cut amount is not in the log: replay works it out again from the
// state, so restarts from the log alone and from a snapshot give the same
// transfers, and a keyed retry returns the original cut amount even after
// the balances changed.
func TestBalancingSurvivesRestart(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	ctx := context.Background()
	e.do(move(dr("equity", 100), cr("alice", 100)))
	c := balancing(true, false, dr("alice", 250), cr("bob", 250))
	k := key("s", "sweep", "sweep")
	first, err := e.l.Do(ctx, c, k)
	if err != nil {
		t.Fatal(err)
	}
	wantBalanced(t, first.Transfer, true, false, 250, 100)
	h := e.do(pendingBalancing(true, false, dr("bob", 500), cr("merchant", 500))).Transfer
	wantBalanced(t, h, true, false, 500, 100)
	e.do(move(dr("equity", 1000), cr("alice", 1000)))

	e.restart()
	again, err := e.l.Do(ctx, c, k)
	if err != nil || !again.Replayed || !reflect.DeepEqual(again.Transfer, first.Transfer) {
		t.Fatalf("keyed retry after restart: %+v %v", again.Transfer, err)
	}
	got, err := e.l.Transfer(h.ID)
	if err != nil || !reflect.DeepEqual(got, h) {
		t.Fatalf("hold after restart: %+v %v", got, err)
	}
	e.invariants()

	dir := e.dir
	e.l.Close()
	fromLog := openAt(t, copyDir(t, dir, false), e.clock, nil)
	fromSnap := openAt(t, copyDir(t, dir, true), e.clock, nil)
	sameState(t, "balancing", fromSnap, fromLog)
}

// The checker refuses a balancing record its postings do not bear out.
func TestCheckerFindsBalancingDamage(t *testing.T) {
	for _, c := range []struct {
		name   string
		damage func(b *Balancing, tr *Transfer)
	}{
		{"more than asked", func(b *Balancing, _ *Transfer) { b.Requested = b.Amount - 1 }},
		{"amount not the postings'", func(b *Balancing, _ *Transfer) { b.Amount-- }},
		{"no side", func(b *Balancing, _ *Transfer) { b.Debit = false }},
		{"zero", func(b *Balancing, tr *Transfer) { b.Amount = 0 }},
		{"one posting differs", func(_ *Balancing, tr *Transfer) { tr.Postings[1].Amount++ }},
		{"same direction", func(_ *Balancing, tr *Transfer) { tr.Postings[1].Direction = Debit }},
		{"on a reversal", func(_ *Balancing, tr *Transfer) { tr.Reverses = "x" }},
		{"three postings", func(_ *Balancing, tr *Transfer) { tr.Postings = append(tr.Postings, tr.Postings[0]) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, false)
			e.standard()
			e.do(move(dr("equity", 100), cr("alice", 100)))
			id := e.do(balancing(true, false, dr("alice", 250), cr("bob", 250))).Transfer.ID
			e.invariants()
			e.l.mu.Lock()
			tr := e.l.st.transfers[id].clone()
			b := *tr.Balancing
			tr.Balancing = &b
			c.damage(&b, tr)
			e.l.st.transfers[id] = tr
			e.l.mu.Unlock()
			v := strings.Join(e.l.CheckInvariants(), "; ")
			if !strings.Contains(v, "transfer "+id+" says it balanced") {
				t.Fatalf("violations %q", v)
			}
		})
	}
}

// A snapshot whose balancing flags name a side that does not exist is
// refused.
func TestSnapshotRefusesBadBalancingFlags(t *testing.T) {
	w := &snapWriter{ver: snapVersion}
	tr := &Transfer{ID: "t", State: StatePosted, Currency: "USD", Entries: []EntryRef{{Seq: 1, Kind: KindPosted}},
		Balancing: &Balancing{Debit: true, Credit: true, Requested: 9, Amount: 3}}
	w.transfer(tr)
	read := func(p []byte) (*Transfer, error) {
		r := &snapReader{ver: snapVersion, p: p, ids: map[string]string{}, syms: map[string]string{}}
		got := r.transfer()
		return got, r.err
	}
	got, err := read(w.buf)
	if err != nil || !reflect.DeepEqual(got.Balancing, tr.Balancing) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	// The flags byte follows the entry references; the two amounts are one
	// byte each, and the rate's absent flag one more.
	bad := append([]byte(nil), w.buf...)
	bad[len(bad)-4] = 4
	if _, err := read(bad); err == nil || !strings.Contains(err.Error(), "balancing flags") {
		t.Fatalf("bad flags: %v", err)
	}
}
