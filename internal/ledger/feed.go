package ledger

import (
	"sort"
	"strings"
	"time"
)

// The journal feed lets a consumer follow every journal entry in sequence
// order and resume exactly where it stopped: it keeps the last sequence it
// processed and asks for the entries after it.
//
// The journal is append-only and an entry never changes once it is
// committed, so a page reads the journal without holding the state lock. A
// request takes the read lock only to note the head (and, with an account
// filter, the account's history rows up to it), which is O(1); the scan,
// the filtering and the encoding of the page all happen with no lock held
// (a currency filter that meets a hold extension, which has no lines, looks
// up its transfer's currencies under the read lock, also O(1)),
// so a feed reader never delays a commit and a commit never changes a page
// being read.
//
// Next is the last sequence a page examined, not the last one it returned:
// an entry that does not match the filter now never will, so a consumer
// that resumes from Next skips nothing it could have wanted and never sees
// an entry twice. A page that reaches the head returns the head as Next, so
// a filtered consumer at the tail does not scan the same entries again on
// its next poll.

// Feed limits.
const (
	// MaxFeedKinds is how many entry kinds one feed request can filter on:
	// every kind there is.
	MaxFeedKinds   = 8
	feedScanFactor = 64
	minFeedScan    = 1 << 16
)

// feedScanBudget is how many entries one page may examine. No lock is held
// while it scans, so the budget only bounds the work of one request.
var feedScanBudget = func(limit int) int { return max(limit*feedScanFactor, minFeedScan) }

// EntryFilter selects journal entries. Empty fields match everything; an
// entry must match every field given.
type EntryFilter struct {
	// Account matches entries with a line on that account. An
	// extend_pending entry has no lines, so it matches no account.
	Account string
	// Kinds matches entries of any of these kinds.
	Kinds []EntryKind
	// Currency matches entries with a line in that currency: an entry in
	// that currency, or a conversion with that as its other currency.
	Currency string
	// Alerts opts in to balance alerts, and AlertMonitor and AlertAccount
	// narrow the alerts wanted to one monitor or the monitors on one
	// account. With AlertsInclude the feed selects the entries the fields
	// above match and, as well, every entry that raised a wanted alert;
	// with AlertsOnly it selects only entries that raised a wanted alert
	// and that the fields above match. Either way each selected entry
	// carries the wanted alerts it raised, so a consumer's position in the
	// journal is its position in the alerts too.
	Alerts       AlertMode
	AlertMonitor string
	AlertAccount string
}

// AlertMode says whether a feed carries balance alerts.
type AlertMode string

// Alert modes.
const (
	AlertsOff     AlertMode = ""
	AlertsInclude AlertMode = "include"
	AlertsOnly    AlertMode = "only"
)

func (m AlertMode) valid() bool { return m == AlertsOff || m == AlertsInclude || m == AlertsOnly }

// alertFilter returns the alerts f wants.
func (f *EntryFilter) alertFilter() AlertFilter {
	return AlertFilter{Monitor: f.AlertMonitor, Account: f.AlertAccount}
}

func (k EntryKind) valid() bool {
	switch k {
	case KindPosted, KindPending, KindPostPending, KindVoidPending, KindExpirePending, KindReversal,
		KindIncreasePending, KindExtendPending:
		return true
	}
	return false
}

func (f *EntryFilter) validate() *Error {
	if f.Account != "" && !ValidID(f.Account) {
		return errf(CodeInvalid, "%q is not a valid account id", f.Account)
	}
	if len(f.Kinds) > MaxFeedKinds {
		return errf(CodeInvalid, "at most %d kinds", MaxFeedKinds)
	}
	for _, k := range f.Kinds {
		if !k.valid() {
			return errf(CodeInvalid, "kind must be one of posted, pending, post_pending, void_pending, expire_pending, "+
				"reversal, increase_pending or extend_pending")
		}
	}
	if f.Currency != "" {
		if err := validateCurrency(f.Currency); err != nil {
			return err
		}
	}
	if !f.Alerts.valid() {
		return errf(CodeInvalid, "alerts must be include or only")
	}
	if f.Alerts == AlertsOff && (f.AlertMonitor != "" || f.AlertAccount != "") {
		return errf(CodeInvalid, "alert_monitor and alert_account need alerts set to include or only")
	}
	af := f.alertFilter()
	return af.validate()
}

