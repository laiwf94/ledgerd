package ledger

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func monCmd(id, acct string, f BalanceField, op MonitorOperator, v int64) Command {
	return Command{CreateMonitor: &MonitorSpec{ID: id, Account: acct, Condition: Condition{Field: f, Operator: op, Value: v}}}
}

func delMonCmd(id string) Command { return Command{DeleteMonitor: &MonitorRef{ID: id}} }

// modelMonitors watches the reference model's accounts with every field and
// operator, at thresholds its workloads cross often.
func modelMonitors() []Command {
	var out []Command
	ops := []MonitorOperator{OpLessThan, OpLessOrEqual, OpEquals, OpNotEquals, OpGreaterOrEqual, OpGreaterThan}
	fields := []BalanceField{FieldPosted, FieldPending, FieldAvailable}
	n := 0
	for _, acct := range []string{"a", "b", "c", "d", "e", "f", "g", "j", "xu"} {
		for _, v := range []int64{-50, 0, 40} {
			f, op := fields[n%len(fields)], ops[(n/len(fields)+n)%len(ops)]
			out = append(out, monCmd(fmt.Sprintf("m%02d", n), acct, f, op, v))
			n++
		}
	}
	return out
}

// writeSmallMonitors adds monitors to writeSmallSnapshot's ledger: a keyed
// create, a keyed refusal, crossings both ways, and a monitor deleted and
// created again under its ID.
func writeSmallMonitors(t *testing.T, e *env) {
	t.Helper()
	ctx := context.Background()
	e.do(openAcct("reserve", "USD", Credit))
	if _, err := e.l.Do(ctx, Command{CreateMonitor: smallLow}, key("k10", "monitor", "monitor")); err != nil {
		t.Fatal(err)
	}
	e.l.Do(ctx, monCmd("nobody", "missing", FieldPosted, OpEquals, 0), key("k10", "monitor-missing", "monitor-missing"))
	e.do(monCmd("reserve-pending", "reserve", FieldPending, OpNotEquals, 0))
	e.do(monCmd("again", "reserve", FieldPosted, OpGreaterThan, 0))
	e.do(move(dr("bank", 6000), cr("reserve", 6000)))
	h := e.do(hold(600, dr("reserve", 25), cr("bank", 25))).Transfer
	e.do(voidCmd(h.ID))
	e.do(delMonCmd("again"))
	e.do(monCmd("again", "reserve", FieldPosted, OpGreaterThan, 10_000))
	e.do(move(dr("bank", 9000), cr("reserve", 9000)))
	if n := len(e.l.st.alerts); n != 5 {
		t.Fatalf("the small ledger raised %d alerts, want 5", n)
	}
}

var smallLow = &MonitorSpec{ID: "reserve-low", Account: "reserve", Condition: Condition{Field: FieldAvailable, Operator: OpLessThan, Value: 5000},
	Description: "top up the reserve"}

func (e *env) alerts() []BalanceAlert {
	e.t.Helper()
	p, err := e.l.Alerts(AlertFilter{}, 0, MaxListLimit)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.Alerts
}

func (e *env) mon(id string) *Monitor {
	e.t.Helper()
	m, err := e.l.Monitor(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

type wantAlert struct {
	monitor string
	event   AlertEvent
	entry   uint64
	value   int64 // the watched field after the entry
}

func checkAlerts(t *testing.T, got []BalanceAlert, want []wantAlert) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d alerts, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Seq != uint64(i+1) || g.Monitor != w.monitor || g.Event != w.event || g.EntrySeq != w.entry || g.Condition.Field.of(g.Balance) != w.value {
			t.Fatalf("alert %d = %+v, want %+v", i+1, g, w)
		}
	}
}

