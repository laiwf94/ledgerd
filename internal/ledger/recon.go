package ledger

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
)

// Saved reconciliation runs. The API matches a statement against the
// ledger's movements (internal/recon) and, when asked to keep the result,
// logs it with save_reconciliation: the run's period and options, every
// match and every break, as computed. The ledger stores the result rather
// than recomputing it on replay, so a run means what it meant when it was
// saved even after the matching rules improve, and replay, snapshots and
// ledgerd verify see exactly the same runs.
//
// The ledger does not trust a saved run: before accepting one it checks that
// every ledger posting it names is a posted movement on the run's account,
// with the amount, transfer and date the journal has, that no movement or
// statement line appears twice, and that every match and break is what its
// kind says (a reference match joins a line to a movement with its reference
// and amount, an amount-and-date match one with its amount within the
// window). The same check runs on snapshot load and in the invariant
// checker, so a stored run always describes the journal truthfully.
//
// Reviewing a run changes only the status of its matches: confirm_matches
// accepts proposed (or reopened) matches and reopen_matches undoes a match,
// putting its statement line and movement back among the open items. A run
// is never edited otherwise; reconciling again makes a new run.

// Reconciliation limits.
const (
	MaxReconRuns = 10_000
	// MaxReconLines is the most statement lines one run holds, the most
	// recon.Parse accepts in one statement.
	MaxReconLines = 10_000
	// MaxReconItems bounds the matches and breaks all saved runs hold
	// together, so stored runs cannot grow without limit.
	MaxReconItems  = 2_000_000
	MaxReconWindow = 31
	// MaxReconExternalID is the longest statement line ID.
	MaxReconExternalID = 64
	MaxReconDays       = 366
	// MaxMatchPostings is the most ledger movements one match joins, the
	// most recon puts in a batch.
	MaxMatchPostings = 1000
	// reconRecordSlack covers what a save_reconciliation record adds around
	// the command: type, time, an escaped idempotency key and its hash.
	reconRecordSlack = 4096
	reconDateLayout  = "2006-01-02"
)

// MatchedBy says how a match was found.
type MatchedBy string

// Match kinds.
const (
	ByReference  MatchedBy = "reference"   // the line's reference is the movement's transfer reference
	ByAmountDate MatchedBy = "amount_date" // same amount, dates within the window
	// ByReferenceBatch joins a line to two or more movements whose
	// transfers carry its reference and whose amounts sum to it.
	ByReferenceBatch MatchedBy = "reference_batch"
	// ByAmountDateBatch joins a line to two or more movements within the
	// window whose amounts sum to it.
	ByAmountDateBatch MatchedBy = "amount_date_batch"
)

func (b MatchedBy) batch() bool { return b == ByReferenceBatch || b == ByAmountDateBatch }

// BreakKind says what is wrong with an item reconciliation could not match.
type BreakKind string

// Break kinds.
const (
	// BreakAmountMismatch is a line and a movement with the same reference
	// but different amounts.
	BreakAmountMismatch BreakKind = "amount_mismatch"
	// BreakMissingInLedger is a statement line no movement matched.
	BreakMissingInLedger BreakKind = "missing_in_ledger"
	// BreakMissingInStatement is a movement inside the period no line
	// matched.
	BreakMissingInStatement BreakKind = "missing_in_statement"
)

// MatchStatus is where a match is in review.
type MatchStatus string

// Match statuses.
const (
	MatchProposed  MatchStatus = "proposed"  // as the run found it
	MatchConfirmed MatchStatus = "confirmed" // accepted by a reviewer
	MatchReopened  MatchStatus = "reopened"  // undone: its line and movements are open again
)

// ReconLine is a statement line as a run keeps it. Amount is in minor units,
// signed in the account's normal-balance sense.
type ReconLine struct {
	Row        int    `json:"row"`
	ExternalID string `json:"external_id"`
	Date       string `json:"date"`
	Amount     int64  `json:"amount"`
	Reference  string `json:"reference,omitempty"`
}

