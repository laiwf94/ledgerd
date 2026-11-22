// Package recon reconciles an external statement (a bank or processor file)
// against the ledger's posted movements for one account.
//
// Matching runs in three passes, the way reconciliation teams work by hand:
//
//  1. Reference: a statement line whose reference equals a movement's
//     reference is the same transaction. Failing an exact amount, a set of
//     movements with the reference that sums to the line is a batch (see
//     batch.go); otherwise it is reported as an amount mismatch, not as two
//     unrelated breaks.
//  2. Amount and date: remaining lines match a remaining movement with the
//     same amount whose date is within a window of days (settlement lag).
//  3. Batches by amount and date: a remaining line matches a set of
//     remaining movements within the window that sums to its amount, such
//     as a payout batch settled as one bank line.
//
// Whatever is left is missing on one side. The report never changes the
// ledger; fixing a break is a new transfer or a reversal.
package recon

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/laiwf94/ledgerd/internal/money"
)

// Limits on a statement file.
const (
	MaxBytes      = 4 << 20
	MaxLines      = 10_000
	MaxExternalID = 64
	MaxReference  = 128
	MaxWindowDays = 31
	dateLayout    = "2006-01-02"
)

var header = []string{"external_id", "date", "amount", "currency", "reference"}

// Line is one parsed statement line. Amount is in minor units, signed so
// that a positive amount increases the account's normal balance.
type Line struct {
	Row        int    `json:"row"`
	ExternalID string `json:"external_id"`
	Date       string `json:"date"`
	Amount     int64  `json:"amount"`
	Currency   string `json:"currency"`
	Reference  string `json:"reference,omitempty"`
}

// ParseError points at the row that failed.
type ParseError struct {
	Row int // 1 is the header
	Msg string
}

func (e *ParseError) Error() string { return fmt.Sprintf("row %d: %s", e.Row, e.Msg) }

// ErrTooLarge is returned for files over MaxBytes or MaxLines.
var ErrTooLarge = fmt.Errorf("statement is larger than %d bytes or %d lines", MaxBytes, MaxLines)

// Parse reads a CSV statement with the header
// external_id,date,amount,currency,reference. Dates are YYYY-MM-DD, amounts
// are decimals in major units with at most the currency's minor-unit digits
// (12.34 USD, 500 JPY). External IDs must be unique within the file.
func Parse(r io.Reader) ([]Line, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBytes {
		return nil, ErrTooLarge
	}
	if !utf8.Valid(data) {
		return nil, &ParseError{Row: 0, Msg: "the file is not valid UTF-8"}
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // spreadsheet byte-order mark
	cr := csv.NewReader(bytes.NewReader(data))
	cr.FieldsPerRecord = len(header)
	cr.ReuseRecord = true
	row, records := 0, 0
	var lines []Line
	seen := map[string]int{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				return nil, &ParseError{Row: pe.Line, Msg: pe.Err.Error()}
			}
			return nil, &ParseError{Row: row + 1, Msg: err.Error()}
		}
		// Rows are file lines: the csv reader skips blank lines and a quoted
		// field may span several, so counting records would drift.
		row, _ = cr.FieldPos(0)
		records++
		if records == 1 {
			for i, h := range header {
				if strings.TrimSpace(strings.ToLower(rec[i])) != h {
					return nil, &ParseError{Row: 1, Msg: "the header must be " + strings.Join(header, ",")}
				}
			}
			continue
		}
		if len(lines) == MaxLines {
			return nil, ErrTooLarge
		}
		ln, perr := parseLine(rec)
		if perr != "" {
			return nil, &ParseError{Row: row, Msg: perr}
		}
		if first, dup := seen[ln.ExternalID]; dup {
			return nil, &ParseError{Row: row, Msg: fmt.Sprintf("external_id %q already appears on row %d", ln.ExternalID, first)}
		}
		seen[ln.ExternalID] = row
		ln.Row = row
		lines = append(lines, ln)
	}
	if records == 0 {
		return nil, &ParseError{Row: 1, Msg: "the file is empty; it needs the header " + strings.Join(header, ",")}
	}
	return lines, nil
}