func TestMonitorAlertsOnCrossings(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	e.do(monCmd("low", "alice", FieldAvailable, OpLessThan, 100))
	e.do(monCmd("high", "alice", FieldPosted, OpGreaterOrEqual, 1000))
	e.do(monCmd("pend", "alice", FieldPending, OpNotEquals, 0))
	if m := e.mon("low"); !m.Met || m.FromSeq != 1 || m.LastAlert != 0 {
		t.Fatalf("low at creation: %+v", m)
	}
	if len(e.alerts()) != 0 {
		t.Fatal("creating a monitor whose condition holds raised an alert")
	}
	fund := e.do(move(dr("bank", 500), cr("alice", 500))).Transfer       // 1: available 500, low clears
	h := e.do(hold(600, dr("alice", 450), cr("merchant", 450))).Transfer // 2: available 50, pending -450
	e.do(voidCmd(h.ID))                                                  // 3
	e.do(move(dr("bank", 600), cr("alice", 600)))                        // 4: posted 1100
	e.do(move(dr("bank", 1), cr("bob", 1)))                              // 5: not alice
	checkAlerts(t, e.alerts(), []wantAlert{
		{"low", AlertCleared, 1, 500},
		{"low", AlertTriggered, 2, 50}, // monitors on one entry go in ID order
		{"pend", AlertTriggered, 2, -450},
		{"low", AlertCleared, 3, 500},
		{"pend", AlertCleared, 3, 0},
		{"high", AlertTriggered, 4, 1100},
	})
	a := e.alerts()[0]
	if a.Account != "alice" || a.Currency != "USD" || a.TransferID != fund.ID || a.MonitorFrom != 1 || a.Time.IsZero() ||
		a.Balance.Posted != 500 || a.Balance.CreditsPosted != 500 {
		t.Fatalf("first alert %+v", a)
	}
	if m := e.mon("low"); m.Met || m.LastAlert != 4 || m.Alerts != 3 {
		t.Fatalf("low %+v", m)
	}
	if m := e.mon("high"); !m.Met || m.LastAlert != 6 || m.Alerts != 1 {
		t.Fatalf("high %+v", m)
	}
	e.invariants()
	before := e.alerts()
	e.restart()
	if !reflect.DeepEqual(e.alerts(), before) || e.mon("low").Met {
		t.Fatal("alerts changed across a restart")
	}
	e.invariants()
	if ms := e.l.Monitors("alice"); len(ms) != 3 || ms[0].ID != "high" || ms[2].ID != "pend" {
		t.Fatalf("monitors on alice: %+v", ms)
	}
	if ms := e.l.Monitors("bob"); len(ms) != 0 || ms == nil {
		t.Fatalf("monitors on bob: %+v", ms)
	}
}

// A batch that takes an account below a threshold and back raises both
// alerts, one per entry.
func TestMonitorSeesEveryEntryOfABatch(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 500), cr("alice", 500)))
	e.do(monCmd("low", "alice", FieldAvailable, OpLessThan, 100))
	e.do(batchCmd(move(dr("alice", 450), cr("merchant", 450)), move(dr("bank", 450), cr("alice", 450))))
	checkAlerts(t, e.alerts(), []wantAlert{{"low", AlertTriggered, 2, 50}, {"low", AlertCleared, 3, 500}})
	// A refused batch leaves no alert behind.
	e.fail(batchCmd(move(dr("alice", 450), cr("merchant", 450)), move(dr("alice", 9000), cr("merchant", 9000))), CodeLimitExceeded)
	if len(e.alerts()) != 2 {
		t.Fatalf("a refused batch raised alerts: %+v", e.alerts())
	}
	e.invariants()
}

// Commands sharing one group commit see monitors exactly where they were
// created and deleted among the entries, as replay does one record at a
// time.
func TestMonitorChangesApplyBetweenTheEntriesOfACommit(t *testing.T) {
	cmds := []Command{
		move(dr("bank", 100), cr("alice", 100)), // before the monitor: no alert
		monCmd("low", "alice", FieldPosted, OpLessThan, 50),
		move(dr("alice", 60), cr("bob", 60)),  // 40: triggered
		move(dr("bank", 20), cr("alice", 20)), // 60: cleared
		delMonCmd("low"),
		move(dr("alice", 30), cr("bob", 30)), // 30: deleted, no alert
		monCmd("low", "alice", FieldPosted, OpLessThan, 50),
		monCmd("x", "bob", FieldPosted, OpGreaterThan, 0),
		delMonCmd("x"),                        // created and deleted with no entry between
		move(dr("bank", 40), cr("alice", 40)), // 70: cleared, by the second low
	}
	run := func(grouped bool) *Ledger {
		e := newEnv(t, false)
		e.standard()
		if !grouped {
			for _, c := range cmds {
				e.do(c)
			}
			return e.l
		}
		e.l.mu.Lock()
		tx := newTxn(e.l.st, e.clock.Now())
		ids := seqIDs()
		for i, c := range cmds {
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			id := ""
			if c.CreateTransfer != nil {
				id = ids()
			}
			if _, err := tx.exec(&c, id, nil); err != nil {
				t.Fatalf("command %d: %v", i, err)
			}
		}
		tx.commit()
		e.l.mu.Unlock()
		return e.l
	}
	one, grouped := run(false), run(true)
	for _, l := range []*Ledger{one, grouped} {
		p, _ := l.Alerts(AlertFilter{}, 0, 10)
		checkAlerts(t, p.Alerts, []wantAlert{{"low", AlertTriggered, 2, 40}, {"low", AlertCleared, 3, 60}, {"low", AlertCleared, 5, 70}})
		if p.Alerts[2].MonitorFrom != 5 || p.Alerts[0].MonitorFrom != 2 {
			t.Fatalf("incarnations %+v", p.Alerts)
		}
		if v := allChecks(l); len(v) > 0 {
			t.Fatal(v)
		}
	}
	a, _ := one.Alerts(AlertFilter{}, 0, 10)
	b, _ := grouped.Alerts(AlertFilter{}, 0, 10)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("grouped commit raised %+v, one at a time %+v", b, a)
	}
	if !reflect.DeepEqual(one.Monitors(""), grouped.Monitors("")) {
		t.Fatal("monitors differ")
	}
}