// ReconPosting is a posted movement on the run's account: the entry, its
// transfer, the entry's UTC date and the amount, signed in the account's
// normal-balance sense.
type ReconPosting struct {
	Seq        uint64 `json:"seq"`
	TransferID string `json:"transfer_id"`
	Date       string `json:"date"`
	Amount     int64  `json:"amount"`
}

// ReconMatchSpec is one match of a run: a statement line and the movements
// it stands for.
type ReconMatchSpec struct {
	Line     ReconLine      `json:"line"`
	Postings []ReconPosting `json:"postings"`
	By       MatchedBy      `json:"matched_by"`
}

// ReconBreakSpec is one item a run could not match. An amount mismatch has
// a line and a posting, a missing_in_ledger break only a line, and a
// missing_in_statement break only a posting.
type ReconBreakSpec struct {
	Kind    BreakKind     `json:"kind"`
	Line    *ReconLine    `json:"line,omitempty"`
	Posting *ReconPosting `json:"posting,omitempty"`
}

// ReconSpec saves a reconciliation run. From and To are the period's first
// and last days (UTC, YYYY-MM-DD) and AsOfSeq the journal head the run was
// computed against.
type ReconSpec struct {
	ID         string           `json:"id"`
	Account    string           `json:"account"`
	From       string           `json:"from"`
	To         string           `json:"to"`
	WindowDays int              `json:"window_days"`
	AsOfSeq    uint64           `json:"as_of_seq"`
	Matches    []ReconMatchSpec `json:"matches"`
	Breaks     []ReconBreakSpec `json:"breaks"`
}

// ReconReviewSpec confirms or reopens matches of a run: those numbered in
// Matches (from 1, in the run's order), or with All every match the command
// applies to. Note is kept with each changed match.
type ReconReviewSpec struct {
	Run     string `json:"run"`
	Matches []int  `json:"matches,omitempty"`
	All     bool   `json:"all,omitempty"`
	Note    string `json:"note,omitempty"`
}

// ReconOutcome is what a reconciliation command returns: the run and how
// many matches the command changed.
type ReconOutcome struct {
	Run     string `json:"run"`
	Number  uint64 `json:"number"`
	Changed int    `json:"changed"`
}

// ReconMatch is a match as clients see it.
type ReconMatch struct {
	ID        int            `json:"id"`
	Line      ReconLine      `json:"line"`
	Postings  []ReconPosting `json:"postings"`
	By        MatchedBy      `json:"matched_by"`
	DaysApart int            `json:"days_apart"`
	Status    MatchStatus    `json:"status"`
	ChangedAt time.Time      `json:"status_changed_at,omitzero"`
	Note      string         `json:"note,omitempty"`
}

// ReconBreak is a break as clients see it.
type ReconBreak struct {
	ID      int           `json:"id"`
	Kind    BreakKind     `json:"kind"`
	Line    *ReconLine    `json:"line,omitempty"`
	Posting *ReconPosting `json:"posting,omitempty"`
}

// ReconCounts sums up a run.
type ReconCounts struct {
	StatementLines     int `json:"statement_lines"`
	LedgerPostings     int `json:"ledger_postings"`
	Matches            int `json:"matches"`
	Proposed           int `json:"proposed"`
	Confirmed          int `json:"confirmed"`
	Reopened           int `json:"reopened"`
	Breaks             int `json:"breaks"`
	AmountMismatches   int `json:"amount_mismatches"`
	MissingInLedger    int `json:"missing_in_ledger"`
	MissingInStatement int `json:"missing_in_statement"`
}

// ReconRun is a saved run as clients see it. Matches and Breaks are left
// out of listings.
type ReconRun struct {
	ID         string       `json:"id"`
	Number     uint64       `json:"number"`
	Account    string       `json:"account"`
	Currency   string       `json:"currency"`
	From       string       `json:"from"`
	To         string       `json:"to"`
	WindowDays int          `json:"window_days"`
	AsOfSeq    uint64       `json:"as_of_seq"`
	CreatedAt  time.Time    `json:"created_at"`
	Summary    ReconCounts  `json:"summary"`
	Matches    []ReconMatch `json:"matches,omitempty"`
	Breaks     []ReconBreak `json:"breaks,omitempty"`
}