// matches reports whether e matches f. quote returns the other currency of
// a multi-currency transfer, or "", for the entries that do not carry it.
func (f *EntryFilter) matches(e *Entry, quote func(transferID string) string) bool {
	if len(f.Kinds) > 0 {
		ok := false
		for _, k := range f.Kinds {
			ok = ok || e.Kind == k
		}
		if !ok {
			return false
		}
	}
	if f.Currency != "" && !e.inCurrency(f.Currency, quote) {
		return false
	}
	if f.Account != "" {
		for i := range e.Lines {
			if e.Lines[i].Account == f.Account {
				return true
			}
		}
		return false
	}
	return true
}

// inCurrency reports whether the entry is in cur: its transfer's currency,
// or a currency one of its lines is in. An extend_pending entry has no
// lines; it is in its transfer's currencies, and quote names the other one
// of a multi-currency transfer.
func (e *Entry) inCurrency(cur string, quote func(transferID string) string) bool {
	if e.Currency == cur || e.FX != nil && e.FX.Quote == cur {
		return true
	}
	for i := range e.Lines {
		if e.Lines[i].Currency == cur {
			return true
		}
	}
	return e.Kind == KindExtendPending && quote(e.TransferID) == cur
}

// quoteCurrency returns the other currency of a multi-currency transfer, or
// "" for a transfer in one currency. A transfer's currencies never change,
// so the answer holds after the lock is released.
func (l *Ledger) quoteCurrency(id string) string {
	l.mu.RLock()
	tr := l.st.transfers[id]
	l.mu.RUnlock()
	if tr == nil || tr.FX == nil {
		return ""
	}
	return tr.FX.Quote
}

// EntryPage is one page of the journal feed.
type EntryPage struct {
	// Entries are in sequence order and must not be modified.
	Entries []Entry
	// Alerts holds, when the filter opted in to alerts, the wanted alerts
	// each entry raised, in the order they were raised: Alerts[i] belongs
	// to Entries[i] and is empty for an entry that raised none. It is nil
	// when the filter did not opt in.
	Alerts [][]BalanceAlert
	// Next is the sequence to pass as after for the following page: the
	// last sequence this page examined, which is at least the last entry
	// on it. It is never less than the after the page was asked for.
	Next uint64
	// Head is the newest journal entry when the page was read. Entries
	// after Next up to Head have not been examined yet; when Next is Head,
	// the consumer has seen everything written so far.
	Head uint64
}

// HasMore reports whether entries the page did not examine were already
// written when it was read.
func (p EntryPage) HasMore() bool { return p.Next < p.Head }

// FeedEntry is a journal entry with the balance alerts it raised that the
// feed opted in to. Without alerts it encodes exactly as the Entry does.
type FeedEntry struct {
	Entry
	Alerts []BalanceAlert `json:"alerts,omitempty"`
}

// Item returns the page's i-th entry with its alerts.
func (p EntryPage) Item(i int) FeedEntry {
	fe := FeedEntry{Entry: p.Entries[i]}
	if p.Alerts != nil {
		fe.Alerts = p.Alerts[i]
	}
	return fe
}

// Items returns the page's entries with their alerts.
func (p EntryPage) Items() []FeedEntry {
	out := make([]FeedEntry, len(p.Entries))
	for i := range out {
		out[i] = p.Item(i)
	}
	return out
}

// AlertCount is the number of alerts the page carries.
func (p EntryPage) AlertCount() int {
	n := 0
	for _, a := range p.Alerts {
		n += len(a)
	}
	return n
}

// alertYield is how many alerts an alert walk examines without finding a
// wanted one before it hands the entry it reached to the page anyway, so the
// page's scan budget bounds the walk.
var alertYield = 1024