// A monitor created again under a deleted one's ID starts over: it takes
// the condition as it stands and its alerts are its own.
func TestMonitorRecreatedUnderItsID(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	e.do(monCmd("m", "alice", FieldPosted, OpGreaterThan, 10))
	e.do(move(dr("bank", 20), cr("alice", 20)))
	e.do(delMonCmd("m"))
	e.fail(delMonCmd("m"), CodeNotFound)
	e.do(monCmd("m", "alice", FieldPosted, OpGreaterThan, 10)) // holds already: no alert
	if m := e.mon("m"); !m.Met || m.Alerts != 0 || m.LastAlert != 0 {
		t.Fatalf("recreated %+v", m)
	}
	e.do(move(dr("alice", 15), cr("bob", 15)))
	checkAlerts(t, e.alerts(), []wantAlert{{"m", AlertTriggered, 1, 20}, {"m", AlertCleared, 2, 5}})
	e.invariants()
	e.restart()
	e.invariants()
}

func TestMonitorCommandsAreChecked(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.fail(monCmd("m", "nobody", FieldPosted, OpEquals, 0), CodeNotFound)
	e.fail(monCmd("m", "alice", "settled", OpEquals, 0), CodeInvalid)
	e.fail(monCmd("m", "alice", FieldPosted, "about", 0), CodeInvalid)
	e.fail(monCmd("", "alice", FieldPosted, OpEquals, 0), CodeInvalid)
	e.fail(monCmd("m", "bad id", FieldPosted, OpEquals, 0), CodeInvalid)
	c := monCmd("m", "alice", FieldPosted, OpEquals, 0)
	c.CreateMonitor.Description = strings.Repeat("x", MaxMonitorDescription+1)
	e.fail(c, CodeInvalid)
	c.CreateMonitor.Description = "line\nbreak"
	e.fail(c, CodeInvalid)
	e.fail(delMonCmd("bad id"), CodeInvalid)
	e.fail(delMonCmd("m"), CodeNotFound)
	e.do(monCmd("m", "alice", FieldPosted, OpEquals, 0))
	e.fail(monCmd("m", "bob", FieldPosted, OpEquals, 0), CodeAlreadyExists)
	// Closed and frozen accounts can still be watched.
	e.do(freezeCmd("bob", ""))
	e.do(monCmd("frozen", "bob", FieldPosted, OpEquals, 0))

	for i := 1; i < MaxMonitorsPerAccount; i++ {
		e.do(monCmd(fmt.Sprintf("n%04d", i), "alice", FieldAvailable, OpGreaterThan, int64(i)))
	}
	e.fail(monCmd("one-more", "alice", FieldPosted, OpEquals, 0), CodeLimitExceeded)
	e.do(monCmd("one-more", "merchant", FieldPosted, OpEquals, 0))
	e.do(delMonCmd("n0500"))
	e.do(monCmd("one-more-alice", "alice", FieldPosted, OpEquals, 0))
	// A transfer that crosses all of them raises one alert each, in ID order.
	e.do(move(dr("bank", 2000), cr("alice", 2000)))
	al := e.alerts()
	if len(al) != MaxMonitorsPerAccount {
		t.Fatalf("%d alerts, want %d", len(al), MaxMonitorsPerAccount)
	}
	if !slices.IsSortedFunc(al, func(a, b BalanceAlert) int { return strings.Compare(a.Monitor, b.Monitor) }) {
		t.Fatal("alerts of one entry are not in monitor ID order")
	}
	e.invariants()
}