func parseLine(rec []string) (Line, string) {
	var ln Line
	ln.ExternalID = rec[0]
	if ln.ExternalID == "" || !printable(ln.ExternalID, MaxExternalID) {
		return ln, fmt.Sprintf("external_id must be 1 to %d printable characters", MaxExternalID)
	}
	if _, err := time.Parse(dateLayout, rec[1]); err != nil {
		return ln, fmt.Sprintf("date %q must be YYYY-MM-DD", truncate(rec[1]))
	}
	ln.Date = rec[1]
	var err error
	cur, ok := money.Lookup(rec[3])
	if !ok {
		return ln, fmt.Sprintf("currency %q is not an ISO 4217 code with a minor unit", truncate(rec[3]))
	}
	ln.Currency = cur.Code
	if ln.Amount, err = money.ParseDecimal(rec[2], cur.Exponent); err != nil {
		return ln, fmt.Sprintf("amount %q: %v", truncate(rec[2]), err)
	}
	ln.Reference = rec[4]
	if !printable(ln.Reference, MaxReference) {
		return ln, fmt.Sprintf("reference must be at most %d printable characters", MaxReference)
	}
	return ln, ""
}

func printable(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == utf8.RuneError {
			return false
		}
	}
	return true
}

func truncate(s string) string {
	if len(s) > 32 {
		return s[:32] + "..."
	}
	return s
}