// Entries returns up to limit journal entries matching f with sequence
// numbers after after, oldest first.
func (l *Ledger) Entries(f EntryFilter, after uint64, limit int) (EntryPage, error) {
	if limit <= 0 || limit > MaxListLimit {
		return EntryPage{}, errf(CodeInvalid, "limit must be between 1 and %d", MaxListLimit)
	}
	if err := f.validate(); err != nil {
		return EntryPage{}, err
	}
	// Note the journal, the alerts and, for an account, its history rows.
	// All are append-only, and an alert is appended in the same commit as
	// the entry that raised it, so the prefixes agree and stay valid after
	// the lock is released.
	l.mu.RLock()
	journal := l.st.entries
	var alerts []alertRec
	if f.Alerts != AlertsOff {
		alerts = l.st.alerts
	}
	var rows []row
	var known bool
	if f.Account != "" {
		if a, ok := l.st.accounts[f.Account]; ok {
			rows, known = a.rows, true
		}
	}
	l.mu.RUnlock()
	head := uint64(len(journal))
	if after > head {
		return EntryPage{}, errf(CodeInvalid, "after_seq %d is after the newest entry %d", after, head)
	}
	page := EntryPage{Next: head, Head: head}
	if f.Alerts != AlertsOff {
		page.Alerts = [][]BalanceAlert{}
	}
	if f.Account != "" && !known && f.Alerts != AlertsInclude {
		// No entry so far touches an account that does not exist.
		return page, nil
	}

	// The walk visits candidate sequences in increasing order: every entry,
	// or the account's own rows (one per line, so an entry with two lines on
	// the account is visited once), or the entries that raised a wanted
	// alert, or the last two merged.
	next := l.baseSeqs(rows, f.Account != "", after, head)
	af := f.alertFilter()
	firstAlert := sort.Search(len(alerts), func(i int) bool { return alerts[i].entry > after })
	switch f.Alerts {
	case AlertsOnly:
		next = alertSeqs(alerts, firstAlert, &af)
	case AlertsInclude:
		if f.Account != "" {
			next = mergeSeqs(next, alertSeqs(alerts, firstAlert, &af))
		}
	}
	src := alertSource{l: l, journal: journal, accts: map[string]alertAccount{}}
	ai := firstAlert // the first alert not before the entry being examined

	budget := feedScanBudget(limit)
	scanned := 0
	for seq, ok := next(); ok; seq, ok = next() {
		l.feedStep(seq)
		e := &journal[seq-1]
		var raised []BalanceAlert
		if alerts != nil {
			for ai < len(alerts) && alerts[ai].entry < seq {
				ai++
			}
			for k := ai; k < len(alerts) && alerts[k].entry == seq; k++ {
				if af.matches(&alerts[k]) {
					raised = append(raised, src.expand(uint64(k)+1, &alerts[k]))
				}
			}
		}
		var match bool
		switch f.Alerts {
		case AlertsOff:
			match = f.matches(e, l.quoteCurrency)
		case AlertsInclude:
			match = len(raised) > 0 || f.matches(e, l.quoteCurrency)
		default:
			match = len(raised) > 0 && f.matches(e, l.quoteCurrency)
		}
		if match {
			if len(page.Entries) == limit {
				// A further match exists: the next page starts after the
				// last entry on this one.
				page.Next = page.Entries[limit-1].Seq
				return page, nil
			}
			page.Entries = append(page.Entries, *e)
			if page.Alerts != nil {
				page.Alerts = append(page.Alerts, raised)
			}
		}
		if scanned++; scanned == budget {
			page.Next = seq
			return page, nil
		}
	}
	return page, nil
}

// seqIter yields increasing journal sequences, then false.
type seqIter func() (uint64, bool)

// baseSeqs yields every sequence after after up to head, or with byAccount
// the distinct sequences of an account's history rows after after.
func (l *Ledger) baseSeqs(rows []row, byAccount bool, after, head uint64) seqIter {
	if !byAccount {
		seq := after
		return func() (uint64, bool) {
			if seq >= head {
				return 0, false
			}
			seq++
			return seq, true
		}
	}
	i := sort.Search(len(rows), func(i int) bool { return rows[i].seq > after })
	var last uint64
	return func() (uint64, bool) {
		for ; i < len(rows); i++ {
			if seq := rows[i].seq; seq != last {
				last = seq
				i++
				return seq, true
			}
		}
		return 0, false
	}
}