// Within one txn the per-account cap counts the txn's own creations and
// deletions.
func TestMonitorCapCountsTheTxn(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.l.mu.Lock()
	defer e.l.mu.Unlock()
	tx := newTxn(e.l.st, e.clock.Now())
	for i := 0; i < MaxMonitorsPerAccount; i++ {
		if _, err := tx.createMonitor(monCmd(fmt.Sprintf("n%04d", i), "alice", FieldPosted, OpEquals, 0).CreateMonitor); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.createMonitor(monCmd("extra", "alice", FieldPosted, OpEquals, 0).CreateMonitor); err == nil || err.Code != CodeLimitExceeded {
		t.Fatalf("over the cap: %v", err)
	}
	if _, err := tx.deleteMonitor(&MonitorRef{ID: "n0000"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.createMonitor(monCmd("extra", "alice", FieldPosted, OpEquals, 0).CreateMonitor); err != nil {
		t.Fatal(err)
	}
	if all, on := tx.monitorCounts("alice"); all != MaxMonitorsPerAccount || on != MaxMonitorsPerAccount {
		t.Fatalf("counts %d %d", all, on)
	}
	tx.commit()
	if n := len(e.l.st.byAccount["alice"]); n != MaxMonitorsPerAccount || e.l.st.monitors["n0000"] != nil {
		t.Fatalf("%d monitors on alice", n)
	}
}

func TestAlertFeed(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(monCmd("a-pos", "alice", FieldPosted, OpGreaterThan, 0))
	e.do(monCmd("b-pos", "bob", FieldPosted, OpGreaterThan, 0))
	for i := 0; i < 5; i++ {
		e.do(move(dr("bank", 10), cr("alice", 10)))
		e.do(move(dr("alice", 10), cr("bob", 10)))
		e.do(move(dr("bob", 10), cr("merchant", 10)))
	}
	all := e.alerts()
	if len(all) != 20 {
		t.Fatalf("%d alerts", len(all))
	}
	// Page through with a small limit; every alert comes once, in order.
	var got []BalanceAlert
	var after uint64
	for {
		p, err := e.l.Alerts(AlertFilter{}, after, 3)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, p.Alerts...)
		if p.Next < after || p.Head != 20 {
			t.Fatalf("page %+v after %d", p, after)
		}
		after = p.Next
		if !p.HasMore() {
			break
		}
	}
	if !reflect.DeepEqual(got, all) {
		t.Fatal("paging lost or repeated alerts")
	}
	p, _ := e.l.Alerts(AlertFilter{Monitor: "b-pos"}, 0, 100)
	if len(p.Alerts) != 10 || p.Next != 20 || p.HasMore() {
		t.Fatalf("by monitor: %d alerts, next %d", len(p.Alerts), p.Next)
	}
	for _, a := range p.Alerts {
		if a.Monitor != "b-pos" || a.Account != "bob" {
			t.Fatalf("%+v", a)
		}
	}
	p, _ = e.l.Alerts(AlertFilter{Account: "alice", Monitor: "b-pos"}, 0, 100)
	if len(p.Alerts) != 0 || p.Next != 20 {
		t.Fatalf("no match: %+v", p)
	}
	// A page that fills up resumes after its last alert.
	p, _ = e.l.Alerts(AlertFilter{Account: "alice"}, 0, 2)
	if len(p.Alerts) != 2 || p.Next != p.Alerts[1].Seq || !p.HasMore() {
		t.Fatalf("full page %+v", p)
	}
	// The scan budget bounds one page; Next says where it stopped.
	old := feedScanBudget
	feedScanBudget = func(int) int { return 4 }
	p, _ = e.l.Alerts(AlertFilter{Monitor: "none"}, 0, 1)
	feedScanBudget = old
	if len(p.Alerts) != 0 || p.Next != 4 || !p.HasMore() {
		t.Fatalf("budgeted page %+v", p)
	}
	for _, bad := range []struct {
		f     AlertFilter
		after uint64
		limit int
	}{{AlertFilter{}, 21, 10}, {AlertFilter{}, 0, 0}, {AlertFilter{}, 0, MaxListLimit + 1}, {AlertFilter{Monitor: "x y"}, 0, 1}, {AlertFilter{Account: "x y"}, 0, 1}} {
		if _, err := e.l.Alerts(bad.f, bad.after, bad.limit); codeOf(err) != CodeInvalid {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if e.l.AlertHead() != 20 {
		t.Fatal(e.l.AlertHead())
	}
}

// expectedAlerts derives the alerts from the journal alone: it folds each
// account's lines entry by entry and evaluates every monitor, in ID order,
// after each entry with a line on its account, from FromSeq on.
func expectedAlerts(l *Ledger, monitors []*Monitor) []BalanceAlert {
	l.mu.RLock()
	defer l.mu.RUnlock()
	bal := map[string]Balance{}
	met := map[string]bool{}
	byAcct := map[string][]*Monitor{}
	for _, m := range monitors {
		byAcct[m.Account] = append(byAcct[m.Account], m)
	}
	for _, ms := range byAcct {
		slices.SortFunc(ms, func(a, b *Monitor) int { return strings.Compare(a.ID, b.ID) })
	}
	var out []BalanceAlert
	for i := range l.st.entries {
		e := &l.st.entries[i]
		for _, ln := range e.Lines {
			prev := bal[ln.Account]
			b, _ := prev.with(ln)
			bal[ln.Account] = b
			a := l.st.accounts[ln.Account]
			v := b.View(a.NormalBalance)
			for _, m := range byAcct[ln.Account] {
				if e.Seq < m.FromSeq {
					continue
				}
				if _, seen := met[m.ID]; !seen {
					// Nothing moved the account between the monitor's
					// creation and this entry, so its state at creation is
					// the condition of the balance before the entry.
					met[m.ID] = m.Condition.Holds(prev.View(a.NormalBalance))
				}
				now := m.Condition.Holds(v)
				if now == met[m.ID] {
					continue
				}
				met[m.ID] = now
				ev := AlertCleared
				if now {
					ev = AlertTriggered
				}
				out = append(out, BalanceAlert{Seq: uint64(len(out) + 1), Time: e.Time, Event: ev, Monitor: m.ID, MonitorFrom: m.FromSeq,
					Account: a.ID, Currency: a.Currency, Condition: m.Condition, EntrySeq: e.Seq, TransferID: e.TransferID, Balance: v})
			}
		}
	}
	return out
}

// allAlerts reads the whole alert feed.
func allAlerts(t testing.TB, l *Ledger) []BalanceAlert {
	t.Helper()
	var out []BalanceAlert
	var after uint64
	for {
		p, err := l.Alerts(AlertFilter{}, after, MaxListLimit)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Alerts...)
		after = p.Next
		if !p.HasMore() {
			return out
		}
	}
}

// The reference workload with modelMonitors raises the alerts the journal
// implies, both ways, and they survive a restart from the log and from a
// snapshot.
func TestAlertsMatchTheJournal(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		dir := t.TempDir()
		clock := newClock()
		l := openAt(t, dir, clock, func(o *Options) { o.SnapshotEvery = 50 })
		randomWorkload(t, l, clock, rand.New(rand.NewSource(seed)), 600)
		waitSnapshots(l)
		ms := l.Monitors("")
		if len(ms) != len(modelMonitors()) {
			t.Fatalf("%d monitors", len(ms))
		}
		want := expectedAlerts(l, ms)
		got := allAlerts(t, l)
		if !reflect.DeepEqual(got, want) {
			for i := range min(len(got), len(want)) {
				if !reflect.DeepEqual(got[i], want[i]) {
					t.Fatalf("seed %d: alert %d = %+v, want %+v", seed, i+1, got[i], want[i])
				}
			}
			t.Fatalf("seed %d: %d alerts, want %d", seed, len(got), len(want))
		}
		events := map[AlertEvent]int{}
		for _, a := range got {
			events[a.Event]++
		}
		if events[AlertTriggered] < 20 || events[AlertCleared] < 20 {
			t.Fatalf("seed %d: the workload crossed too little: %v", seed, events)
		}
		if v := allChecks(l); len(v) > 0 {
			t.Fatal(v)
		}
		l.Close()
		fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
		full := openAt(t, copyDir(t, dir, false), clock, nil)
		sameState(t, "alerts", fromSnap, full)
	}
}

func TestMonitorInvariantsCatchDamage(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(monCmd("low", "alice", FieldAvailable, OpLessThan, 100))
	e.do(monCmd("pos", "alice", FieldPosted, OpGreaterThan, 0))
	e.do(move(dr("bank", 500), cr("alice", 500)))
	e.do(move(dr("alice", 450), cr("bob", 450)))
	e.invariants()
	damage := func(what string, want string, f func(st *state) func()) {
		t.Helper()
		e.l.mu.Lock()
		undo := f(e.l.st)
		e.l.mu.Unlock()
		v := e.l.CheckInvariants()
		e.l.mu.Lock()
		undo()
		e.l.mu.Unlock()
		if !slices.ContainsFunc(v, func(s string) bool { return strings.Contains(s, want) }) {
			t.Fatalf("%s: violations %v, want one mentioning %q", what, v, want)
		}
		e.invariants()
	}
	damage("met flipped", "says its condition", func(st *state) func() {
		m := st.monitors["low"]
		c := *m
		c.met = !c.met
		st.monitors["low"] = &c
		return func() { st.monitors["low"] = m }
	})
	damage("alert direction", "says triggered is false", func(st *state) func() {
		st.alerts[1].up = false
		return func() { st.alerts[1].up = true }
	})
	damage("alert on another line", "is not on account alice", func(st *state) func() {
		st.alerts[0].line = 0
		return func() { st.alerts[0].line = 1 }
	})
	damage("alert repeated", "repeats the event", func(st *state) func() {
		old := st.alerts
		st.alerts = append(slices.Clone(old[:3]), alertRec{def: old[2].def, entry: old[2].entry, line: old[2].line, up: old[2].up})
		return func() { st.alerts = old }
	})
	damage("alert count", "counts", func(st *state) func() {
		m := st.monitors["pos"]
		c := *m
		c.count = 7
		st.monitors["pos"] = &c
		return func() { st.monitors["pos"] = m }
	})
	damage("definition replaced", "counts 1 alerts up to 2, the alert list holds 0", func(st *state) func() {
		m := st.monitors["pos"]
		c := *m
		d := *m.def // equal, but not the definition its alerts name
		c.def = &d
		st.monitors["pos"] = &c
		return func() { st.monitors["pos"] = m }
	})
	damage("lost account", "does not exist", func(st *state) func() {
		m := st.monitors["pos"]
		c := *m
		d := *m.def
		d.account = "ghost"
		c.def = &d
		st.monitors["pos"] = &c
		return func() { st.monitors["pos"] = m }
	})
	damage("future entry", "out of journal order", func(st *state) func() {
		st.alerts[0].entry = 99
		return func() { st.alerts[0].entry = 1 }
	})
	damage("monitor created after its alert", "created after entry", func(st *state) func() {
		old := st.alerts[2].def
		d := *old
		d.from = 99
		st.alerts[2].def = &d
		return func() { st.alerts[2].def = old }
	})
}

// Monitors created, crossed and deleted while the invariant checker runs
// do not disturb it: it checks them as of the head it froze.
func TestAuditChecksMonitorsAsFrozen(t *testing.T) {
	withAuditChunk(t, 1)
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 1000), cr("alice", 1000)))
	e.do(monCmd("low", "alice", FieldAvailable, OpLessThan, 500))
	e.do(monCmd("gone", "alice", FieldPosted, OpGreaterThan, 0))
	head := e.l.Head()
	n := 0
	saved := 0
	e.l.auditStep = func(string) {
		n++
		e.do(move(dr("alice", 600), cr("bob", 600)))  // low triggers
		e.do(move(dr("bank", 600), cr("alice", 600))) // and clears
		switch n {
		case 1:
			e.do(delMonCmd("gone"))
		case 2:
			e.do(monCmd("new", "bob", FieldPosted, OpGreaterThan, 100))
		}
		e.l.mu.RLock()
		if v := e.l.st.view; v != nil {
			saved = len(v.monSaved)
		}
		e.l.mu.RUnlock()
	}
	got, v := e.l.Audit()
	e.l.auditStep = nil
	if len(v) > 0 || got != head {
		t.Fatalf("checked %d (want %d): %v", got, head, v)
	}
	if saved < 3 {
		t.Fatalf("commits saved %d monitors into the view; the test did not exercise it", saved)
	}
	e.invariants()
}

