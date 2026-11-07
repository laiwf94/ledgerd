package ledger

import (
	"sort"
	"time"
)

// Account life cycle. An account opens as open. Freezing stops money moving
// through it: no new transfer, posting of a reservation or reversal may touch
// it, while reservations it already has can still be voided or expire, so
// freezing never strands money that belongs to someone else. Unfreezing
// restores it. Closing needs a zero balance and no open reservations, and is
// final: nothing touches a closed account again.
//
// Each change is a logged command like any other. The account keeps every
// change it went through, with the journal head it happened at, so a
// point-in-time read reports the status as of that sequence and the
// invariant checker can prove that no entry ever moved money through a frozen
// or closed account.

// statusChange is one change of an account's status. Seq is the journal head
// when it happened: entries up to Seq came before it, entries after came
// under the new status.
type statusChange struct {
	seq    uint64
	at     time.Time
	status AccountStatus
	reason string
}

// transitionAllowed reports whether an account may go from one status to
// another. It is shared by the commands, the snapshot reader and the
// invariant checker, so all three agree on the rules.
func transitionAllowed(from, to AccountStatus) bool {
	switch to {
	case StatusFrozen:
		return from == StatusOpen
	case StatusOpen:
		return from == StatusFrozen
	case StatusClosed:
		return from == StatusOpen || from == StatusFrozen
	}
	return false
}

func validStatus(s AccountStatus) bool {
	return s == StatusOpen || s == StatusFrozen || s == StatusClosed
}

// status returns the account's current status.
func (a *account) status() AccountStatus {
	if n := len(a.life); n > 0 {
		return a.life[n-1].status
	}
	return StatusOpen
}

// statusAt returns the change in effect after journal entry seq, or nil if
// the account was still open then as it was created.
func statusAt(life []statusChange, seq uint64) *statusChange {
	n := sort.Search(len(life), func(i int) bool { return life[i].seq > seq })
	if n == 0 {
		return nil
	}
	return &life[n-1]
}

// setStatus fills the status fields of an account state from a change.
func (s *AccountState) setStatus(c *statusChange) {
	if c == nil {
		s.Status = StatusOpen
		return
	}
	s.Status, s.StatusReason, s.StatusChangedAt = c.status, c.reason, c.at
}

// usable refuses an entry of the given kind on an account that is not open.
// Releasing a reservation (void or expiry) is allowed on a frozen account; a
// closed account has no reservations to release.
func (a *account) usable(kind EntryKind) *Error {
	switch a.status() {
	case StatusFrozen:
		if kind == KindVoidPending || kind == KindExpirePending {
			return nil
		}
		return errf(CodeAccountFrozen, "account %s is frozen", a.ID)
	case StatusClosed:
		return errf(CodeAccountClosed, "account %s is closed", a.ID)
	}
	return nil
}

// changeStatus applies a freeze, unfreeze or close.
func (t *txn) changeStatus(s *StatusSpec, to AccountStatus) (Outcome, *Error) {
	a, ok := t.account(s.AccountID)
	if !ok {
		return Outcome{}, errf(CodeNotFound, "account %s does not exist", s.AccountID)
	}
	from := a.status()
	switch {
	case from == StatusClosed:
		return Outcome{}, errf(CodeAccountClosed, "account %s is closed", a.ID)
	case !transitionAllowed(from, to): // the only refusal left is a change to the status it has
		return Outcome{}, errf(CodeInvalidState, "account %s is already %s", a.ID, to)
	}
	if to == StatusClosed {
		v := a.bal.View(a.NormalBalance)
		if a.bal.DebitsPending != 0 || a.bal.CreditsPending != 0 {
			return Outcome{}, errf(CodePrecondition, "account %s has open reservations (%d debit, %d credit pending); post, void or let them expire before closing",
				a.ID, a.bal.DebitsPending, a.bal.CreditsPending)
		}
		if v.Posted != 0 {
			return Outcome{}, errf(CodePrecondition, "account %s has a posted balance of %d; move it out before closing", a.ID, v.Posted)
		}
	}
	head := t.st.head() + uint64(len(t.entries))
	// The full slice expression makes append copy, so readers holding the
	// committed history never see it change.
	a.life = append(a.life[:len(a.life):len(a.life)], statusChange{seq: head, at: t.now, status: to, reason: s.Reason})
	st := a.state(head)
	return Outcome{Account: &st}, nil
}