// alertSeqs yields the distinct entries, from alerts[from:], that raised an
// alert f matches. After alertYield alerts in a row that f does not match it
// yields the entry it has reached as well, so the caller's scan budget also
// bounds a walk through other monitors' alerts; examining that entry is
// harmless, since the caller decides for itself whether it matches.
func alertSeqs(alerts []alertRec, from int, f *AlertFilter) seqIter {
	i := from
	var last uint64
	return func() (uint64, bool) {
		missed := 0
		for ; i < len(alerts); i++ {
			a := &alerts[i]
			if a.entry == last {
				continue
			}
			if missed++; f.matches(a) || missed > alertYield {
				last = a.entry
				i++
				return a.entry, true
			}
		}
		return 0, false
	}
}

// mergeSeqs yields the sequences either iterator yields, once each, in
// increasing order.
func mergeSeqs(a, b seqIter) seqIter {
	av, aok := a()
	bv, bok := b()
	return func() (uint64, bool) {
		switch {
		case !aok && !bok:
			return 0, false
		case aok && (!bok || av < bv):
			v := av
			av, aok = a()
			return v, true
		case bok && (!aok || bv < av):
			v := bv
			bv, bok = b()
			return v, true
		}
		v := av
		av, aok = a()
		bv, bok = b()
		return v, true
	}
}

// EntriesGrown returns a channel that is closed once a commit after this
// call appends journal entries. A live consumer takes it before reading a
// page and waits on it when the page reaches the head, so an entry
// committed between the read and the wait still wakes it.
func (l *Ledger) EntriesGrown() <-chan struct{} {
	l.growMu.Lock()
	defer l.growMu.Unlock()
	if l.grown == nil {
		l.grown = make(chan struct{})
	}
	return l.grown
}

// wakeEntryWaiters closes the channel EntriesGrown handed out. The writer
// calls it after publishing a commit that appended entries.
func (l *Ledger) wakeEntryWaiters() {
	l.growMu.Lock()
	if l.grown != nil {
		close(l.grown)
		l.grown = nil
	}
	l.growMu.Unlock()
}

func (l *Ledger) feedStep(seq uint64) {
	if l.feedHook != nil {
		l.feedHook(seq)
	}
}

// ParseEntryKinds reads a comma-separated list of entry kinds.
func ParseEntryKinds(s string) ([]EntryKind, error) {
	parts := strings.Split(s, ",")
	if len(parts) > MaxFeedKinds {
		return nil, errf(CodeInvalid, "at most %d kinds", MaxFeedKinds)
	}
	kinds := make([]EntryKind, 0, len(parts))
	for _, p := range parts {
		k := EntryKind(p)
		if !k.valid() {
			return nil, errf(CodeInvalid, "kind %q is not one of posted, pending, post_pending, void_pending, expire_pending, "+
				"reversal, increase_pending or extend_pending", p)
		}
		for _, have := range kinds {
			if have == k {
				return nil, errf(CodeInvalid, "kind %s is listed twice", k)
			}
		}
		kinds = append(kinds, k)
	}
	return kinds, nil
}

// Journal is the journal as of one moment: every entry up to the head when
// it was taken. Taking it costs O(1) under the read lock, and reading it
// takes no lock at all, because the journal is append-only and an entry
// never changes once committed. An export walks the whole journal through
// one, so it never delays a commit, however long its client takes to read.
type Journal struct {
	entries []Entry
	step    func(seq uint64)
}

// Journal returns the journal as of now.
func (l *Ledger) Journal() Journal {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return Journal{entries: l.st.entries, step: l.feedStep}
}

// Head is the sequence of the newest entry in the journal, 0 when empty.
func (j Journal) Head() uint64 { return uint64(len(j.entries)) }

// Each calls fn for every entry whose time is in [from, to), oldest first,
// and stops at the first error fn returns. A zero from or to leaves that
// end open. Entries are in sequence order, not time order: the clock that
// stamps them may step back, so every entry is examined. fn must not
// modify the entry or keep it after it returns.
func (j Journal) Each(from, to time.Time, fn func(e *Entry) error) error {
	for i := range j.entries {
		e := &j.entries[i]
		if j.step != nil {
			j.step(e.Seq)
		}
		if !from.IsZero() && e.Time.Before(from) || !to.IsZero() && !e.Time.Before(to) {
			continue
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}
