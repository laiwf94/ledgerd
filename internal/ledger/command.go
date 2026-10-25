package ledger

import (
	"time"
	"unicode/utf8"

	"github.com/laiwf94/ledgerd/internal/money"
)

// Limits on what one command may contain.
const (
	MaxPostings        = 64
	MaxPreconditions   = 64
	MaxMetadataKeys    = 20
	MaxMetadataKey     = 40
	MaxMetadataValue   = 500
	MaxReference       = 128
	MaxIDLength        = 64
	MaxIdempotencyKey  = 255
	MaxScope           = 64
	DefaultTimeout     = 24 * time.Hour
	MaxTimeout         = 30 * 24 * time.Hour
	defaultTimeoutSecs = int64(DefaultTimeout / time.Second)
	maxTimeoutSecs     = int64(MaxTimeout / time.Second)
)

// AccountSpec opens an account.
type AccountSpec struct {
	ID            string            `json:"id"`
	Currency      string            `json:"currency"`
	NormalBalance Direction         `json:"normal_balance"`
	NonNegative   bool              `json:"non_negative,omitempty"`
	NonPositive   bool              `json:"non_positive,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// PostingSpec is one requested leg of a transfer.
type PostingSpec struct {
	Account   string    `json:"account"`
	Direction Direction `json:"direction"`
	Amount    int64     `json:"amount"`
}

// Precondition makes a transfer conditional on an account's current state,
// for optimistic concurrency: read the account, decide, and transfer only if
// nothing changed in between.
type Precondition struct {
	Account       string  `json:"account"`
	Version       *uint64 `json:"version,omitempty"`
	PostedBalance *int64  `json:"posted_balance,omitempty"`
}

// TransferSpec creates a transfer: a balanced journal entry in one currency.
type TransferSpec struct {
	Currency string        `json:"currency"`
	Postings []PostingSpec `json:"postings"`
	// Pending makes a two-phase transfer: the amounts are reserved until the
	// transfer is posted or voided, or until TimeoutSeconds pass.
	Pending        bool              `json:"pending,omitempty"`
	TimeoutSeconds int64             `json:"timeout_seconds,omitempty"`
	Reference      string            `json:"reference,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Preconditions  []Precondition    `json:"preconditions,omitempty"`
}

// PartialAmount names how much of one posting to post or reverse.
type PartialAmount struct {
	Account string `json:"account"`
	Amount  int64  `json:"amount"`
}

// Amounts selects how much of a transfer to post or reverse. Empty means all
// of it. Amount is a shorthand for two-posting transfers; Postings works for
// any transfer and must balance.
type Amounts struct {
	Amount   int64           `json:"amount,omitempty"`
	Postings []PartialAmount `json:"postings,omitempty"`
}

// PostSpec posts a pending transfer, fully or partially. Whatever is not
// posted is released.
type PostSpec struct {
	TransferID string `json:"transfer_id"`
	Amounts
}

// VoidSpec releases a pending transfer's reservation.
type VoidSpec struct {
	TransferID string `json:"transfer_id"`
}