func TestMonitorSnapshotRoundTrip(t *testing.T) {
	dir, clock := writeSmallSnapshot(t, snapVersion)
	fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
	full := openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "small", fromSnap, full)
	ctx := context.Background()
	for _, l := range []*Ledger{fromSnap, full} {
		out, err := l.Do(ctx, Command{CreateMonitor: smallLow}, key("k10", "monitor", "monitor"))
		if err != nil || !out.Replayed || out.Monitor == nil || out.Monitor.Description != smallLow.Description || !out.Monitor.Met {
			t.Fatalf("replayed create: %v %+v", err, out.Monitor)
		}
		if _, err := l.Do(ctx, monCmd("nobody", "missing", FieldPosted, OpEquals, 0), key("k10", "monitor-missing", "monitor-missing")); codeOf(err) != CodeNotFound {
			t.Fatalf("replayed refusal: %v", err)
		}
		m, err := l.Monitor("again")
		if err != nil || m.Condition.Value != 10_000 || m.FromSeq == 1 {
			t.Fatalf("again: %+v %v", m, err)
		}
		if v := allChecks(l); len(v) > 0 {
			t.Fatal(v)
		}
	}
}

// A version 8 snapshot, written before monitors existed, still loads.
func TestVersion8SnapshotsLoad(t *testing.T) {
	old := snapWriteVersion
	snapWriteVersion = 8
	t.Cleanup(func() { snapWriteVersion = old })
	dir, clock := writeSmallSnapshot(t, 8)
	snapWriteVersion = old
	fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
	full := openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "v8", fromSnap, full)
	if ms := fromSnap.Monitors(""); len(ms) != 0 {
		t.Fatal(ms)
	}
}