// Format writes lines as a statement file that Parse accepts.
func Format(w io.Writer, lines []Line) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, ln := range lines {
		cur, ok := money.Lookup(ln.Currency)
		if !ok {
			return fmt.Errorf("unknown currency %q", ln.Currency)
		}
		if err := cw.Write([]string{ln.ExternalID, ln.Date, money.FormatDecimal(ln.Amount, cur.Exponent), ln.Currency, ln.Reference}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// Posting is a ledger movement on the account being reconciled.
type Posting struct {
	Seq        uint64    `json:"seq"`
	Time       time.Time `json:"-"`
	Date       string    `json:"date"`
	TransferID string    `json:"transfer_id"`
	Reference  string    `json:"reference,omitempty"`
	Amount     int64     `json:"amount"`
}

// Match is a statement line paired with a ledger posting, or for a batch
// with several.
type Match struct {
	ExternalID string `json:"external_id"`
	Row        int    `json:"row"`
	TransferID string `json:"transfer_id,omitempty"` // the posting; empty for a batch
	Seq        uint64 `json:"seq,omitempty"`
	Amount     int64  `json:"amount"`
	// By is "reference" or "amount_date" for one posting, and
	// "reference_batch" or "amount_date_batch" for a batch.
	By string `json:"matched_by"`
	// DaysApart is the most days between the line and any of its postings.
	DaysApart int64 `json:"days_apart"`
	// Postings lists a batch's postings, two or more, in journal order.
	Postings []Part `json:"postings,omitempty"`
}

// Part is one posting of a batch match.
type Part struct {
	Seq        uint64 `json:"seq"`
	TransferID string `json:"transfer_id"`
	Date       string `json:"date"`
	Amount     int64  `json:"amount"`
}

// Match kinds.
const (
	ByReference       = "reference"
	ByAmountDate      = "amount_date"
	ByReferenceBatch  = "reference_batch"
	ByAmountDateBatch = "amount_date_batch"
)

// Mismatch is a line and posting with the same reference but different amounts.
type Mismatch struct {
	ExternalID      string `json:"external_id"`
	Row             int    `json:"row"`
	TransferID      string `json:"transfer_id"`
	Seq             uint64 `json:"seq"`
	StatementAmount int64  `json:"statement_amount"`
	LedgerAmount    int64  `json:"ledger_amount"`
}

// Summary counts the report's sections.
type Summary struct {
	StatementLines     int `json:"statement_lines"`
	LedgerPostings     int `json:"ledger_postings"`
	Matched            int `json:"matched"`
	AmountMismatches   int `json:"amount_mismatches"`
	MissingInLedger    int `json:"missing_in_ledger"`
	MissingInStatement int `json:"missing_in_statement"`
	// BatchMatches counts the matches that join a line to several postings.
	BatchMatches int `json:"batch_matches"`
	// BatchAmbiguous counts the lines reported as breaks (missing in the
	// ledger or an amount mismatch) for which two or more sets of movements
	// sum to the line, so no batch was proposed.
	BatchAmbiguous int `json:"batch_ambiguous"`
	// BatchSearchesCut counts the lines reported as breaks whose batch
	// search could not finish (too many candidates or out of budget), so a
	// batch for them may exist.
	BatchSearchesCut int `json:"batch_searches_cut"`
}

// Report is the result of a reconciliation.
type Report struct {
	From               string     `json:"from"`
	To                 string     `json:"to"`
	WindowDays         int        `json:"window_days"`
	Matched            []Match    `json:"matched"`
	AmountMismatches   []Mismatch `json:"amount_mismatches"`
	MissingInLedger    []Line     `json:"missing_in_ledger"`
	MissingInStatement []Posting  `json:"missing_in_statement"`
	Summary            Summary    `json:"summary"`
}

// Period returns the statement's date range, the default reconciliation
// period. ok is false for a statement with no lines.
func Period(lines []Line) (from, to time.Time, ok bool) {
	for _, ln := range lines {
		d, err := time.Parse(dateLayout, ln.Date)
		if err != nil {
			continue
		}
		if !ok || d.Before(from) {
			from = d
		}
		if !ok || d.After(to) {
			to = d
		}
		ok = true
	}
	return from, to, ok
}

// day numbers whole days since the Unix epoch, rounding down.
func day(t time.Time) int64 {
	s := t.Unix()
	d := s / 86400
	if s < 0 && s%86400 != 0 {
		d--
	}
	return d
}

func dayDate(d int64) string { return time.Unix(d*86400, 0).UTC().Format(dateLayout) }

// Reconcile matches statement lines against postings. from and to are the
// period's first and last days (UTC); postings may come from up to window
// days either side, so lines near the edges can still match, but only
// postings inside the period are reported as missing from the statement.
func Reconcile(lines []Line, postings []Posting, from, to time.Time, window int) Report {
	rep := Report{From: from.Format(dateLayout), To: to.Format(dateLayout), WindowDays: window,
		Matched: []Match{}, AmountMismatches: []Mismatch{}, MissingInLedger: []Line{}, MissingInStatement: []Posting{}}
	type posting struct {
		Posting
		day int64
	}
	type line struct {
		Line
		day int64
		ok  bool // the date parsed; lines from Parse always do
	}
	ps := make([]posting, len(postings))
	for i, p := range postings {
		ps[i] = posting{Posting: p, day: day(p.Time)}
		ps[i].Date = dayDate(ps[i].day)
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Seq < ps[j].Seq })
	ls := make([]line, len(lines))
	for i, ln := range lines {
		ls[i].Line = ln
		if d, err := time.Parse(dateLayout, ln.Date); err == nil {
			ls[i].day, ls[i].ok = day(d), true
		}
	}
	sort.SliceStable(ls, func(i, j int) bool {
		if ls[i].day != ls[j].day {
			return ls[i].day < ls[j].day
		}
		return ls[i].Row < ls[j].Row
	})
	used := make([]bool, len(ps))
	done := make([]bool, len(ls))

	// byRef lists the postings with each reference, and exact those with
	// each reference and amount, in journal order. The rounds below take
	// from the front of these lists, or move a cursor along them, so a
	// reference shared by thousands of lines and movements costs linear
	// time, not lines times movements.
	type refAmount struct {
		ref string
		amt int64
	}
	byRef := map[string][]int{}
	exact := map[refAmount][]int{}
	for i, p := range ps {
		if p.Reference != "" {
			byRef[p.Reference] = append(byRef[p.Reference], i)
			k := refAmount{p.Reference, p.Amount}
			exact[k] = append(exact[k], i)
		}
	}
	b := budget{total: SearchSteps}
	var cs []cand // candidates for the line being searched, reused
	// batchMatch marks a batch's postings used and describes it.
	batchMatch := func(ln Line, lday int64, by string, pis []int) Match {
		sort.Ints(pis) // ps is in seq order
		m := Match{ExternalID: ln.ExternalID, Row: ln.Row, Amount: ln.Amount, By: by, Postings: make([]Part, len(pis))}
		for k, pi := range pis {
			used[pi] = true
			p := ps[pi]
			m.Postings[k] = Part{Seq: p.Seq, TransferID: p.TransferID, Date: p.Date, Amount: p.Amount}
			m.DaysApart = max(m.DaysApart, abs(p.day-lday))
		}
		return m
	}
	// Pass 1 runs in three rounds, so a batch never takes a movement some
	// line matches exactly and a mismatch never takes one a batch needs:
	// exact amounts, then batches, then mismatches.
	refLine := func(ln line) bool { return ln.Reference != "" && ln.ok }
	for li, ln := range ls {
		if !refLine(ln) {
			continue
		}
		// Nothing is used before this round, so the front of the list is
		// the earliest free posting.
		k := refAmount{ln.Reference, ln.Amount}
		if pis := exact[k]; len(pis) > 0 {
			pi := pis[0]
			exact[k] = pis[1:]
			used[pi], done[li] = true, true
			p := ps[pi]
			rep.Matched = append(rep.Matched, Match{ExternalID: ln.ExternalID, Row: ln.Row, TransferID: p.TransferID,
				Seq: p.Seq, Amount: p.Amount, By: ByReference, DaysApart: abs(p.day - ln.day)})
		}
	}
	// outcome is what the line's batch searches ended in when they found
	// none: ambiguous if any found two, else cut if any could not finish.
	outcome := make([]searchResult, len(ls))
	note := func(li int, res searchResult) {
		if res == ambiguous || (res == cut && outcome[li] != ambiguous) {
			outcome[li] = res
		}
	}
	for li, ln := range ls {
		if done[li] || !refLine(ln) || !batchable(ln.Amount) || len(byRef[ln.Reference]) < 2 {
			continue
		}
		if b.total <= 0 {
			note(li, cut)
			continue
		}
		b.startLine()
		cs = cs[:0]
		res := noBatch
		for _, pi := range byRef[ln.Reference] {
			if !b.spend() {
				res = cut
				break
			}
			if !used[pi] && fits(ps[pi].Amount, ln.Amount) {
				if len(cs) == MaxBatchPostings {
					res = cut // too many for any batch
					break
				}
				cs = append(cs, cand{pi: pi, amt: absAmount(ps[pi].Amount), seq: ps[pi].Seq})
			}
		}
		var pis []int
		if res != cut {
			pis, res = batch(cs, absAmount(ln.Amount), &b)
		}
		if pis != nil {
			done[li] = true
			rep.Matched = append(rep.Matched, batchMatch(ln.Line, ln.day, ByReferenceBatch, pis))
		} else {
			note(li, res)
		}
	}
	// counted adds up how the batch searches of lines reported as breaks
	// ended.
	var nCut, nAmbiguous int
	counted := func(li int) {
		switch outcome[li] {
		case cut:
			nCut++
		case ambiguous:
			nAmbiguous++
		}
	}
	// Postings only ever become used, so each reference's cursor moves
	// forward past them and never back.
	cursor := map[string]int{}
	for li, ln := range ls {
		if done[li] || !refLine(ln) {
			continue
		}
		pis := byRef[ln.Reference]
		c := cursor[ln.Reference]
		for c < len(pis) && used[pis[c]] {
			c++
		}
		cursor[ln.Reference] = c
		if c == len(pis) {
			continue
		}
		pi := pis[c]
		used[pi], done[li] = true, true
		counted(li)
		p := ps[pi]
		rep.AmountMismatches = append(rep.AmountMismatches, Mismatch{ExternalID: ln.ExternalID, Row: ln.Row,
			TransferID: p.TransferID, Seq: p.Seq, StatementAmount: ln.Amount, LedgerAmount: p.Amount})
	}

	// Pass 2. Lines go in date order and each takes the earliest free posting
	// that is not before its window. For intervals this greedy choice matches
	// as many lines as any assignment could. The union-find "next free index"
	// per amount stops it going quadratic when thousands of postings share an
	// amount and a day.
	type bucket struct {
		idx  []int // posting indexes sorted by (day, seq)
		next []int // union-find: next free position at or after i
	}
	buckets := map[int64]*bucket{}
	for i, p := range ps {
		if used[i] {
			continue
		}
		b := buckets[p.Amount]
		if b == nil {
			b = &bucket{}
			buckets[p.Amount] = b
		}
		b.idx = append(b.idx, i)
	}
	for _, b := range buckets {
		sort.SliceStable(b.idx, func(x, y int) bool { return ps[b.idx[x]].day < ps[b.idx[y]].day })
		b.next = make([]int, len(b.idx)+1)
		for i := range b.next {
			b.next[i] = i
		}
	}
	find := func(b *bucket, i int) int {
		for b.next[i] != i {
			b.next[i] = b.next[b.next[i]] // path halving
			i = b.next[i]
		}
		return i
	}
	w := int64(window)
	for li, ln := range ls {
		if done[li] || !ln.ok {
			continue
		}
		b := buckets[ln.Amount]
		if b == nil {
			continue
		}
		start := sort.Search(len(b.idx), func(i int) bool { return ps[b.idx[i]].day >= ln.day-w })
		pos := find(b, start)
		if pos == len(b.idx) || ps[b.idx[pos]].day > ln.day+w {
			continue
		}
		pi := b.idx[pos]
		b.next[pos] = pos + 1
		used[pi], done[li] = true, true
		p := ps[pi]
		rep.Matched = append(rep.Matched, Match{ExternalID: ln.ExternalID, Row: ln.Row, TransferID: p.TransferID,
			Seq: p.Seq, Amount: p.Amount, By: ByAmountDate, DaysApart: abs(p.day - ln.day)})
	}

	// Pass 3, for the lines still open. Free postings in (day, seq) order,
	// with a union-find "next free" index so postings taken by earlier
	// batches are skipped.
	open := false
	for li, ln := range ls {
		open = open || (!done[li] && ln.ok && batchable(ln.Amount))
	}
	var order []int
	if open {
		for i := range ps {
			if !used[i] {
				order = append(order, i)
			}
		}
	}
	sort.SliceStable(order, func(x, y int) bool { return ps[order[x]].day < ps[order[y]].day })
	posOf := make(map[int]int, len(order))
	for pos, pi := range order {
		posOf[pi] = pos
	}
	next := make([]int, len(order)+1)
	for i := range next {
		next[i] = i
	}
	free := func(i int) int {
		for next[i] != i {
			next[i] = next[next[i]]
			i = next[i]
		}
		return i
	}
	for li, ln := range ls {
		if !open || done[li] || !ln.ok || !batchable(ln.Amount) {
			continue
		}
		if b.total <= 0 {
			note(li, cut)
			continue
		}
		b.startLine()
		cs = cs[:0]
		res := noBatch
		start := sort.Search(len(order), func(i int) bool { return ps[order[i]].day >= ln.day-w })
		for pos := free(start); pos < len(order) && ps[order[pos]].day <= ln.day+w; pos = free(pos + 1) {
			if !b.spend() {
				res = cut
				break
			}
			if pi := order[pos]; fits(ps[pi].Amount, ln.Amount) {
				if len(cs) == MaxBatchPostings {
					res = cut // too many for any batch
					break
				}
				cs = append(cs, cand{pi: pi, amt: absAmount(ps[pi].Amount), seq: ps[pi].Seq})
			}
		}
		var pis []int
		if res != cut {
			pis, res = batch(cs, absAmount(ln.Amount), &b)
		}
		if pis == nil {
			note(li, res)
			continue
		}
		done[li] = true
		rep.Matched = append(rep.Matched, batchMatch(ln.Line, ln.day, ByAmountDateBatch, pis))
		for _, pi := range pis {
			next[posOf[pi]] = posOf[pi] + 1
		}
	}

	for li, ln := range ls {
		if !done[li] {
			counted(li)
			rep.MissingInLedger = append(rep.MissingInLedger, ln.Line)
		}
	}
	lo, hi := day(from), day(to)
	missing := 0
	for i, p := range ps {
		if !used[i] && p.day >= lo && p.day <= hi {
			missing++
		}
	}
	rep.MissingInStatement = make([]Posting, 0, missing)
	for i, p := range ps {
		if !used[i] && p.day >= lo && p.day <= hi {
			rep.MissingInStatement = append(rep.MissingInStatement, p.Posting)
		}
	}
	sort.SliceStable(rep.Matched, func(i, j int) bool { return rep.Matched[i].Row < rep.Matched[j].Row })
	sort.SliceStable(rep.AmountMismatches, func(i, j int) bool { return rep.AmountMismatches[i].Row < rep.AmountMismatches[j].Row })
	sort.SliceStable(rep.MissingInLedger, func(i, j int) bool { return rep.MissingInLedger[i].Row < rep.MissingInLedger[j].Row })
	rep.Summary = Summary{StatementLines: len(lines), LedgerPostings: len(postings), Matched: len(rep.Matched),
		AmountMismatches: len(rep.AmountMismatches), MissingInLedger: len(rep.MissingInLedger), MissingInStatement: len(rep.MissingInStatement),
		BatchSearchesCut: nCut, BatchAmbiguous: nAmbiguous}
	for _, m := range rep.Matched {
		if m.Postings != nil {
			rep.Summary.BatchMatches++
		}
	}
	return rep
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
