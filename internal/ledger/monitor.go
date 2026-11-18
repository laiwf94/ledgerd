package ledger

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// Balance monitors watch one account's posted, pending or available
// balance against a threshold and raise a balance alert each time the
// condition changes: "triggered" when an entry makes it true, "cleared"
// when an entry makes it false again. A monitor is part of the ledger's
// state, changed only by the logged create_monitor and delete_monitor
// commands, and its alerts are derived from the journal as each batch
// commits, so replay rebuilds exactly the same alerts in the same order,
// snapshots carry them, and ledgerd verify compares them.
//
// A monitor is evaluated after every journal line on its account, against
// the balance right after that entry, so a batch that dips below a
// threshold and comes back raises both alerts. Monitors on one account are
// evaluated in ID order, so the alerts one entry raises have a fixed order.
// Creating a monitor records whether its condition already holds and
// raises no alert: alerts mark crossings, not states.
//
// Alerts form a feed of their own, numbered from 1 in the order they were
// raised, which is journal order. Each names the entry that caused it, so a
// consumer can line the two feeds up. The alert list is append-only and an
// alert never changes, so the feed reads it without holding the state lock,
// as the journal feed does.

// Monitor limits.
const (
	MaxMonitors           = 100_000
	MaxMonitorsPerAccount = 1000
	MaxMonitorDescription = 256
)

// BalanceField names the balance a monitor watches. Each is signed in the
// account's normal-balance sense, as BalanceView shows it.
type BalanceField string

// Balance fields.
const (
	FieldPosted    BalanceField = "posted"
	FieldPending   BalanceField = "pending"
	FieldAvailable BalanceField = "available"
)

func (f BalanceField) valid() bool {
	return f == FieldPosted || f == FieldPending || f == FieldAvailable
}

func (f BalanceField) of(v BalanceView) int64 {
	switch f {
	case FieldPosted:
		return v.Posted
	case FieldPending:
		return v.Pending
	}
	return v.Available
}

// MonitorOperator compares the watched balance with the monitor's value.
type MonitorOperator string

// Monitor operators.
const (
	OpLessThan       MonitorOperator = "less_than"
	OpLessOrEqual    MonitorOperator = "less_than_or_equals"
	OpEquals         MonitorOperator = "equals"
	OpNotEquals      MonitorOperator = "not_equals"
	OpGreaterOrEqual MonitorOperator = "greater_than_or_equals"
	OpGreaterThan    MonitorOperator = "greater_than"
)

func (o MonitorOperator) valid() bool {
	switch o {
	case OpLessThan, OpLessOrEqual, OpEquals, OpNotEquals, OpGreaterOrEqual, OpGreaterThan:
		return true
	}
	return false
}

func (o MonitorOperator) holds(x, v int64) bool {
	switch o {
	case OpLessThan:
		return x < v
	case OpLessOrEqual:
		return x <= v
	case OpEquals:
		return x == v
	case OpNotEquals:
		return x != v
	case OpGreaterOrEqual:
		return x >= v
	}
	return x > v
}

// Condition is what a monitor watches for: Field Operator Value.
type Condition struct {
	Field    BalanceField    `json:"field"`
	Operator MonitorOperator `json:"operator"`
	Value    int64           `json:"value"`
}

// Holds reports whether the condition is true of a balance.
func (c Condition) Holds(v BalanceView) bool { return c.Operator.holds(c.Field.of(v), c.Value) }

func (c Condition) validate() *Error {
	if !c.Field.valid() {
		return errf(CodeInvalid, "field must be posted, pending or available")
	}
	if !c.Operator.valid() {
		return errf(CodeInvalid, "operator must be one of less_than, less_than_or_equals, equals, not_equals, "+
			"greater_than_or_equals or greater_than")
	}
	return nil
}