// reconDef is what never changes about a run.
type reconDef struct {
	number   uint64
	created  time.Time
	currency string
	spec     ReconSpec
	counts   ReconCounts // statuses not filled in
}

// matchState is a reviewed match's status. The zero value is proposed.
type matchState struct {
	status MatchStatus // "" for proposed
	at     time.Time
	note   string
}

// reconRun is a run as the ledger keeps it. A review replaces the run with
// a new reconRun holding a new status slice, so readers and snapshots may
// keep the old one without a lock. status is nil while every match is
// proposed.
type reconRun struct {
	def    *reconDef
	status []matchState
}

func (m matchState) view() MatchStatus {
	if m.status == "" {
		return MatchProposed
	}
	return m.status
}

func (r *reconRun) state(i int) matchState {
	if r.status == nil {
		return matchState{}
	}
	return r.status[i]
}

func (r *reconRun) counts() ReconCounts {
	c := r.def.counts
	c.Proposed = c.Matches
	for _, s := range r.status {
		switch s.status {
		case MatchConfirmed:
			c.Confirmed++
			c.Proposed--
		case MatchReopened:
			c.Reopened++
			c.Proposed--
		}
	}
	return c
}

func (r *reconRun) view(detail bool) *ReconRun {
	d := r.def
	s := &d.spec
	v := &ReconRun{ID: s.ID, Number: d.number, Account: s.Account, Currency: d.currency, From: s.From, To: s.To,
		WindowDays: s.WindowDays, AsOfSeq: s.AsOfSeq, CreatedAt: d.created, Summary: r.counts()}
	if !detail {
		return v
	}
	v.Matches = make([]ReconMatch, len(s.Matches))
	for i := range s.Matches {
		m := &s.Matches[i]
		st := r.state(i)
		days := 0
		for _, p := range m.Postings {
			days = max(days, daysApart(m.Line.Date, p.Date))
		}
		v.Matches[i] = ReconMatch{ID: i + 1, Line: m.Line, Postings: m.Postings, By: m.By, DaysApart: days,
			Status: st.view(), ChangedAt: st.at, Note: st.note}
	}
	v.Breaks = make([]ReconBreak, len(s.Breaks))
	for i := range s.Breaks {
		b := &s.Breaks[i]
		v.Breaks[i] = ReconBreak{ID: i + 1, Kind: b.Kind, Line: b.Line, Posting: b.Posting}
	}
	return v
}

func reconDay(date string) (int64, bool) {
	t, err := time.Parse(reconDateLayout, date)
	if err != nil || t.Format(reconDateLayout) != date {
		return 0, false
	}
	return t.Unix() / 86400, true // dates before 1970 are refused, so this never rounds
}

func daysApart(a, b string) int {
	x, _ := reconDay(a)
	y, _ := reconDay(b)
	if x > y {
		return int(x - y)
	}
	return int(y - x)
}

func validMatchedBy(b MatchedBy) bool { return b == ByReference || b == ByAmountDate || b.batch() }

func validBreakKind(k BreakKind) bool {
	return k == BreakAmountMismatch || k == BreakMissingInLedger || k == BreakMissingInStatement
}

