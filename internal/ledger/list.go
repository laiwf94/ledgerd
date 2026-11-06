package ledger

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