// Monitor fan-out on the commit path: one transfer evaluated against 0 or
// 1,000 monitors on the account it moves, with every monitor crossing each
// time (1,000 alerts per transfer) or none of them.
func BenchmarkMonitorFanout(b *testing.B) {
	for _, c := range []struct {
		name     string
		monitors int
		cross    bool
	}{{"monitors=0", 0, false}, {"monitors=1000/crossing=none", 1000, false}, {"monitors=1000/crossing=all", 1000, true}} {
		b.Run(c.name, func(b *testing.B) {
			e := newEnv(b, false)
			e.standard()
			e.do(move(dr("bank", 1_000_000), cr("alice", 1_000_000)))
			threshold := int64(1)
			if c.cross {
				threshold = 1_000_000 // the balance swings across it every transfer
			}
			for i := 0; i < c.monitors; i++ {
				e.do(monCmd(fmt.Sprintf("m%04d", i), "alice", FieldPosted, OpGreaterOrEqual, threshold))
			}
			out := move(dr("alice", 500), cr("bob", 500))
			back := move(dr("bob", 500), cr("alice", 500))
			alerts0 := e.l.AlertHead()
			b.ReportAllocs()
			b.ResetTimer()
			start := time.Now()
			for i := 0; i < b.N; i++ {
				if i%2 == 0 {
					e.do(out)
				} else {
					e.do(back)
				}
			}
			el := time.Since(start)
			b.StopTimer()
			n := e.l.AlertHead() - alerts0
			b.ReportMetric(float64(n)/float64(b.N), "alerts/op")
			if n > 0 {
				b.ReportMetric(float64(el.Nanoseconds())/float64(n), "ns/alert")
			}
		})
	}
}

// Reading the alert feed: one page of the 1,000 alerts a single transfer
// raised across 1,000 monitors, expanded from the stored records.
func BenchmarkAlertFeedPage(b *testing.B) {
	e := newEnv(b, false)
	e.standard()
	for i := 0; i < 1000; i++ {
		e.do(monCmd(fmt.Sprintf("m%04d", i), "alice", FieldPosted, OpGreaterThan, 0))
	}
	e.do(move(dr("bank", 1), cr("alice", 1)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := e.l.Alerts(AlertFilter{}, 0, 1000)
		if err != nil || len(p.Alerts) != 1000 {
			b.Fatal(len(p.Alerts), err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1000, "ns/alert")
}

// A stored alert stays 24 bytes: it is the one part of monitoring that
// grows with traffic.
func TestAlertRecordSize(t *testing.T) {
	if n := unsafe.Sizeof(alertRec{}); n != 24 {
		t.Fatalf("alertRec is %d bytes", n)
	}
}