// validate checks a run's shape: what can be checked without the journal.
func (s *ReconSpec) validate() *Error {
	if !ValidID(s.ID) {
		return errf(CodeInvalid, "reconciliation id must be 1 to %d characters of letters, digits and _ . : -", MaxIDLength)
	}
	if !ValidID(s.Account) {
		return errf(CodeInvalid, "account is not a valid account id")
	}
	from, ok1 := reconDay(s.From)
	to, ok2 := reconDay(s.To)
	if !ok1 || !ok2 || from < 0 {
		return errf(CodeInvalid, "from and to must be dates (YYYY-MM-DD) from 1970-01-01")
	}
	if to < from || to-from > MaxReconDays {
		return errf(CodeInvalid, "the period must run forwards and end at most %d days after it starts", MaxReconDays)
	}
	if s.WindowDays < 0 || s.WindowDays > MaxReconWindow {
		return errf(CodeInvalid, "window_days must be between 0 and %d", MaxReconWindow)
	}
	lines, postings := 0, 0
	checkLine := func(where string, ln *ReconLine) *Error {
		lines++
		d, ok := reconDay(ln.Date)
		switch {
		case ln.Row < 2:
			return errf(CodeInvalid, "%s.line.row must be 2 or more: row 1 is the header", where)
		case ln.ExternalID == "" || !validText(ln.ExternalID, MaxReconExternalID):
			return errf(CodeInvalid, "%s.line.external_id must be 1 to %d characters of printable text", where, MaxReconExternalID)
		case !ok || d < 0:
			return errf(CodeInvalid, "%s.line.date must be a date (YYYY-MM-DD) from 1970-01-01", where)
		case !validText(ln.Reference, MaxReference):
			return errf(CodeInvalid, "%s.line.reference must be at most %d characters of printable text", where, MaxReference)
		}
		return nil
	}
	checkPosting := func(where string, p *ReconPosting) *Error {
		postings++
		if d, ok := reconDay(p.Date); !ok || d < 0 || p.Seq == 0 || p.Seq > s.AsOfSeq || !ValidID(p.TransferID) || p.Amount == 0 {
			return errf(CodeInvalid, "%s is not a posted movement up to entry %d", where, s.AsOfSeq)
		}
		return nil
	}
	for i := range s.Matches {
		m := &s.Matches[i]
		where := fmt.Sprintf("matches[%d]", i)
		if !validMatchedBy(m.By) {
			return errf(CodeInvalid, "%s.matched_by must be reference, amount_date, reference_batch or amount_date_batch", where)
		}
		if !m.By.batch() && len(m.Postings) != 1 {
			return errf(CodeInvalid, "%s must join its line to exactly one movement", where)
		}
		if m.By.batch() && (len(m.Postings) < 2 || len(m.Postings) > MaxMatchPostings) {
			return errf(CodeInvalid, "%s is a batch: it joins its line to 2 to %d movements", where, MaxMatchPostings)
		}
		if err := checkLine(where, &m.Line); err != nil {
			return err
		}
		for j := range m.Postings {
			if err := checkPosting(fmt.Sprintf("%s.postings[%d]", where, j), &m.Postings[j]); err != nil {
				return err
			}
		}
	}
	for i := range s.Breaks {
		b := &s.Breaks[i]
		where := fmt.Sprintf("breaks[%d]", i)
		if !validBreakKind(b.Kind) {
			return errf(CodeInvalid, "%s.kind must be amount_mismatch, missing_in_ledger or missing_in_statement", where)
		}
		if (b.Line != nil) != (b.Kind != BreakMissingInStatement) || (b.Posting != nil) != (b.Kind != BreakMissingInLedger) {
			return errf(CodeInvalid, "%s: an amount mismatch has a line and a posting, missing_in_ledger only a line, "+
				"missing_in_statement only a posting", where)
		}
		if b.Line != nil {
			if err := checkLine(where, b.Line); err != nil {
				return err
			}
		}
		if b.Posting != nil {
			if err := checkPosting(where+".posting", b.Posting); err != nil {
				return err
			}
		}
	}
	if lines > MaxReconLines {
		return errf(CodeInvalid, "a run holds at most %d statement lines, not %d", MaxReconLines, lines)
	}
	if postings > MaxMovements {
		return errf(CodeInvalid, "a run holds at most %d ledger movements, not %d", MaxMovements, postings)
	}
	return nil
}

// fits checks that a save command fits in one log record.
func (c *Command) reconFits() *Error {
	p, err := json.Marshal(c)
	if err != nil {
		return errf(CodeInvalid, "the reconciliation cannot be encoded: %v", err)
	}
	if size := len(p) + reconRecordSlack; size > wal.MaxRecord {
		return errf(CodeInvalid, "the reconciliation takes %d bytes in the log; the limit is %d, so reconcile a shorter period", size, wal.MaxRecord)
	}
	return nil
}