// Monitor is a balance monitor as clients see it: its definition and
// whether its condition holds now.
type Monitor struct {
	ID          string    `json:"id"`
	Account     string    `json:"account"`
	Condition   Condition `json:"condition"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	// FromSeq is the first journal entry the monitor watches.
	FromSeq uint64 `json:"from_seq"`
	// Met is whether the condition holds of the account's balance now.
	Met bool `json:"condition_met"`
	// LastAlert is the alert raised by the most recent crossing, 0 while
	// there has been none.
	LastAlert uint64 `json:"last_alert_seq"`
	// Alerts counts the alerts the monitor has raised.
	Alerts uint64 `json:"alerts"`
}

// monitorDef is what never changes about one monitor: one per
// create_monitor, shared by the monitor's state and its alerts. A monitor
// deleted and created again under its ID gets a new one.
type monitorDef struct {
	id      string
	account string
	cond    Condition
	desc    string
	created time.Time
	from    uint64
}

// monState is a monitor as the ledger keeps it. Commit changes it in place
// under the write lock when a crossing flips it, so a crossing allocates
// nothing; readers copy it under the read lock, and the invariant checker's
// view and snapshots keep copies of their own.
type monState struct {
	def   *monitorDef
	met   bool
	last  uint64 // the newest alert's sequence, 0 for none
	count uint64 // alerts raised
}

func (m *monState) view() *Monitor {
	d := m.def
	return &Monitor{ID: d.id, Account: d.account, Condition: d.cond, Description: d.desc, CreatedAt: d.created, FromSeq: d.from,
		Met: m.met, LastAlert: m.last, Alerts: m.count}
}

// alertRec is a balance alert as the ledger keeps it. Its sequence number
// is its position in the list, and everything else a BalanceAlert shows
// comes from the monitor's definition, the journal entry and the account's
// history row for the entry's line, so a stored alert costs 24 bytes
// however much it carries.
type alertRec struct {
	def   *monitorDef
	entry uint64
	line  uint32 // index of the line in the entry, which is on def.account
	up    bool   // triggered; cleared when false
}

// AlertEvent says which way a condition crossed.
type AlertEvent string

// Alert events.
const (
	AlertTriggered AlertEvent = "triggered" // the condition became true
	AlertCleared   AlertEvent = "cleared"   // the condition became false
)

// BalanceAlert is one crossing of a monitor's condition. It never changes.
type BalanceAlert struct {
	// Seq numbers alerts from 1 in the order they were raised.
	Seq     uint64     `json:"seq"`
	Time    time.Time  `json:"time"`
	Event   AlertEvent `json:"event"`
	Monitor string     `json:"monitor"`
	// MonitorFrom is the monitor's FromSeq. A monitor deleted and created
	// again under the same ID starts over; this tells the two apart.
	MonitorFrom uint64    `json:"monitor_from_seq"`
	Account     string    `json:"account"`
	Currency    string    `json:"currency"`
	Condition   Condition `json:"condition"`
	// EntrySeq and TransferID name the journal entry that crossed it, and
	// Balance is the account's balance right after that entry.
	EntrySeq   uint64      `json:"entry_seq"`
	TransferID string      `json:"transfer_id"`
	Balance    BalanceView `json:"balance"`
}

// MonitorSpec creates a monitor.
type MonitorSpec struct {
	ID          string    `json:"id"`
	Account     string    `json:"account"`
	Condition   Condition `json:"condition"`
	Description string    `json:"description,omitempty"`
}

// MonitorRef names a monitor, to delete it.
type MonitorRef struct {
	ID string `json:"id"`
}

func (s *MonitorSpec) validate() *Error {
	if !ValidID(s.ID) {
		return errf(CodeInvalid, "monitor id must be 1 to %d characters of letters, digits and _ . : -", MaxIDLength)
	}
	if !ValidID(s.Account) {
		return errf(CodeInvalid, "account is not a valid account id")
	}
	if !validText(s.Description, MaxMonitorDescription) {
		return errf(CodeInvalid, "description must be at most %d characters of printable text", MaxMonitorDescription)
	}
	return s.Condition.validate()
}

func (s *MonitorRef) validate() *Error {
	if !ValidID(s.ID) {
		return errf(CodeInvalid, "monitor id is not valid")
	}
	return nil
}

// monitorOp is a monitor created or deleted by a txn. pos is the journal
// sequence the txn would give its next entry when the command ran, so
// commit applies it between the right entries.
type monitorOp struct {
	pos uint64
	id  string
	m   *monState // nil deletes
}

// monitor returns the txn's view of a monitor's definition, or nil. The
// txn never changes a monitor's state; only commit does.
func (t *txn) monitor(id string) *monitorDef {
	for i := len(t.monOps) - 1; i >= 0; i-- {
		if t.monOps[i].id == id {
			if m := t.monOps[i].m; m != nil {
				return m.def
			}
			return nil
		}
	}
	if m := t.st.monitors[id]; m != nil {
		return m.def
	}
	return nil
}

// monitorCounts returns how many monitors exist and how many watch acct,
// with the txn's own creations and deletions counted.
func (t *txn) monitorCounts(acct string) (all, onAccount int) {
	all, onAccount = len(t.st.monitors), len(t.st.byAccount[acct])
	seen := map[string]bool{}
	for i := len(t.monOps) - 1; i >= 0; i-- {
		op := t.monOps[i]
		if seen[op.id] {
			continue
		}
		seen[op.id] = true
		var before *monitorDef
		if m := t.st.monitors[op.id]; m != nil {
			before = m.def
		}
		d := 0
		switch {
		case op.m != nil && before == nil:
			d = 1
		case op.m == nil && before != nil:
			d = -1
		}
		all += d
		a := before
		if op.m != nil {
			a = op.m.def
		}
		if a != nil && a.account == acct {
			onAccount += d
		}
	}
	return all, onAccount
}

// peek returns the txn's view of an account without copying it into the
// txn.
func (t *txn) peek(id string) *account {
	if a, ok := t.accounts[id]; ok {
		return a
	}
	return t.st.accounts[id]
}

func (t *txn) createMonitor(s *MonitorSpec) (Outcome, *Error) {
	if t.monitor(s.ID) != nil {
		return Outcome{}, errf(CodeAlreadyExists, "monitor %s already exists", s.ID)
	}
	a := t.peek(s.Account)
	if a == nil {
		return Outcome{}, errf(CodeNotFound, "account %s does not exist", s.Account)
	}
	all, on := t.monitorCounts(s.Account)
	if all >= MaxMonitors {
		return Outcome{}, errf(CodeLimitExceeded, "at most %d balance monitors", MaxMonitors)
	}
	if on >= MaxMonitorsPerAccount {
		return Outcome{}, errf(CodeLimitExceeded, "account %s already has %d balance monitors", s.Account, MaxMonitorsPerAccount)
	}
	d := &monitorDef{id: s.ID, account: s.Account, cond: s.Condition, desc: s.Description, created: t.now, from: t.nextSeq()}
	m := &monState{def: d, met: s.Condition.Holds(a.bal.View(a.NormalBalance))}
	t.monOps = append(t.monOps, monitorOp{pos: d.from, id: d.id, m: m})
	return Outcome{Monitor: m.view()}, nil
}

func (t *txn) deleteMonitor(s *MonitorRef) (Outcome, *Error) {
	var out *Monitor
	for i := len(t.monOps) - 1; i >= 0 && out == nil; i-- {
		if op := t.monOps[i]; op.id == s.ID {
			if op.m == nil {
				break
			}
			out = op.m.view()
		}
	}
	if out == nil && t.monitor(s.ID) != nil {
		out = t.st.monitors[s.ID].view()
	}
	if out == nil {
		return Outcome{}, errf(CodeNotFound, "monitor %s does not exist", s.ID)
	}
	t.monOps = append(t.monOps, monitorOp{pos: t.nextSeq(), id: s.ID})
	return Outcome{Monitor: out}, nil
}

func monitorOrder(x *monState, id string) int { return strings.Compare(x.def.id, id) }

// addMonitor files a monitor under its account, keeping each account's
// monitors in ID order. The caller holds the write lock. The per-account
// slices change in place: readers copy them under the read lock.
func (s *state) addMonitor(m *monState) {
	s.keepMonitor(m.def.id)
	s.monitors[m.def.id] = m
	ms := s.byAccount[m.def.account]
	i, _ := slices.BinarySearchFunc(ms, m.def.id, monitorOrder)
	s.byAccount[m.def.account] = slices.Insert(ms, i, m)
}

func (s *state) removeMonitor(id string) {
	m := s.monitors[id]
	if m == nil {
		return
	}
	s.keepMonitor(id)
	delete(s.monitors, id)
	ms := s.byAccount[m.def.account]
	if len(ms) == 1 {
		delete(s.byAccount, m.def.account)
		return
	}
	i, _ := slices.BinarySearchFunc(ms, id, monitorOrder)
	s.byAccount[m.def.account] = slices.Delete(ms, i, i+1)
}

// keepMonitor lets a running invariant check keep the monitor filed under
// id as it was at its head.
func (s *state) keepMonitor(id string) {
	if s.view != nil {
		s.view.keepMonitor(id, s.monitors[id])
	}
}

// commitMonitors applies the txn's monitor changes and evaluates every
// monitor against the balance after each of the txn's journal lines on its
// account, appending an alert for each crossing. It runs inside txn.commit,
// under the write lock, after the accounts have been updated.
func (t *txn) commitMonitors() {
	st := t.st
	if len(t.monOps) == 0 && len(st.monitors) == 0 {
		return
	}
	next := 0
	applyUpTo := func(seq uint64) {
		for ; next < len(t.monOps) && t.monOps[next].pos <= seq; next++ {
			op := t.monOps[next]
			st.removeMonitor(op.id)
			if op.m != nil {
				st.addMonitor(op.m)
			}
		}
	}
	for i := range t.rows {
		r := &t.rows[i]
		applyUpTo(r.row.seq)
		ms := st.byAccount[r.account]
		if len(ms) == 0 {
			continue
		}
		view := r.row.balance.View(st.accounts[r.account].NormalBalance)
		for _, m := range ms {
			met := m.def.cond.Holds(view)
			if met == m.met {
				continue
			}
			st.keepMonitor(m.def.id)
			st.alerts = append(st.alerts, alertRec{def: m.def, entry: r.row.seq, line: uint32(r.row.line), up: met})
			m.met, m.last, m.count = met, uint64(len(st.alerts)), m.count+1
		}
	}
	applyUpTo(^uint64(0))
}

// Monitors returns every monitor, sorted by ID, optionally only those on one
// account.
func (l *Ledger) Monitors(account string) []*Monitor {
	l.mu.RLock()
	var out []*Monitor
	if account != "" {
		ms := l.st.byAccount[account]
		out = make([]*Monitor, len(ms))
		for i, m := range ms {
			out[i] = m.view()
		}
	} else {
		out = make([]*Monitor, 0, len(l.st.monitors))
		for _, m := range l.st.monitors {
			out = append(out, m.view())
		}
	}
	l.mu.RUnlock()
	slices.SortFunc(out, func(a, b *Monitor) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Monitor returns one monitor.
func (l *Ledger) Monitor(id string) (*Monitor, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	m := l.st.monitors[id]
	if m == nil {
		return nil, errf(CodeNotFound, "monitor %s does not exist", id)
	}
	return m.view(), nil
}

// AlertFilter selects balance alerts. Empty fields match everything.
type AlertFilter struct {
	Monitor string
	Account string
}

func (f *AlertFilter) validate() *Error {
	if f.Monitor != "" && !ValidID(f.Monitor) {
		return errf(CodeInvalid, "%q is not a valid monitor id", f.Monitor)
	}
	if f.Account != "" && !ValidID(f.Account) {
		return errf(CodeInvalid, "%q is not a valid account id", f.Account)
	}
	return nil
}

func (f *AlertFilter) matches(a *alertRec) bool {
	return (f.Monitor == "" || a.def.id == f.Monitor) && (f.Account == "" || a.def.account == f.Account)
}

// alertSource expands stored alerts for one feed request. The journal and
// each account's history rows are append-only, so the prefixes it notes
// under the read lock stay valid after it is released.
type alertSource struct {
	l       *Ledger
	journal []Entry
	accts   map[string]alertAccount
}

type alertAccount struct {
	cur    string
	normal Direction
	rows   []row
}

func (src *alertSource) account(id string) alertAccount {
	if a, ok := src.accts[id]; ok {
		return a
	}
	src.l.mu.RLock()
	acct := src.l.st.accounts[id]
	a := alertAccount{cur: acct.Currency, normal: acct.NormalBalance, rows: acct.rows}
	src.l.mu.RUnlock()
	src.accts[id] = a
	return a
}

// expand builds the alert numbered seq.
func (src *alertSource) expand(seq uint64, r *alertRec) BalanceAlert {
	e := &src.journal[r.entry-1]
	a := src.account(r.def.account)
	al := BalanceAlert{Seq: seq, Time: e.Time, Event: AlertCleared, Monitor: r.def.id, MonitorFrom: r.def.from, Account: r.def.account,
		Currency: a.cur, Condition: r.def.cond, EntrySeq: r.entry, TransferID: e.TransferID}
	if r.up {
		al.Event = AlertTriggered
	}
	if b, ok := rowBalance(a.rows, r.entry, int(r.line)); ok {
		al.Balance = b.View(a.normal)
	}
	return al
}

// rowBalance finds the balance an account's history holds after line of
// entry seq.
func rowBalance(rows []row, seq uint64, line int) (Balance, bool) {
	for k := sort.Search(len(rows), func(k int) bool { return rows[k].seq >= seq }); k < len(rows) && rows[k].seq == seq; k++ {
		if rows[k].line == line {
			return rows[k].balance, true
		}
	}
	return Balance{}, false
}

// AlertPage is one page of the alert feed. Next and Head work as in
// EntryPage, in alert sequence numbers.
type AlertPage struct {
	Alerts []BalanceAlert
	Next   uint64
	Head   uint64
}

// HasMore reports whether alerts the page did not examine were already
// raised when it was read.
func (p AlertPage) HasMore() bool { return p.Next < p.Head }

// Alerts returns up to limit balance alerts matching f with sequence numbers
// after after, oldest first. Like the journal feed it takes the read lock
// only to note the head (and, once per account on the page, the account's
// history rows), and scans with no lock held.
func (l *Ledger) Alerts(f AlertFilter, after uint64, limit int) (AlertPage, error) {
	if limit <= 0 || limit > MaxListLimit {
		return AlertPage{}, errf(CodeInvalid, "limit must be between 1 and %d", MaxListLimit)
	}
	if err := f.validate(); err != nil {
		return AlertPage{}, err
	}
	l.mu.RLock()
	alerts, journal := l.st.alerts, l.st.entries
	l.mu.RUnlock()
	src := alertSource{l: l, journal: journal, accts: map[string]alertAccount{}}
	head := uint64(len(alerts))
	if after > head {
		return AlertPage{}, errf(CodeInvalid, "after_seq %d is after the newest alert %d", after, head)
	}
	page := AlertPage{Next: head, Head: head}
	if n := head - after; n > 0 && (f == AlertFilter{}) {
		page.Alerts = make([]BalanceAlert, 0, min(n, uint64(limit)))
	}
	budget := feedScanBudget(limit)
	scanned := 0
	for seq := after + 1; seq <= head; seq++ {
		a := &alerts[seq-1]
		if f.matches(a) {
			if len(page.Alerts) == limit {
				page.Next = page.Alerts[limit-1].Seq
				return page, nil
			}
			page.Alerts = append(page.Alerts, src.expand(seq, a))
		}
		if scanned++; scanned == budget {
			page.Next = seq
			return page, nil
		}
	}
	return page, nil
}

// AlertHead is the sequence of the newest balance alert, 0 when none.
func (l *Ledger) AlertHead() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return uint64(len(l.st.alerts))
}

// monitorProblems checks the monitors and alerts as of a journal head
// against the journal and the accounts: every monitor watches an account
// that exists from an entry no later than head+1 and says truly whether
// its condition holds now, and its alert count and last alert match the
// list; alerts name entries in journal order, each on a line of its
// monitor's account, say triggered exactly when the condition holds of the
// balance the account's history has after that line, and alternate per
// monitor.
func monitorProblems(monitors map[string]monState, alerts []alertRec, entries []Entry, acct func(id string) *acctCheck, bad func(string, ...any)) {
	head := uint64(len(entries))
	if len(monitors) > MaxMonitors {
		bad("%d balance monitors, more than %d", len(monitors), MaxMonitors)
	}
	type tally struct {
		last  uint64 // sequence of the newest alert
		up    bool
		count uint64
	}
	seen := map[*monitorDef]*tally{}
	var prevEntry uint64
	for i := range alerts {
		al := &alerts[i]
		seq := uint64(i) + 1
		if al.def == nil || al.entry == 0 || al.entry > head || al.entry < prevEntry {
			bad("balance alert %d names entry %d, out of journal order", seq, al.entry)
			return
		}
		prevEntry = al.entry
		e := &entries[al.entry-1]
		a := acct(al.def.account)
		switch {
		case al.def.cond.validate() != nil || al.entry < al.def.from:
			bad("balance alert %d comes from an invalid monitor or one created after entry %d", seq, al.entry)
		case int(al.line) >= len(e.Lines) || e.Lines[al.line].Account != al.def.account || a == nil:
			bad("balance alert %d names line %d of entry %d, which is not on account %s", seq, al.line, al.entry, al.def.account)
		default:
			b, ok := rowBalance(a.rows, al.entry, int(al.line))
			if !ok {
				bad("balance alert %d: account %s's history has no row for entry %d", seq, al.def.account, al.entry)
			} else if al.def.cond.Holds(b.View(a.normal)) != al.up {
				bad("balance alert %d says triggered is %v but its condition is %v", seq, al.up, !al.up)
			}
		}
		t := seen[al.def]
		if t == nil {
			t = &tally{}
			seen[al.def] = t
		} else if t.up == al.up {
			bad("balance alert %d repeats the event of alert %d of monitor %s", seq, t.last, al.def.id)
		}
		t.last, t.up = seq, al.up
		t.count++
	}
	ids := make([]string, 0, len(monitors))
	for id := range monitors {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		m := monitors[id]
		if m.def == nil || m.def.id != id {
			bad("monitor %s is filed under the wrong ID", id)
			continue
		}
		d := m.def
		a := acct(d.account)
		t := seen[d]
		if t == nil {
			t = &tally{}
		}
		switch {
		case a == nil:
			bad("monitor %s watches account %s, which does not exist", id, d.account)
		case d.from == 0 || d.from > head+1:
			bad("monitor %s watches from entry %d, past the head %d", id, d.from, head)
		case d.cond.validate() != nil || !validText(d.desc, MaxMonitorDescription):
			bad("monitor %s has an invalid condition or description", id)
		case d.cond.Holds(a.bal.View(a.normal)) != m.met:
			bad("monitor %s says its condition is %v, but account %s's balance makes it %v", id, m.met, d.account, !m.met)
		case m.count != t.count || m.last != t.last:
			bad("monitor %s counts %d alerts up to %d, the alert list holds %d up to %d", id, m.count, m.last, t.count, t.last)
		case t.count > 0 && t.up != m.met:
			bad("monitor %s's last alert %d disagrees with its state", id, m.last)
		}
	}
}