// ReverseSpec sends back some or all of a posted transfer as a new transfer
// linked to it. History is never edited; corrections are new entries.
type ReverseSpec struct {
	TransferID string `json:"transfer_id"`
	Amounts
	Reference string            `json:"reference,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Command is one state change. Exactly one field is set.
type Command struct {
	CreateAccount  *AccountSpec  `json:"create_account,omitempty"`
	CreateTransfer *TransferSpec `json:"create_transfer,omitempty"`
	Post           *PostSpec     `json:"post,omitempty"`
	Void           *VoidSpec     `json:"void,omitempty"`
	Reverse        *ReverseSpec  `json:"reverse,omitempty"`
}

// Idem identifies a request for idempotency. Scope separates callers (the API
// uses one scope per API key) so two clients can't collide on, or read, each
// other's keys. Hash is a digest of the request; reusing a key for a
// different request is an error.
type Idem struct {
	Scope string
	Key   string
	Hash  [32]byte
}

func (i *Idem) id() string { return i.Scope + "\x00" + i.Key }

func (i *Idem) validate() *Error {
	if len(i.Key) == 0 || len(i.Key) > MaxIdempotencyKey {
		return errf(CodeInvalid, "idempotency key must be 1 to %d characters", MaxIdempotencyKey)
	}
	for j := 0; j < len(i.Key); j++ {
		if i.Key[j] < 0x20 || i.Key[j] > 0x7e {
			return errf(CodeInvalid, "idempotency key must be printable ASCII")
		}
	}
	if len(i.Scope) > MaxScope {
		return errf(CodeInvalid, "idempotency scope is longer than %d characters", MaxScope)
	}
	return nil
}

// ValidID reports whether s is an acceptable account or transfer ID: 1 to 64
// characters of letters, digits and _ . : - starting with a letter or digit.
func ValidID(s string) bool {
	if len(s) == 0 || len(s) > MaxIDLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case i > 0 && (c == '_' || c == '.' || c == ':' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// validText accepts UTF-8 without control characters, up to max bytes.
func validText(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return false
		}
	}
	return true
}

func validateMetadata(m map[string]string) *Error {
	if len(m) > MaxMetadataKeys {
		return errf(CodeInvalid, "metadata has more than %d keys", MaxMetadataKeys)
	}
	for k, v := range m {
		if k == "" || !validText(k, MaxMetadataKey) {
			return errf(CodeInvalid, "metadata keys must be 1 to %d characters of printable text", MaxMetadataKey)
		}
		if !validText(v, MaxMetadataValue) {
			return errf(CodeInvalid, "metadata value for %q must be at most %d characters of printable text", k, MaxMetadataValue)
		}
	}
	return nil
}

func validateCurrency(code string) *Error {
	if _, ok := money.Lookup(code); !ok {
		return errf(CodeInvalid, "currency %q is not an ISO 4217 code with a minor unit", code)
	}
	return nil
}

// Validate checks everything about a command that does not depend on ledger
// state, and fills in defaults. Commands that fail here are rejected before
// they reach the journal, and are not recorded for idempotency.
func (c *Command) Validate() *Error {
	n := 0
	for _, set := range []bool{c.CreateAccount != nil, c.CreateTransfer != nil, c.Post != nil, c.Void != nil, c.Reverse != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errf(CodeInvalid, "a command must set exactly one operation")
	}
	switch {
	case c.CreateAccount != nil:
		return c.CreateAccount.validate()
	case c.CreateTransfer != nil:
		return c.CreateTransfer.validate()
	case c.Post != nil:
		if !ValidID(c.Post.TransferID) {
			return errf(CodeInvalid, "transfer_id is not valid")
		}
		return c.Post.Amounts.validate()
	case c.Void != nil:
		if !ValidID(c.Void.TransferID) {
			return errf(CodeInvalid, "transfer_id is not valid")
		}
		return nil
	default:
		r := c.Reverse
		if !ValidID(r.TransferID) {
			return errf(CodeInvalid, "transfer_id is not valid")
		}
		if !validText(r.Reference, MaxReference) {
			return errf(CodeInvalid, "reference must be at most %d characters of printable text", MaxReference)
		}
		if err := validateMetadata(r.Metadata); err != nil {
			return err
		}
		return r.Amounts.validate()
	}
}

func (s *AccountSpec) validate() *Error {
	if !ValidID(s.ID) {
		return errf(CodeInvalid, "account id must be 1 to %d characters of letters, digits and _ . : -", MaxIDLength)
	}
	if err := validateCurrency(s.Currency); err != nil {
		return err
	}
	if !s.NormalBalance.valid() {
		return errf(CodeInvalid, "normal_balance must be debit or credit")
	}
	if s.NonNegative && s.NonPositive {
		return errf(CodeInvalid, "an account can't be both non_negative and non_positive")
	}
	return validateMetadata(s.Metadata)
}

func (s *TransferSpec) validate() *Error {
	if err := validateCurrency(s.Currency); err != nil {
		return err
	}
	if len(s.Postings) < 2 || len(s.Postings) > MaxPostings {
		return errf(CodeInvalid, "a transfer needs between 2 and %d postings", MaxPostings)
	}
	seen := make(map[string]bool, len(s.Postings))
	var debits, credits int64
	for i, p := range s.Postings {
		if !ValidID(p.Account) {
			return errf(CodeInvalid, "postings[%d].account is not a valid account id", i)
		}
		if seen[p.Account] {
			return errf(CodeInvalid, "account %s appears in more than one posting", p.Account)
		}
		seen[p.Account] = true
		if !p.Direction.valid() {
			return errf(CodeInvalid, "postings[%d].direction must be debit or credit", i)
		}
		if p.Amount <= 0 {
			return errf(CodeInvalid, "postings[%d].amount must be a positive number of minor units", i)
		}
		var ok bool
		if p.Direction == Debit {
			debits, ok = money.Add(debits, p.Amount)
		} else {
			credits, ok = money.Add(credits, p.Amount)
		}
		if !ok {
			return errf(CodeInvalid, "postings add up to more than the largest amount")
		}
	}
	if debits != credits {
		return errf(CodeInvalid, "debits (%d) must equal credits (%d)", debits, credits)
	}
	if s.Pending {
		if s.TimeoutSeconds == 0 {
			s.TimeoutSeconds = defaultTimeoutSecs
		}
		if s.TimeoutSeconds < 1 || s.TimeoutSeconds > maxTimeoutSecs {
			return errf(CodeInvalid, "timeout_seconds must be between 1 and %d", maxTimeoutSecs)
		}
	} else if s.TimeoutSeconds != 0 {
		return errf(CodeInvalid, "timeout_seconds only applies to pending transfers")
	}
	if !validText(s.Reference, MaxReference) {
		return errf(CodeInvalid, "reference must be at most %d characters of printable text", MaxReference)
	}
	if err := validateMetadata(s.Metadata); err != nil {
		return err
	}
	if len(s.Preconditions) > MaxPreconditions {
		return errf(CodeInvalid, "at most %d preconditions", MaxPreconditions)
	}
	pre := make(map[string]bool, len(s.Preconditions))
	for i, p := range s.Preconditions {
		if !ValidID(p.Account) {
			return errf(CodeInvalid, "preconditions[%d].account is not a valid account id", i)
		}
		if pre[p.Account] {
			return errf(CodeInvalid, "account %s appears in more than one precondition", p.Account)
		}
		pre[p.Account] = true
		if p.Version == nil && p.PostedBalance == nil {
			return errf(CodeInvalid, "preconditions[%d] must set version or posted_balance", i)
		}
	}
	return nil
}

func (a *Amounts) validate() *Error {
	if a.Amount < 0 {
		return errf(CodeInvalid, "amount must be positive")
	}
	if a.Amount > 0 && len(a.Postings) > 0 {
		return errf(CodeInvalid, "give amount or postings, not both")
	}
	if len(a.Postings) > MaxPostings {
		return errf(CodeInvalid, "at most %d postings", MaxPostings)
	}
	seen := make(map[string]bool, len(a.Postings))
	for i, p := range a.Postings {
		if !ValidID(p.Account) {
			return errf(CodeInvalid, "postings[%d].account is not a valid account id", i)
		}
		if seen[p.Account] {
			return errf(CodeInvalid, "account %s appears more than once", p.Account)
		}
		seen[p.Account] = true
		if p.Amount <= 0 {
			return errf(CodeInvalid, "postings[%d].amount must be positive", i)
		}
	}
	return nil
}