func (s *ReconReviewSpec) validate() *Error {
	if !ValidID(s.Run) {
		return errf(CodeInvalid, "run is not a valid reconciliation id")
	}
	if s.All == (len(s.Matches) > 0) {
		return errf(CodeInvalid, "give matches, a list of match ids, or all: true")
	}
	if len(s.Matches) > MaxReconLines {
		return errf(CodeInvalid, "at most %d matches at once", MaxReconLines)
	}
	seen := make(map[int]bool, len(s.Matches))
	for _, m := range s.Matches {
		if m < 1 || m > MaxReconLines {
			return errf(CodeInvalid, "match ids run from 1 to %d; %d is not one", MaxReconLines, m)
		}
		if seen[m] {
			return errf(CodeInvalid, "match %d appears twice", m)
		}
		seen[m] = true
	}
	if !validText(s.Note, MaxReason) {
		return errf(CodeInvalid, "note must be at most %d characters of printable text", MaxReason)
	}
	return nil
}

// reconJournal is what reconProblem reads: the journal, and the accounts and
// transfers it names. Account definitions and transfer references never
// change, so any version will do.
type reconJournal struct {
	entries   []Entry
	account   func(id string) *Account
	reference func(transferID string) (string, bool)
}

// reconProblem checks a run against the journal and returns what is wrong
// with it, or "". It is the check save_reconciliation, snapshot loading and
// the invariant checker share.
func reconProblem(s *ReconSpec, j reconJournal) string {
	if verr := s.validate(); verr != nil {
		return verr.Message
	}
	a := j.account(s.Account)
	if a == nil {
		return fmt.Sprintf("account %s does not exist", s.Account)
	}
	if s.AsOfSeq > uint64(len(j.entries)) {
		return fmt.Sprintf("the run is as of entry %d, past the head %d", s.AsOfSeq, len(j.entries))
	}
	postings := make(map[uint64]bool)
	lines := make(map[string]bool)
	line := func(where string, ln *ReconLine) string {
		if lines[ln.ExternalID] {
			return fmt.Sprintf("%s: statement line %q appears twice", where, ln.ExternalID)
		}
		lines[ln.ExternalID] = true
		return ""
	}
	posting := func(where string, p *ReconPosting) string {
		if postings[p.Seq] {
			return fmt.Sprintf("%s: entry %d appears twice", where, p.Seq)
		}
		postings[p.Seq] = true
		e := &j.entries[p.Seq-1]
		for _, ln := range e.Lines {
			if ln.Account != s.Account {
				continue
			}
			amt := ln.Posted
			if ln.Direction != a.NormalBalance {
				amt = -amt
			}
			if ln.Posted == 0 || amt != p.Amount || e.TransferID != p.TransferID || e.Time.UTC().Format(reconDateLayout) != p.Date {
				break
			}
			return ""
		}
		return fmt.Sprintf("%s does not match what entry %d posted to account %s", where, p.Seq, s.Account)
	}
	lo, _ := reconDay(s.From)
	hi, _ := reconDay(s.To)
	for i := range s.Matches {
		m := &s.Matches[i]
		where := fmt.Sprintf("matches[%d]", i)
		if p := line(where, &m.Line); p != "" {
			return p
		}
		var sum int64
		overflow := false
		for k := range m.Postings {
			p := &m.Postings[k]
			if msg := posting(fmt.Sprintf("%s.postings[%d]", where, k), p); msg != "" {
				return msg
			}
			if k > 0 && p.Seq <= m.Postings[k-1].Seq {
				return fmt.Sprintf("%s: the movements must be in journal order", where)
			}
			if (p.Amount > 0) != (m.Line.Amount > 0) {
				return fmt.Sprintf("%s: entry %d moves the other way from the line", where, p.Seq)
			}
			if next := sum + p.Amount; (p.Amount > 0 && next < sum) || (p.Amount < 0 && next > sum) {
				overflow = true
			} else {
				sum = next
			}
			if (m.By == ByAmountDate || m.By == ByAmountDateBatch) && daysApart(m.Line.Date, p.Date) > s.WindowDays {
				return fmt.Sprintf("%s: entry %d is more than %d days from the line", where, p.Seq, s.WindowDays)
			}
			if ref, _ := j.reference(p.TransferID); (m.By == ByReference || m.By == ByReferenceBatch) && (m.Line.Reference == "" || ref != m.Line.Reference) {
				return fmt.Sprintf("%s: transfer %s does not have the line's reference", where, p.TransferID)
			}
		}
		if overflow || sum != m.Line.Amount {
			return fmt.Sprintf("%s: the line is for %d, the movements sum to %d", where, m.Line.Amount, sum)
		}
	}
	for i := range s.Breaks {
		b := &s.Breaks[i]
		where := fmt.Sprintf("breaks[%d]", i)
		if b.Line != nil {
			if p := line(where, b.Line); p != "" {
				return p
			}
		}
		if b.Posting != nil {
			if p := posting(where+".posting", b.Posting); p != "" {
				return p
			}
		}
		switch b.Kind {
		case BreakAmountMismatch:
			ref, _ := j.reference(b.Posting.TransferID)
			if b.Line.Reference == "" || ref != b.Line.Reference || b.Line.Amount == b.Posting.Amount {
				return fmt.Sprintf("%s is not a line and a movement with one reference and different amounts", where)
			}
		case BreakMissingInStatement:
			if d, _ := reconDay(b.Posting.Date); d < lo || d > hi {
				return fmt.Sprintf("%s: entry %d is outside the period", where, b.Posting.Seq)
			}
		}
	}
	return ""
}

