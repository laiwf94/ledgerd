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
	CodeAccountFrozen    Code = "account_frozen"
	CodeAccountClosed    Code = "account_closed"
)

// Error is a ledger error with a stable code. Errors returned while executing
// a command are part of its result: retrying with the same idempotency key
// returns the same Error.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	// Item is the index of the transfer that failed when the command was a
	// batch, and nil otherwise.
	Item *int `json:"item,omitempty"`
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
	NonNegative bool `json:"non_negative"`
	NonPositive bool `json:"non_positive"`
	// Exchange marks a currency exchange account: the ledger's position in
	// one currency on the other side of conversions. Every multi-currency
	// transfer converts through exchange accounts, one or more in each of
	// its currencies, and its rate is checked against what they move.
	Exchange  bool              `json:"exchange,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
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

// AccountStatus is where an account is in its life cycle.
type AccountStatus string

// Account statuses. An account opens as open. Freezing stops money moving
// through it until it is unfrozen; closing is final.
const (
	StatusOpen   AccountStatus = "open"
	StatusFrozen AccountStatus = "frozen"
	StatusClosed AccountStatus = "closed"
)

// AccountState is an account with its balance at some point in the journal.
type AccountState struct {
	Account
	// Version counts the journal entries that touched the account. It is the
	// value to pass as an optimistic-concurrency precondition.
	Version uint64      `json:"version"`
	Balance BalanceView `json:"balance"`
	// Status is the account's status as of AsOfSeq. StatusReason and
	// StatusChangedAt describe the change that set it; both are empty while
	// the account has never left open.
	Status          AccountStatus `json:"status"`
	StatusReason    string        `json:"status_reason,omitempty"`
	StatusChangedAt time.Time     `json:"status_changed_at,omitzero"`
	// Limits are the balance limits in force as of AsOfSeq.
	Limits Limits `json:"limits"`
	// AsOfSeq is the journal sequence the balance reflects.
	AsOfSeq uint64 `json:"as_of_seq"`
}

// LimitKind names one of an account's two balance limits.
type LimitKind string

// Limit kinds. See limits.go for what each bounds.
const (
	LimitOverdraft LimitKind = "overdraft"
	LimitCredit    LimitKind = "credit"
)

// Limits are an account's balance limits at some point. A nil limit is not
// set.
type Limits struct {
	// Overdraft is how far below zero the available balance may go.
	Overdraft *int64 `json:"overdraft"`
	// Credit is how high the balance including incoming pending may go.
	Credit *int64 `json:"credit"`
	// EffectiveSeq is the first journal entry governed by the newest
	// set_limit change, or 0 while the limits are those the account was
	// opened with.
	EffectiveSeq uint64 `json:"effective_seq,omitempty"`
}

// LimitChange is one set_limit change in an account's history.
type LimitChange struct {
	Kind LimitKind `json:"kind"`
	// Amount is the new limit, or nil when the change removed it.
	Amount *int64 `json:"amount"`
	// EffectiveSeq is the first journal entry the change governs.
	EffectiveSeq uint64    `json:"effective_seq"`
	Time         time.Time `json:"time"`
	Reason       string    `json:"reason,omitempty"`
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
	// KindIncreasePending adds to an open reservation: its lines reserve
	// more on each posting it grows.
	KindIncreasePending EntryKind = "increase_pending"
	// KindExtendPending moves an open reservation's timeout later. It moves
	// no money, so it has no lines; ExpiresAt is the new timeout.
	KindExtendPending EntryKind = "extend_pending"
)

// Posting is one leg of a transfer.
type Posting struct {
	Account   string    `json:"account"`
	Direction Direction `json:"direction"`
	// Currency is set when the posting is in another currency than the
	// transfer, which only a multi-currency transfer has.
	Currency string `json:"currency,omitempty"`
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
	Reverses  string   `json:"reverses,omitempty"`
	Reversals []string `json:"reversals,omitempty"`
	// Balancing is set when the transfer was created with balance_debit or
	// balance_credit, and says how much was asked for and how much moved.
	Balancing *Balancing `json:"balancing,omitempty"`
	// FX is set on a multi-currency transfer: the rate its conversion used,
	// where the rate came from and when it was quoted.
	FX      *FX        `json:"fx,omitempty"`
	Entries []EntryRef `json:"entries"`
}

// FX is the exchange rate a multi-currency transfer converts at. Base is the
// transfer's currency and Quote its other one; Rate is how many major units
// of Quote one major unit of Base buys, as the decimal the client sent.
type FX struct {
	Base     string    `json:"base"`
	Quote    string    `json:"quote"`
	Rate     string    `json:"rate"`
	Source   string    `json:"source"`
	QuotedAt time.Time `json:"quoted_at"`
}

// currencies returns the currencies a transfer's postings are in: its own
// first, then the other one of a multi-currency transfer.
func (t *Transfer) currencies() []string {
	if t.FX != nil {
		return []string{t.Currency, t.FX.Quote}
	}
	return []string{t.Currency}
}

// currency returns the currency of a posting of the transfer.
func (t *Transfer) currency(p *Posting) string {
	if p.Currency != "" {
		return p.Currency
	}
	return t.Currency
}

// Balancing records a balancing transfer: the amount asked for, which sides
// were allowed to clamp it, and the amount that moved, which is what each
// posting's Amount holds.
type Balancing struct {
	Debit     bool  `json:"balance_debit,omitempty"`
	Credit    bool  `json:"balance_credit,omitempty"`
	Requested int64 `json:"requested_amount"`
	Amount    int64 `json:"amount"`
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
	// Currency is set when the line is in another currency than the entry.
	Currency string `json:"currency,omitempty"`
	Posted   int64  `json:"posted"`
	Pending  int64  `json:"pending"`
}

// Entry is an immutable journal entry. Within an entry, debits equal credits
// in each currency, for posted and for pending amounts. An entry of a
// multi-currency transfer has lines in two currencies.
type Entry struct {
	Seq        uint64    `json:"seq"`
	Time       time.Time `json:"time"`
	Kind       EntryKind `json:"kind"`
	TransferID string    `json:"transfer_id"`
	Currency   string    `json:"currency"`
	Lines      []Line    `json:"lines"`
	// ExpiresAt is the new timeout of an extend_pending entry, and zero on
	// every other kind.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// FX is the rate of a multi-currency transfer, on the entry that created
	// it, and nil on every other entry.
	FX *FX `json:"fx,omitempty"`
}

// currency returns the currency of one of the entry's lines.
func (e *Entry) currency(ln *Line) string {
	if ln.Currency != "" {
		return ln.Currency
	}
	return e.Currency
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
	// Transfers are a batch's transfers, in the order it gave them.
	Transfers []*Transfer
	// Replayed is true when the result was returned from the idempotency
	// store instead of executing the command again.
	Replayed bool
	// Expired is the number of reservations released, for ExpireDue.
	Expired int
}
