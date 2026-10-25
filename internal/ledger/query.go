package ledger

import (
	"sort"
	"time"
)

// Read limits.
const (
	MaxStatementLimit = 1000
	MaxMovements      = 100_000
)

// Head returns the sequence number of the newest journal entry.
func (l *Ledger) Head() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.st.head()
}

// Account returns an account with its current balance, or with its balance
// as of journal entry asOf when asOf is not zero.
func (l *Ledger) Account(id string, asOf uint64) (AccountState, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	a, ok := l.st.accounts[id]
	if !ok {
		return AccountState{}, errf(CodeNotFound, "account %s does not exist", id)
	}
	head := l.st.head()
	if asOf == 0 || asOf == head {
		return a.state(head), nil
	}
	if asOf > head {
		return AccountState{}, errf(CodeInvalid, "as_of_seq %d is after the newest entry %d", asOf, head)
	}
	n := sort.Search(len(a.rows), func(i int) bool { return a.rows[i].seq > asOf })
	st := AccountState{Account: a.Account, Version: uint64(n), AsOfSeq: asOf}
	var b Balance
	if n > 0 {
		b = a.rows[n-1].balance
	}
	st.Balance = b.View(a.NormalBalance)
	return st, nil
}

// Transfer returns a transfer by ID. The result must not be modified.
func (l *Ledger) Transfer(id string) (*Transfer, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	tr, ok := l.st.transfers[id]
	if !ok {
		return nil, errf(CodeNotFound, "transfer %s does not exist", id)
	}
	return tr, nil
}

// Statement returns up to limit statement lines for an account with sequence
// numbers after afterSeq, oldest first, each with the running balance after
// it. next is the cursor for the following page, or 0 at the end.
func (l *Ledger) Statement(id string, afterSeq uint64, limit int) (lines []StatementLine, next uint64, err error) {
	if limit <= 0 || limit > MaxStatementLimit {
		return nil, 0, errf(CodeInvalid, "limit must be between 1 and %d", MaxStatementLimit)
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	a, ok := l.st.accounts[id]
	if !ok {
		return nil, 0, errf(CodeNotFound, "account %s does not exist", id)
	}
	start := sort.Search(len(a.rows), func(i int) bool { return a.rows[i].seq > afterSeq })
	end := min(start+limit, len(a.rows))
	lines = make([]StatementLine, 0, end-start)
	for _, r := range a.rows[start:end] {
		e := &l.st.entries[r.seq-1]
		ln := e.Lines[r.line]
		sign := int64(1)
		if ln.Direction != a.NormalBalance {
			sign = -1
		}
		lines = append(lines, StatementLine{Seq: e.Seq, Time: e.Time, Kind: e.Kind, TransferID: e.TransferID,
			Direction: ln.Direction, PostedChange: sign * ln.Posted, PendingChange: sign * ln.Pending,
			Balance: r.balance.View(a.NormalBalance)})
	}
	if end < len(a.rows) {
		next = a.rows[end-1].seq
	}
	return lines, next, nil
}

// Movements returns the posted changes to an account with entry times in
// [from, to), for reconciliation. Pending reservations are not movements:
// money has not moved until they post.
func (l *Ledger) Movements(id string, from, to time.Time) (Account, []Movement, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	a, ok := l.st.accounts[id]
	if !ok {
		return Account{}, nil, errf(CodeNotFound, "account %s does not exist", id)
	}
	var out []Movement
	for _, r := range a.rows {
		e := &l.st.entries[r.seq-1]
		ln := e.Lines[r.line]
		if ln.Posted == 0 || e.Time.Before(from) || !e.Time.Before(to) {
			continue
		}
		if len(out) == MaxMovements {
			return Account{}, nil, errf(CodeInvalid, "more than %d ledger movements in the period; reconcile a shorter period", MaxMovements)
		}
		amt := ln.Posted
		if ln.Direction != a.NormalBalance {
			amt = -amt
		}
		out = append(out, Movement{Seq: e.Seq, Time: e.Time, TransferID: e.TransferID,
			Reference: l.st.transfers[e.TransferID].Reference, Amount: amt})
	}
	return a.Account, out, nil
}

// Stats describes the ledger's size.
type Stats struct {
	Head            uint64 `json:"head"`
	Accounts        int    `json:"accounts"`
	Transfers       int    `json:"transfers"`
	OpenPending     int    `json:"open_pending"`
	IdempotencyKeys int    `json:"idempotency_keys"`
	LogBytes        int64  `json:"log_bytes"`
	LogSyncs        uint64 `json:"log_syncs"`
}

// Stats returns current counts.
func (l *Ledger) Stats() Stats {
	l.mu.RLock()
	s := Stats{Head: l.st.head(), Accounts: len(l.st.accounts), Transfers: len(l.st.transfers), OpenPending: l.st.open}
	l.mu.RUnlock()
	l.idemMu.Lock()
	s.IdempotencyKeys = len(l.idem)
	l.idemMu.Unlock()
	if l.log != nil {
		s.LogBytes, s.LogSyncs = l.log.Size(), l.log.Syncs()
	}
	return s
}
