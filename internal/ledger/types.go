package ledger

import (
	"fmt"
	"time"
)

// Direction is the side of an account a posting lands on.
type Direction string

// Posting directions.
const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

func (d Direction) valid() bool { return d == Debit || d == Credit }

func (d Direction) flip() Direction {
	if d == Debit {
		return Credit
	}
	return Debit
}

// Code is a stable, machine-readable error code.
type Code string

// Error codes. They are part of the API contract.
const (
	CodeInvalid          Code = "invalid_argument"
	CodeNotFound         Code = "not_found"
	CodeAlreadyExists    Code = "already_exists"
	CodeCurrencyMismatch Code = "currency_mismatch"
	CodeLimitExceeded    Code = "limit_exceeded"
	CodeOverflow         Code = "overflow"
	CodeInvalidState     Code = "invalid_state"
	CodeExpired          Code = "transfer_expired"
	CodePrecondition     Code = "failed_precondition"
	CodeKeyReused        Code = "idempotency_key_reused"
	CodeInProgress       Code = "idempotency_in_progress"
	CodeUnavailable      Code = "unavailable"
)

// Error is a ledger error with a stable code. Errors returned while executing
// a command are part of its result: retrying with the same idempotency key
// returns the same Error.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

func errf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Account is an account's fixed definition.
type Account struct {
	ID       string `json:"id"`
	Currency string `json:"currency"`
	// Exponent is the currency's minor-unit exponent, so clients can format
	// amounts: 1234 with exponent 2 is 12.34.
	Exponent      int       `json:"exponent"`
	NormalBalance Direction `json:"normal_balance"`
	// NonNegative and NonPositive bound the balance on the normal side,
	// counting pending amounts against it (TigerBeetle's
	// debits_must_not_exceed_credits and its mirror).
	NonNegative bool              `json:"non_negative"`
	NonPositive bool              `json:"non_positive"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

// Balance holds the four running totals an account keeps. Each only grows,
// except pending totals, which shrink when a reservation is posted, voided or
// expires. The signed balances below are derived from them.
type Balance struct {
	DebitsPosted   int64 `json:"debits_posted"`
	CreditsPosted  int64 `json:"credits_posted"`
	DebitsPending  int64 `json:"debits_pending"`
	CreditsPending int64 `json:"credits_pending"`
}

// sides returns the totals that increase and decrease the balance on the
// account's normal side.
func (b Balance) sides(normal Direction) (incPosted, incPending, decPosted, decPending int64) {
	if normal == Debit {
		return b.DebitsPosted, b.DebitsPending, b.CreditsPosted, b.CreditsPending
	}
	return b.CreditsPosted, b.CreditsPending, b.DebitsPosted, b.DebitsPending
}

// BalanceView is a balance as clients see it, signed so that a positive
// number means "more on the account's normal side".
type BalanceView struct {
	Balance
	// Posted is the settled balance.
	Posted int64 `json:"posted"`
	// Pending is the net amount of open reservations.
	Pending int64 `json:"pending"`
	// Available is what can still be spent: posted, minus reservations that
	// would reduce it. Reservations that would increase it are not counted
	// until they post.
	Available int64 `json:"available"`
}

// View derives the signed balances. Every per-side total, posted plus
// pending, fits in an int64 (the ledger refuses postings that would break
// this), so none of these subtractions can overflow.
func (b Balance) View(normal Direction) BalanceView {
	ip, iq, dp, dq := b.sides(normal)
	return BalanceView{Balance: b, Posted: ip - dp, Pending: iq - dq, Available: ip - (dp + dq)}
}

// AccountState is an account with its balance at some point in the journal.
type AccountState struct {
	Account
	// Version counts the journal entries that touched the account. It is the
	// value to pass as an optimistic-concurrency precondition.
	Version uint64      `json:"version"`
	Balance BalanceView `json:"balance"`
	// AsOfSeq is the journal sequence the balance reflects.
	AsOfSeq uint64 `json:"as_of_seq"`
}

// TransferState is where a transfer is in its life cycle.
type TransferState string

// Transfer states.
const (
	StatePending TransferState = "pending"
	StatePosted  TransferState = "posted"
	StateVoided  TransferState = "voided"
	StateExpired TransferState = "expired"
)

// EntryKind names what a journal entry did.
type EntryKind string

// Journal entry kinds.
const (
	KindPosted        EntryKind = "posted"         // single-phase transfer
	KindPending       EntryKind = "pending"        // reservation created
	KindPostPending   EntryKind = "post_pending"   // reservation released, amount posted
	KindVoidPending   EntryKind = "void_pending"   // reservation released by request
	KindExpirePending EntryKind = "expire_pending" // reservation released by timeout
	KindReversal      EntryKind = "reversal"       // posted amounts sent back
)

// Posting is one leg of a transfer.
type Posting struct {
	Account   string    `json:"account"`
	Direction Direction `json:"direction"`
	// Amount is what was requested: reserved for a pending transfer, posted
	// for a single-phase one.
	Amount int64 `json:"amount"`
	// Posted is what has been posted so far (at most Amount).
	Posted int64 `json:"posted"`
	// Reversed is how much of Posted later reversals sent back.
	Reversed int64 `json:"reversed"`
}

// EntryRef points at a journal entry from the transfer it belongs to.
type EntryRef struct {
	Seq  uint64    `json:"seq"`
	Kind EntryKind `json:"kind"`
}

// Transfer is a business-level movement of money. Its history is the list of
// journal entries that produced its current state; the entries never change.
// A committed *Transfer is never modified: changes replace it with a copy, so
// readers may hold one without a lock.
type Transfer struct {
	ID        string            `json:"id"`
	State     TransferState     `json:"state"`
	Currency  string            `json:"currency"`
	Postings  []Posting         `json:"postings"`
	Reference string            `json:"reference,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	ExpiresAt time.Time         `json:"expires_at,omitzero"`
	// Reverses links a reversal to the transfer it reverses; Reversals links
	// the other way.
	Reverses  string     `json:"reverses,omitempty"`
	Reversals []string   `json:"reversals,omitempty"`
	Entries   []EntryRef `json:"entries"`
}

