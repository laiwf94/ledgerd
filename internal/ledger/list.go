package ledger

import (
	"fmt"
	"slices"
)

// Listings page through the secondary indexes (index.go) under the read lock.
//
// Accounts are listed in ID order and a page resumes after the last ID the
// previous page examined. Accounts are never deleted and their ID, currency
// and metadata never change, so walking the pages while other clients write
// returns every matching account that existed when the walk began exactly
// once, in order; an account created during the walk appears if and only if
// its ID sorts after the position the walk had reached when it was created.
// Each page is read under one lock, so every balance on it is as of the same
// journal entry (AsOfSeq).
//
// A request scans at most listScanBudget(limit) candidates, so a filter that
// matches rarely cannot hold the read lock for long. When the budget runs out
// the page may be short, or even empty, and still carry a Next position; the
// caller keeps paging until Next is empty.

// Listing limits.
const (
	MaxListLimit = 1000
	// MaxListMetadata is the most metadata pairs one listing can filter on.
	MaxListMetadata = 8
	listScanFactor  = 16
	minListScan     = 4096
)

// listScanBudget is how many candidates one page may examine.
var listScanBudget = func(limit int) int { return max(limit*listScanFactor, minListScan) }

// AccountFilter selects accounts. Empty fields match everything; an account
// must match every field given.
type AccountFilter struct {
	Currency string
	// Metadata pairs must all be present with exactly these values.
	Metadata map[string]string
}

func (f *AccountFilter) validate() *Error {
	if f.Currency != "" {
		if err := validateCurrency(f.Currency); err != nil {
			return err
		}
	}
	if len(f.Metadata) > MaxListMetadata {
		return errf(CodeInvalid, "at most %d metadata filters", MaxListMetadata)
	}
	for k, v := range f.Metadata {
		if k == "" || !validText(k, MaxMetadataKey) {
			return errf(CodeInvalid, "metadata filter keys must be 1 to %d characters of printable text", MaxMetadataKey)
		}
		if !validText(v, MaxMetadataValue) {
			return errf(CodeInvalid, "metadata filter value for %q must be at most %d characters of printable text", k, MaxMetadataValue)
		}
	}
	return nil
}