func newReconDef(s *ReconSpec, number uint64, created time.Time, currency string) *reconDef {
	d := &reconDef{number: number, created: created, currency: currency, spec: *s}
	// Empty lists are nil however they arrived (a log record, a snapshot),
	// so replayed and loaded runs compare equal.
	if len(d.spec.Matches) == 0 {
		d.spec.Matches = nil
	}
	if len(d.spec.Breaks) == 0 {
		d.spec.Breaks = nil
	}
	c := &d.counts
	c.Matches, c.Breaks = len(s.Matches), len(s.Breaks)
	for i := range s.Matches {
		c.StatementLines++
		c.LedgerPostings += len(s.Matches[i].Postings)
	}
	for i := range s.Breaks {
		switch b := &s.Breaks[i]; b.Kind {
		case BreakAmountMismatch:
			c.AmountMismatches++
		case BreakMissingInLedger:
			c.MissingInLedger++
		case BreakMissingInStatement:
			c.MissingInStatement++
		}
		if s.Breaks[i].Line != nil {
			c.StatementLines++
		}
		if s.Breaks[i].Posting != nil {
			c.LedgerPostings++
		}
	}
	return d
}

func (d *reconDef) items() int { return len(d.spec.Matches) + len(d.spec.Breaks) }

// recon returns the txn's view of a run, or nil.
func (t *txn) recon(id string) *reconRun {
	for i := len(t.recons) - 1; i >= 0; i-- {
		if t.recons[i].def.spec.ID == id {
			return t.recons[i]
		}
	}
	if n, ok := t.st.reconByID[id]; ok {
		return t.st.recons[n-1]
	}
	return nil
}

// reconTotals returns how many runs and items exist with the txn's own.
func (t *txn) reconTotals() (runs uint64, items int) {
	runs, items = uint64(len(t.st.recons)), t.st.reconItems
	for _, r := range t.recons {
		if r.def.number > runs {
			runs = r.def.number
			items += r.def.items()
		}
	}
	return runs, items
}