func (t *Transfer) clone() *Transfer {
	c := *t
	c.Postings = append([]Posting(nil), t.Postings...)
	c.Reversals = append([]string(nil), t.Reversals...)
	c.Entries = append([]EntryRef(nil), t.Entries...)
	return &c
}

// Line is one account's part of a journal entry. Posted is added to the
// posted total on Direction's side; Pending (which may be negative) is added
// to the pending total on that side.
type Line struct {
	Account   string    `json:"account"`
	Direction Direction `json:"direction"`
	Posted    int64     `json:"posted"`
	Pending   int64     `json:"pending"`
}

// Entry is an immutable journal entry. Within an entry, debits equal credits
// for posted and for pending amounts.
type Entry struct {
	Seq        uint64    `json:"seq"`
	Time       time.Time `json:"time"`
	Kind       EntryKind `json:"kind"`
	TransferID string    `json:"transfer_id"`
	Currency   string    `json:"currency"`
	Lines      []Line    `json:"lines"`
}

// row is one entry as seen from one account, with the balance after it.
type row struct {
	seq     uint64
	line    int // index into the entry's lines
	balance Balance
}

// StatementLine is a row of an account statement. Changes are signed in the
// account's normal-balance sense.
type StatementLine struct {
	Seq           uint64      `json:"seq"`
	Time          time.Time   `json:"time"`
	Kind          EntryKind   `json:"kind"`
	TransferID    string      `json:"transfer_id"`
	Direction     Direction   `json:"direction"`
	PostedChange  int64       `json:"posted_change"`
	PendingChange int64       `json:"pending_change"`
	Balance       BalanceView `json:"balance"`
}

// Movement is a posted change to an account, the unit reconciliation matches
// against external statement lines.
type Movement struct {
	Seq        uint64    `json:"seq"`
	Time       time.Time `json:"time"`
	TransferID string    `json:"transfer_id"`
	Reference  string    `json:"reference,omitempty"`
	// Amount is signed in the account's normal-balance sense.
	Amount int64 `json:"amount"`
}

// Outcome is the result of a command.
type Outcome struct {
	Account  *AccountState
	Transfer *Transfer
	// Replayed is true when the result was returned from the idempotency
	// store instead of executing the command again.
	Replayed bool
	// Expired is the number of reservations released, for ExpireDue.
	Expired int
}