func (f *AccountFilter) matches(a *Account) bool {
	if f.Currency != "" && a.Currency != f.Currency {
		return false
	}
	for k, v := range f.Metadata {
		if got, ok := a.Metadata[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// AccountPage is one page of a listing.
type AccountPage struct {
	Accounts []AccountState
	// Next is the position to pass as after for the following page, or ""
	// when the listing is complete.
	Next string
	// AsOfSeq is the journal entry every balance on the page reflects.
	AsOfSeq uint64
}

// ListAccounts returns up to limit accounts matching f with IDs after after
// ("" starts at the beginning), in ID order, each with its current balance.
func (l *Ledger) ListAccounts(f AccountFilter, after string, limit int) (AccountPage, error) {
	if limit <= 0 || limit > MaxListLimit {
		return AccountPage{}, errf(CodeInvalid, "limit must be between 1 and %d", MaxListLimit)
	}
	if after != "" && !ValidID(after) {
		return AccountPage{}, errf(CodeInvalid, "%q is not a valid account id", after)
	}
	if err := f.validate(); err != nil {
		return AccountPage{}, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	st := l.st
	head := st.head()
	page := AccountPage{AsOfSeq: head}

	// Walk the smallest set that every match must be in and check the other
	// filters on each account. A filter value no account has means no match.
	src := &st.ix.accounts
	if f.Currency != "" {
		s := st.ix.acctCurrency[f.Currency]
		if s == nil {
			return page, nil
		}
		src = s
	}
	for k, v := range f.Metadata {
		s := st.ix.acctMeta[metaPair{k, v}]
		if s == nil {
			return page, nil
		}
		if s.Len() < src.Len() {
			src = s
		}
	}

	budget := listScanBudget(limit)
	scanned := 0
	for id := range src.ascend(after, after == "") {
		a := st.accounts[id]
		if f.matches(&a.Account) {
			if len(page.Accounts) == limit {
				// A further match exists: the next page starts after the
				// last account on this one.
				page.Next = page.Accounts[limit-1].ID
				return page, nil
			}
			page.Accounts = append(page.Accounts, a.state(head))
		}
		if scanned++; scanned == budget {
			page.Next = id
			return page, nil
		}
	}
	return page, nil
}

// Transfers are listed by the sequence number of the journal entry that
// created them, oldest first or, with Desc, newest first. A page resumes
// from the last sequence the previous page examined. Transfers are never
// deleted and only their state changes after they are created, so a walk
// returns each transfer at most once and in order, and returns every
// transfer that existed when the walk began and matched the filter for the
// whole walk. A transfer whose state changes during the walk is listed if it
// matched when its page was read. Oldest-first walks also reach transfers
// created after they began; newest-first walks never do. Every transfer on a
// page is as of the same journal entry (AsOfSeq).
//
// A reference filter is how clients find the transfers that carry an
// external reference: the walk reads the reference's own index set, so it
// costs the number of transfers with that reference, not a statement scan.

// TransferFilter selects transfers. Empty fields match everything; a
// transfer must match every field given.
type TransferFilter struct {
	// Account matches transfers with a posting on that account.
	Account   string
	State     TransferState
	Currency  string
	Reference string
	// Metadata pairs must all be present with exactly these values.
	Metadata map[string]string
}

func (s TransferState) valid() bool {
	switch s {
	case StatePending, StatePosted, StateVoided, StateExpired:
		return true
	}
	return false
}

func (f *TransferFilter) validate() *Error {
	if f.Account != "" && !ValidID(f.Account) {
		return errf(CodeInvalid, "%q is not a valid account id", f.Account)
	}
	if f.State != "" && !f.State.valid() {
		return errf(CodeInvalid, "state must be one of pending, posted, voided or expired")
	}
	if f.Reference != "" && !validText(f.Reference, MaxReference) {
		return errf(CodeInvalid, "reference must be at most %d characters of printable text", MaxReference)
	}
	af := AccountFilter{Currency: f.Currency, Metadata: f.Metadata}
	return af.validate()
}

func (f *TransferFilter) matches(tr *Transfer) bool {
	if f.State != "" && tr.State != f.State {
		return false
	}
	if f.Currency != "" && tr.Currency != f.Currency {
		return false
	}
	if f.Reference != "" && tr.Reference != f.Reference {
		return false
	}
	if f.Account != "" && !slices.ContainsFunc(tr.Postings, func(p Posting) bool { return p.Account == f.Account }) {
		return false
	}
	for k, v := range f.Metadata {
		if got, ok := tr.Metadata[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// source returns the smallest index set every match of f must be in, or
// nil when a filter value no transfer has rules out every match. The caller
// holds the read lock.
func (f *TransferFilter) source(ix *indexes) *ordset[uint64] {
	src := &ix.transfers
	narrow := func(s *ordset[uint64]) bool {
		if s == nil {
			return false
		}
		if s.Len() < src.Len() {
			src = s
		}
		return true
	}
	if f.State != "" && !narrow(ix.xferState[f.State]) ||
		f.Currency != "" && !narrow(ix.xferCurrency[f.Currency]) ||
		f.Reference != "" && !narrow(ix.xferRef[f.Reference]) ||
		f.Account != "" && !narrow(ix.xferAccount[f.Account]) {
		return nil
	}
	for k, v := range f.Metadata {
		if !narrow(ix.xferMeta[metaPair{k, v}]) {
			return nil
		}
	}
	return src
}

// TransferPage is one page of a transfer listing.
type TransferPage struct {
	// Transfers must not be modified.
	Transfers []*Transfer
	// Next is the position to pass as from for the following page, or 0
	// when the listing is complete.
	Next uint64
	// AsOfSeq is the journal entry every transfer on the page reflects.
	AsOfSeq uint64
}

// ListTransfers returns up to limit transfers matching f in the order they
// were created, or newest first when desc is true. from is the position the
// previous page returned as Next (0 starts at the beginning): an oldest-first
// page holds transfers created after it, a newest-first page transfers
// created before it.
func (l *Ledger) ListTransfers(f TransferFilter, from uint64, desc bool, limit int) (TransferPage, error) {
	if limit <= 0 || limit > MaxListLimit {
		return TransferPage{}, errf(CodeInvalid, "limit must be between 1 and %d", MaxListLimit)
	}
	if err := f.validate(); err != nil {
		return TransferPage{}, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	st := l.st
	page := TransferPage{AsOfSeq: st.head()}
	src := f.source(st.ix)
	if src == nil {
		return page, nil
	}
	walk := src.ascend(from, from == 0)
	if desc {
		walk = src.descend(from, from == 0)
	}
	budget := listScanBudget(limit)
	scanned := 0
	for seq := range walk {
		tr, ok := st.transferAt(seq)
		if !ok {
			// The indexes agree with the state under the lock; a member
			// with no transfer is damage CheckIndexes reports.
			panic(fmt.Sprintf("ledger: transfer index holds %d, which creates no transfer", seq))
		}
		if f.matches(tr) {
			if len(page.Transfers) == limit {
				page.Next = createdAt(page.Transfers[limit-1])
				return page, nil
			}
			page.Transfers = append(page.Transfers, tr)
		}
		if scanned++; scanned == budget {
			page.Next = seq
			return page, nil
		}
	}
	return page, nil
}