func (t *txn) saveRecon(s *ReconSpec) (Outcome, *Error) {
	if t.recon(s.ID) != nil {
		return Outcome{}, errf(CodeAlreadyExists, "reconciliation %s already exists", s.ID)
	}
	a := t.peek(s.Account)
	if a == nil {
		return Outcome{}, errf(CodeNotFound, "account %s does not exist", s.Account)
	}
	runs, items := t.reconTotals()
	if runs >= MaxReconRuns {
		return Outcome{}, errf(CodeLimitExceeded, "at most %d saved reconciliations", MaxReconRuns)
	}
	if n := len(s.Matches) + len(s.Breaks); items+n > MaxReconItems {
		return Outcome{}, errf(CodeLimitExceeded, "saved reconciliations hold at most %d matches and breaks; %d are in use", MaxReconItems, items)
	}
	// A run is computed against the committed journal, so the entries it
	// names are committed ones; on replay each record commits before the
	// next, which gives the same journal.
	j := reconJournal{entries: t.st.entries, account: func(id string) *Account {
		if a := t.peek(id); a != nil {
			return &a.Account
		}
		return nil
	}, reference: func(id string) (string, bool) {
		tr, ok := t.transfer(id)
		if !ok {
			return "", false
		}
		return tr.Reference, true
	}}
	if p := reconProblem(s, j); p != "" {
		return Outcome{}, errf(CodePrecondition, "the reconciliation does not match the journal: %s", p)
	}
	r := &reconRun{def: newReconDef(s, runs+1, t.now, a.Currency)}
	t.recons = append(t.recons, r)
	return Outcome{Recon: &ReconOutcome{Run: s.ID, Number: r.def.number, Changed: len(s.Matches)}}, nil
}

// reviewRecon confirms (to = MatchConfirmed) or reopens matches. Confirming
// takes a proposed or reopened match; reopening a proposed or confirmed one.
// Naming a match that is already where the command would put it is an
// error; with All, such matches are left alone.
func (t *txn) reviewRecon(s *ReconReviewSpec, to MatchStatus) (Outcome, *Error) {
	r := t.recon(s.Run)
	if r == nil {
		return Outcome{}, errf(CodeNotFound, "reconciliation %s does not exist", s.Run)
	}
	n := len(r.def.spec.Matches)
	for _, m := range s.Matches {
		if m > n {
			return Outcome{}, errf(CodeNotFound, "reconciliation %s has %d matches; there is no match %d", s.Run, n, m)
		}
		if r.state(m-1).view() == to {
			return Outcome{}, errf(CodePrecondition, "match %d of reconciliation %s is already %s", m, s.Run, to)
		}
	}
	status := make([]matchState, n)
	copy(status, r.status)
	changed := 0
	set := func(i int) {
		status[i] = matchState{status: to, at: t.now, note: s.Note}
		changed++
	}
	if s.All {
		for i := range status {
			if status[i].view() != to {
				set(i)
			}
		}
	} else {
		for _, m := range s.Matches {
			set(m - 1)
		}
	}
	if changed > 0 {
		t.recons = append(t.recons, &reconRun{def: r.def, status: status})
	}
	return Outcome{Recon: &ReconOutcome{Run: s.Run, Number: r.def.number, Changed: changed}}, nil
}

// commitRecons makes the txn's runs the committed ones. It runs inside
// txn.commit, under the write lock.
func (t *txn) commitRecons() {
	st := t.st
	for _, r := range t.recons {
		n := r.def.number
		if n == uint64(len(st.recons))+1 {
			st.recons = append(st.recons, r)
			st.reconByID[r.def.spec.ID] = n
			st.reconItems += r.def.items()
			continue
		}
		if st.view != nil {
			st.view.keepRecon(st.recons[n-1])
		}
		st.recons[n-1] = r
	}
}

// ReconFilter selects saved runs. An empty Account matches every run.
type ReconFilter struct {
	Account string
}

// ReconPage is one page of saved runs, oldest first. Next is the number of
// the last run the page examined, to pass as after for the next page.
type ReconPage struct {
	Runs    []*ReconRun
	Next    uint64
	HasMore bool
}

