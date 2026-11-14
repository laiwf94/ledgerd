package ledger

import (
	"sort"
	"strings"
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
		return validateCurrency(f.Currency)
	}
	return nil
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

// Entries returns up to limit journal entries matching f with sequence
// numbers after after, oldest first.
func (l *Ledger) Entries(f EntryFilter, after uint64, limit int) (EntryPage, error) {
	if limit <= 0 || limit > MaxListLimit {
		return EntryPage{}, errf(CodeInvalid, "limit must be between 1 and %d", MaxListLimit)
	}
	if err := f.validate(); err != nil {
		return EntryPage{}, err
	}
	// Note the journal and, for an account, its history rows. Both are
	// append-only, so the prefixes stay valid after the lock is released.
	l.mu.RLock()
	journal := l.st.entries
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
	if f.Account != "" && !known {
		// No entry so far touches an account that does not exist.
		return page, nil
	}

	budget := feedScanBudget(limit)
	scanned := 0
	// examine reports whether the walk goes on after the entry at seq.
	examine := func(seq uint64) bool {
		l.feedStep(seq)
		e := &journal[seq-1]
		if f.matches(e, l.quoteCurrency) {
			if len(page.Entries) == limit {
				// A further match exists: the next page starts after the
				// last entry on this one.
				page.Next = page.Entries[limit-1].Seq
				return false
			}
			page.Entries = append(page.Entries, *e)
		}
		if scanned++; scanned == budget {
			page.Next = seq
			return false
		}
		return true
	}
	if f.Account != "" {
		// Walk the account's own rows: one per line, so an entry with two
		// lines on the account (none today) is examined once.
		i := sort.Search(len(rows), func(i int) bool { return rows[i].seq > after })
		var last uint64
		for ; i < len(rows); i++ {
			seq := rows[i].seq
			if seq == last {
				continue
			}
			last = seq
			if !examine(seq) {
				return page, nil
			}
		}
		return page, nil
	}
	for seq := after + 1; seq <= head; seq++ {
		if !examine(seq) {
			return page, nil
		}
	}
	return page, nil
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
