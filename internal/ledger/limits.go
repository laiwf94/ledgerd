package ledger

import (
	"math"
	"time"
)

// Balance limits. An account can have two, each an amount in minor units or
// none:
//
//   - the overdraft limit O bounds how far below zero the available balance
//     may go: posted, minus reservations that would reduce it, stays at or
//     above -O;
//   - the credit limit C bounds how high the balance may go: posted plus
//     reservations that would increase it stays at or below C.
//
// Both are measured on the account's normal side. An account opened with
// non_negative starts with an overdraft limit of 0 and one opened with
// non_positive a credit limit of 0; any other starts with neither.
//
// A set_limit command changes one of them. Like a status change it is
// logged, keeps no journal entry and does not change the account's version,
// and the account keeps every change with the journal head it happened at.
// A change governs the entries after that head: its effective sequence is
// head+1. Lowering a limit below what the account already uses is allowed,
// as when a credit line is cut; the account is then over its limit and may
// only move back towards it. The rule an entry must pass is the same at
// every point in time: it may not take the balance past the limit in force,
// nor further past it than it already was. So releasing or posting a
// reservation, which never moves a balance away from where its reservation
// already counted it, is never refused.

// noLimit is the stored amount of a limit that is not set.
const noLimit int64 = -1

// limitChange is one change of one of an account's limits. Seq is the
// journal head when it happened; it governs entries after Seq.
type limitChange struct {
	seq    uint64
	at     time.Time
	kind   LimitKind
	amount int64 // noLimit or an amount >= 0
	reason string
}

// bounds are the limits in force at some point, noLimit where none is set.
type bounds struct {
	overdraft int64
	credit    int64
}

func validLimitKind(k LimitKind) bool { return k == LimitOverdraft || k == LimitCredit }

// initialBounds are the limits an account is opened with.
func initialBounds(a *Account) bounds {
	b := bounds{overdraft: noLimit, credit: noLimit}
	if a.NonNegative {
		b.overdraft = 0
	}
	if a.NonPositive {
		b.credit = 0
	}
	return b
}

func (b *bounds) apply(c *limitChange) {
	if c.kind == LimitOverdraft {
		b.overdraft = c.amount
	} else {
		b.credit = c.amount
	}
}

// boundsAt returns the limits in force after journal entry seq: the
// initial limits b with every change made at a head of at most seq.
// Changes are in journal order.
func boundsAt(b bounds, changes []limitChange, seq uint64) bounds {
	for i := range changes {
		if changes[i].seq > seq {
			break
		}
		b.apply(&changes[i])
	}
	return b
}

// current returns the limits in force now.
func (a *account) bounds() bounds {
	return boundsAt(initialBounds(&a.Account), a.limits, math.MaxUint64)
}

// floorOf is the available balance the overdraft limit protects; ceilingOf
// is the balance including incoming reservations the credit limit caps.
func floorOf(b Balance, normal Direction) int64 { return b.View(normal).Available }

func ceilingOf(b Balance, normal Direction) int64 {
	ip, iq, dp, _ := b.sides(normal)
	return (ip + iq) - dp // ip+iq fits in an int64 (see Balance.with)
}

// check refuses a change of balance from old to next that ends past a limit
// and further past it than old was. It is shared by the writer and the
// invariant checker, so both apply the same rule.
func (b bounds) check(id string, normal Direction, old, next Balance) *Error {
	if b.overdraft != noLimit {
		before, after := floorOf(old, normal), floorOf(next, normal)
		if after < -b.overdraft && after < before {
			if b.overdraft == 0 {
				return errf(CodeLimitExceeded, "account %s may not go negative: available balance would be %d", id, after)
			}
			return errf(CodeLimitExceeded, "account %s has an overdraft limit of %d: available balance would be %d", id, b.overdraft, after)
		}
	}
	if b.credit != noLimit {
		before, after := ceilingOf(old, normal), ceilingOf(next, normal)
		if after > b.credit && after > before {
			if b.credit == 0 {
				return errf(CodeLimitExceeded, "account %s may not go positive: balance including pending would be %d", id, after)
			}
			return errf(CodeLimitExceeded, "account %s has a credit limit of %d: balance including pending would be %d", id, b.credit, after)
		}
	}
	return nil
}

func limitPtr(v int64) *int64 {
	if v == noLimit {
		return nil
	}
	return &v
}

// limitsView describes the limits in force after journal entry seq.
func limitsView(a *Account, changes []limitChange, seq uint64) Limits {
	b := boundsAt(initialBounds(a), changes, seq)
	l := Limits{Overdraft: limitPtr(b.overdraft), Credit: limitPtr(b.credit)}
	for i := len(changes) - 1; i >= 0; i-- {
		if changes[i].seq <= seq {
			l.EffectiveSeq = changes[i].seq + 1
			break
		}
	}
	return l
}

func (c *limitChange) public() LimitChange {
	return LimitChange{Kind: c.kind, Amount: limitPtr(c.amount), EffectiveSeq: c.seq + 1, Time: c.at, Reason: c.reason}
}

// limitHistoryProblem checks a stored history of limit changes: known kinds,
// amounts that are a limit or none, and sequences in journal order up to
// head. It is shared by the snapshot reader and the invariant checker.
func limitHistoryProblem(changes []limitChange, head uint64) bool {
	var seq uint64
	for _, c := range changes {
		if !validLimitKind(c.kind) || c.amount < noLimit || c.seq < seq || c.seq > head {
			return true
		}
		seq = c.seq
	}
	return false
}

// setLimit applies a set_limit command.
func (t *txn) setLimit(s *LimitSpec) (Outcome, *Error) {
	a, ok := t.account(s.AccountID)
	if !ok {
		return Outcome{}, errf(CodeNotFound, "account %s does not exist", s.AccountID)
	}
	if a.status() == StatusClosed {
		return Outcome{}, errf(CodeAccountClosed, "account %s is closed", a.ID)
	}
	amount := noLimit
	if s.Amount != nil {
		amount = *s.Amount
	}
	head := t.st.head() + uint64(len(t.entries))
	// The full slice expression makes append copy, so readers holding the
	// committed history never see it change.
	a.limits = append(a.limits[:len(a.limits):len(a.limits)], limitChange{seq: head, at: t.now, kind: s.Kind, amount: amount, reason: s.Reason})
	st := a.state(head)
	return Outcome{Account: &st}, nil
}

// LimitHistory returns an account's limits now and every change made to
// them, oldest first.
func (l *Ledger) LimitHistory(id string) (Limits, []LimitChange, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	a, ok := l.st.accounts[id]
	if !ok {
		return Limits{}, nil, errf(CodeNotFound, "account %s does not exist", id)
	}
	out := make([]LimitChange, len(a.limits))
	for i := range a.limits {
		out[i] = a.limits[i].public()
	}
	return limitsView(&a.Account, a.limits, l.st.head()), out, nil
}