// Recons lists saved runs numbered after after, without their matches and
// breaks.
func (l *Ledger) Recons(f ReconFilter, after uint64, limit int) (ReconPage, error) {
	if limit <= 0 || limit > MaxListLimit {
		return ReconPage{}, errf(CodeInvalid, "limit must be between 1 and %d", MaxListLimit)
	}
	if f.Account != "" && !ValidID(f.Account) {
		return ReconPage{}, errf(CodeInvalid, "%q is not a valid account id", f.Account)
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	runs := l.st.recons
	page := ReconPage{Runs: []*ReconRun{}, Next: after}
	for n := after + 1; n <= uint64(len(runs)); n++ {
		r := runs[n-1]
		if f.Account != "" && r.def.spec.Account != f.Account {
			page.Next = n
			continue
		}
		if len(page.Runs) == limit {
			page.HasMore = true
			return page, nil
		}
		page.Runs = append(page.Runs, r.view(false))
		page.Next = n
	}
	return page, nil
}

// Recon returns one saved run with its matches and breaks.
func (l *Ledger) Recon(id string) (*ReconRun, error) {
	l.mu.RLock()
	n, ok := l.st.reconByID[id]
	var r *reconRun
	if ok {
		r = l.st.recons[n-1]
	}
	l.mu.RUnlock()
	if r == nil {
		return nil, errf(CodeNotFound, "reconciliation %s does not exist", id)
	}
	return r.view(true), nil // a committed run never changes, so no lock is needed
}

// reconStateProblems checks the runs as of a journal head: numbered from 1
// in order and filed under their IDs, within the limits, and each sound
// (reconRunProblems).
func reconStateProblems(runs []*reconRun, byID func(id string) (uint64, bool), j reconJournal, bad func(string, ...any)) {
	if len(runs) > MaxReconRuns {
		bad("%d saved reconciliations, more than %d", len(runs), MaxReconRuns)
	}
	items := 0
	ids := make(map[string]bool, len(runs))
	for i, r := range runs {
		d := r.def
		id := d.spec.ID
		items += d.items()
		switch n, ok := byID(id); {
		case d.number != uint64(i)+1:
			bad("reconciliation %s is number %d at position %d", id, d.number, i+1)
		case ids[id] || !ok || n != d.number:
			bad("reconciliation %s is filed twice or under the wrong number", id)
		}
		ids[id] = true
		reconRunProblems(r, j, bad)
	}
	if items > MaxReconItems {
		bad("saved reconciliations hold %d items, more than %d", items, MaxReconItems)
	}
}

// reconRunProblems checks one run: truthful about the journal
// (reconProblem), in its account's currency, with counts that match its
// items and match statuses that are valid and no older than the run.
func reconRunProblems(r *reconRun, j reconJournal, bad func(string, ...any)) {
	d := r.def
	id := d.spec.ID
	if p := reconProblem(&d.spec, j); p != "" {
		bad("reconciliation %s: %s", id, p)
	}
	if a := j.account(d.spec.Account); a != nil && a.Currency != d.currency {
		bad("reconciliation %s says %s, account %s holds %s", id, d.currency, d.spec.Account, a.Currency)
	}
	if want := newReconDef(&d.spec, d.number, d.created, d.currency).counts; want != d.counts {
		bad("reconciliation %s counts %+v, its items %+v", id, d.counts, want)
	}
	if r.status != nil && len(r.status) != len(d.spec.Matches) {
		bad("reconciliation %s has %d statuses for %d matches", id, len(r.status), len(d.spec.Matches))
		return
	}
	for k, s := range r.status {
		ok := s.status == "" && s.at.IsZero() && s.note == "" ||
			(s.status == MatchConfirmed || s.status == MatchReopened) && !s.at.Before(d.created) && validText(s.note, MaxReason)
		if !ok {
			bad("reconciliation %s match %d has a malformed status", id, k+1)
		}
	}
}

// reconsAsOf returns the runs as the frozen view sees them. The caller holds
// the read lock.
func (v *frozen) reconsAsOf() []*reconRun {
	out := slices.Clone(v.recons)
	for i, r := range out {
		if old, ok := v.reconSaved[r.def.number]; ok {
			out[i] = old
		}
	}
	return out
}

// keepRecon saves the run a review is about to replace, the first time
// since the view was taken. The caller holds the write lock.
func (v *frozen) keepRecon(r *reconRun) {
	if r.def.number > uint64(len(v.recons)) {
		return // saved after the view was taken
	}
	if _, ok := v.reconSaved[r.def.number]; !ok {
		v.reconSaved[r.def.number] = r
	}
}
