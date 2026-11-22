package recon

// ISO 20022 camt.053 (BankToCustomerStatement) statements.
//
// A camt.053 file holds one or more statements, each for one bank account,
// and each statement lists entries (Ntry): the booked lines of the account,
// one per amount the bank moved. An entry becomes one statement Line:
//
//   - Amount: Ntry/Amt in its Ccy, positive for CRDT (money into the
//     account) and negative for DBIT, exact to the currency's minor unit.
//   - Date: the booking date (BookgDt, Dt or the date part of DtTm), or the
//     value date when the bank gives no booking date.
//   - ExternalID: the bank's reference for the entry (AcctSvcrRef), else
//     NtryRef, else the bank's reference of its only transaction, else
//     "entry-N" for the Nth entry of the file.
//   - Reference: what the payer or the payout carries, the reference the
//     ledger's transfers are likely to hold: a batch entry's PmtInfId; for an
//     entry with one transaction its EndToEndId, the structured creditor
//     reference, the first unstructured remittance line or its PmtInfId;
//     for several transactions the PmtInfId they share. "NOTPROVIDED" and
//     values that are not printable text of at most MaxReference bytes are
//     passed over.
//
// Only booked entries (Sts BOOK, or Sts/Cd BOOK from version 08) are lines;
// pending and information entries are counted in Skipped. Line.Row is the
// entry's position in the file plus one, so the first entry is row 2 like
// the first data row of a CSV statement, and rows stay distinct when a
// whole file sits on one line.
//
// The parser streams the tokens of encoding/xml and keeps only the fields
// above, so its memory is the lines it returns. It is bounded like the CSV
// parser (MaxBytes, MaxLines entries) and also by element depth
// (MaxCamtDepth) and the length of the text it reads (MaxCamtText). It
// accepts only UTF-8 and refuses document type declarations, so no entity
// is ever defined or expanded.

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/laiwf94/ledgerd/internal/money"
)

// Limits on a camt.053 file, beyond MaxBytes and MaxLines (which counts
// every entry, booked or not).
const (
	MaxCamtDepth = 32   // nested elements, Document being 1
	MaxCamtText  = 1024 // bytes of text in any element the parser reads
)

// CamtNamespacePrefix is the namespace of every camt.053 version; the file's
// Document must be in it followed by the version, such as 001.02 or 001.08.
const CamtNamespacePrefix = "urn:iso:std:iso:20022:tech:xsd:camt.053."

// CamtOptions selects what to read from a camt.053 file.
type CamtOptions struct {
	// Account keeps only the statements for this account: its IBAN or its
	// other identification, compared ignoring case and spaces. When empty,
	// every statement in the file must be for the same account.
	Account string
}

// CamtStatement describes one statement of the file.
type CamtStatement struct {
	ID       string `json:"id"`
	Account  string `json:"account"`
	Currency string `json:"currency,omitempty"`
	Entries  int    `json:"entries"` // every entry, booked or not
	Booked   int    `json:"booked"`
	Selected bool   `json:"selected"` // its lines are in the result
}

// CamtResult is a parsed camt.053 file.
type CamtResult struct {
	Lines      []Line          `json:"lines"`
	Statements []CamtStatement `json:"statements"`
	Skipped    int             `json:"skipped"` // entries of selected statements that are not booked
}

// CamtError points at the place in the file that failed.
type CamtError struct {
	Line  int // line of the file, from 1
	Entry int // entry of the file, from 1; 0 outside an entry
	Msg   string
}

func (e *CamtError) Error() string {
	if e.Entry > 0 {
		return fmt.Sprintf("line %d: entry %d: %s", e.Line, e.Entry, e.Msg)
	}
	return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
}

// Paths, relative to the statement (S), the entry (N) or the transaction
// details (T), of the elements whose text the parser reads.
const (
	kStmtID      = "S/Id"
	kIBAN        = "S/Acct/Id/IBAN"
	kOthrID      = "S/Acct/Id/Othr/Id"
	kAcctCcy     = "S/Acct/Ccy"
	kNtryRef     = "N/NtryRef"
	kAmt         = "N/Amt"
	kCdtDbt      = "N/CdtDbtInd"
	kSts         = "N/Sts"
	kStsCd       = "N/Sts/Cd"
	kBookDt      = "N/BookgDt/Dt"
	kBookDtTm    = "N/BookgDt/DtTm"
	kValDt       = "N/ValDt/Dt"
	kValDtTm     = "N/ValDt/DtTm"
	kSvcrRef     = "N/AcctSvcrRef"
	kBtchPmtInf  = "N/NtryDtls/Btch/PmtInfId"
	kTxEndToEnd  = "T/Refs/EndToEndId"
	kTxPmtInf    = "T/Refs/PmtInfId"
	kTxSvcrRef   = "T/Refs/AcctSvcrRef"
	kTxCdtrRef   = "T/RmtInf/Strd/CdtrRefInf/Ref"
	kTxUstrd     = "T/RmtInf/Ustrd"
	keyStmt      = "S"
	keyEntry     = "N"
	keyTx        = "T"
	keyBk        = "B"
	keyEntryDtls = "N/NtryDtls"
)

var camtFields = [...]string{
	kStmtID, kIBAN, kOthrID, kAcctCcy,
	kNtryRef, kAmt, kCdtDbt, kSts, kStsCd, kBookDt, kBookDtTm, kValDt, kValDtTm, kSvcrRef, kBtchPmtInf,
	kTxEndToEnd, kTxPmtInf, kTxSvcrRef, kTxCdtrRef, kTxUstrd,
}

// camtIndex numbers the wanted paths; camtStep maps a path and a child's
// name to the child's path, for the paths on the way to a wanted one, so
// the parser follows the tree without building strings.
var (
	camtIndex = map[string]int{}
	camtStep  = map[[2]string]string{}
)

func init() {
	for i, k := range camtFields {
		camtIndex[k] = i
	}
	for _, k := range append(camtFields[:], keyEntryDtls) {
		segs := strings.Split(k, "/")
		for i := 1; i < len(segs); i++ {
			camtStep[[2]string{strings.Join(segs[:i], "/"), segs[i]}] = strings.Join(segs[:i+1], "/")
		}
	}
}

// fieldSet holds the text of the wanted elements of one scope.
type fieldSet struct {
	v   [len(camtFields)]string
	has [len(camtFields)]bool
}

func (f *fieldSet) get(key string) string { return f.v[camtIndex[key]] }

func (f *fieldSet) lookup(key string) (string, bool) {
	i := camtIndex[key]
	return f.v[i], f.has[i]
}

// repeatable lists the wanted elements that may occur more than once in
// their scope; the parser keeps the first. Any other wanted element
// occurring twice is an error, since the schema allows it once.
var repeatable = map[string]bool{kTxUstrd: true}

type frame struct {
	key  string // path from the enclosing scope, "" outside any
	idx  int    // index in camtFields, or -1 if the text is not read
	text []byte
}

type camtEntry struct {
	n       int
	fields  fieldSet
	ccy     string // Amt/@Ccy
	hasCcy  bool
	txs     int
	first   fieldSet // the first transaction's fields
	pmtInf  string   // the PmtInfId every transaction so far shares
	pmtSame bool
}

type camtStmt struct {
	fields  fieldSet
	lines   []Line
	entries int
	skipped int
}

type camtParser struct {
	d       *xml.Decoder
	opt     CamtOptions
	stack   []frame
	root    bool   // Document seen
	ns      string // its namespace
	done    bool   // Document closed
	entries int    // every entry so far
	stmt    *camtStmt
	entry   *camtEntry // nil outside an entry, else &entryBuf
	tx      *fieldSet  // nil outside a transaction, else &txBuf
	// One entry and one transaction are open at a time, so their fields
	// live here and are cleared for each one.
	entryBuf camtEntry
	txBuf    fieldSet
	res      CamtResult
	ids      map[string]int // external ID to entry number, selected statements
	accts    map[string]bool
}

// ParseCamt053 reads an ISO 20022 camt.053 statement file of any version.
func ParseCamt053(r io.Reader, opt CamtOptions) (*CamtResult, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBytes {
		return nil, ErrTooLarge
	}
	if !utf8.Valid(data) {
		return nil, &CamtError{Line: 1, Msg: "the file is not valid UTF-8"}
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	p := &camtParser{
		d:     xml.NewDecoder(bytes.NewReader(data)),
		opt:   opt,
		ids:   map[string]int{},
		accts: map[string]bool{},
	}
	p.d.Strict = true
	if err := p.run(); err != nil {
		return nil, err
	}
	return &p.res, nil
}

func (p *camtParser) errf(format string, args ...any) error {
	line, _ := p.d.InputPos()
	e := &CamtError{Line: line, Msg: fmt.Sprintf(format, args...)}
	if p.entry != nil {
		e.Entry = p.entry.n
	}
	return e
}

func (p *camtParser) run() error {
	for {
		at, _ := p.d.InputPos() // where the next token starts
		tok, err := p.d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var se *xml.SyntaxError
			if errors.As(err, &se) {
				return &CamtError{Line: se.Line, Msg: "not well-formed XML: " + se.Msg}
			}
			return p.errf("%v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := p.start(t); err != nil {
				return err
			}
		case xml.EndElement:
			if err := p.end(); err != nil {
				return err
			}
		case xml.CharData:
			if n := len(p.stack); n > 0 && p.stack[n-1].idx >= 0 {
				f := &p.stack[n-1]
				if len(f.text)+len(t) > MaxCamtText {
					return p.errf("%s is longer than %d bytes", f.key[2:], MaxCamtText)
				}
				f.text = append(f.text, t...)
			} else if len(p.stack) == 0 && len(bytes.TrimSpace(t)) > 0 {
				return &CamtError{Line: at, Msg: "text outside the Document element"}
			}
		case xml.Directive:
			return p.errf("document type declarations are not accepted")
		case xml.ProcInst:
			if t.Target != "xml" || p.root {
				return p.errf("processing instruction <?%s?> is not accepted", t.Target)
			}
		}
	}
	if !p.done {
		return &CamtError{Line: 1, Msg: "the file has no camt.053 Document element"}
	}
	return p.finish()
}

func (p *camtParser) start(t xml.StartElement) error {
	depth := len(p.stack) + 1
	if depth > MaxCamtDepth {
		return p.errf("elements are nested more than %d deep", MaxCamtDepth)
	}
	name := t.Name.Local
	var parent *frame
	if depth > 1 {
		parent = &p.stack[depth-2]
	}
	key := ""
	switch {
	case depth == 1:
		if p.done {
			return p.errf("the file holds more than one Document")
		}
		if name != "Document" || !camtNamespace(t.Name.Space) {
			return p.errf("the root element must be Document in namespace %s001.NN, not %s %q", CamtNamespacePrefix, name, t.Name.Space)
		}
		p.root, p.ns = true, t.Name.Space
	case t.Name.Space != p.ns:
		// Supplementary data and extensions in other namespaces carry
		// nothing the parser reads.
	case depth == 2:
		if name != "BkToCstmrStmt" {
			return p.errf("Document must hold BkToCstmrStmt, not %s", name)
		}
		key = keyBk
	case parent.key == keyBk && name == "Stmt":
		key = keyStmt
		p.stmt = &camtStmt{}
	case parent.key == keyStmt && name == "Ntry":
		key = keyEntry
		p.entries++
		if p.entries > MaxLines {
			return ErrTooLarge
		}
		p.entryBuf = camtEntry{n: p.entries, pmtSame: true}
		p.entry = &p.entryBuf
	case parent.key == keyEntryDtls && name == "TxDtls":
		key = keyTx
		p.entry.txs++
		p.txBuf = fieldSet{}
		p.tx = &p.txBuf
	case parent.key != "":
		key = camtStep[[2]string{parent.key, name}]
	}
	f := frame{key: key, idx: -1}
	if i, ok := camtIndex[key]; ok {
		f.idx = i
		// Reuse the text buffer an earlier element at this depth left.
		if depth <= cap(p.stack) {
			f.text = p.stack[:depth][depth-1].text[:0]
		}
	}
	if key == kAmt {
		for _, a := range t.Attr {
			if a.Name.Local == "Ccy" && a.Name.Space == "" {
				p.entry.ccy, p.entry.hasCcy = a.Value, true
			}
		}
	}
	p.stack = append(p.stack, f)
	return nil
}

func camtNamespace(ns string) bool {
	v, ok := strings.CutPrefix(ns, CamtNamespacePrefix)
	if !ok || len(v) != 6 || v[:4] != "001." {
		return false
	}
	return v[4] >= '0' && v[4] <= '9' && v[5] >= '0' && v[5] <= '9'
}

func (p *camtParser) end() error {
	n := len(p.stack)
	f := p.stack[n-1]
	p.stack = p.stack[:n-1]
	if f.idx >= 0 {
		if err := p.field(f.key, f.idx, string(bytes.TrimSpace(f.text))); err != nil {
			return err
		}
	}
	switch f.key {
	case keyTx:
		p.endTx()
	case keyEntry:
		if err := p.endEntry(); err != nil {
			return err
		}
		p.entry = nil
	case keyStmt:
		p.endStmt()
		p.stmt = nil
	}
	if n == 1 {
		p.done = true
	}
	return nil
}

func (p *camtParser) field(key string, i int, v string) error {
	var m *fieldSet
	switch key[0] {
	case 'S':
		m = &p.stmt.fields
	case 'N':
		m = &p.entry.fields
	default:
		m = p.tx
	}
	if m.has[i] {
		if repeatable[key] {
			return nil
		}
		return p.errf("%s appears twice", key[2:])
	}
	m.v[i], m.has[i] = v, true
	return nil
}

func (p *camtParser) endTx() {
	e, tx := p.entry, p.tx
	p.tx = nil
	if e.txs == 1 {
		e.first = *tx
		e.pmtInf = tx.get(kTxPmtInf)
	} else if tx.get(kTxPmtInf) != e.pmtInf {
		e.pmtSame = false
	}
}

func (p *camtParser) endEntry() error {
	e, s := p.entry, p.stmt
	s.entries++
	sts := e.fields.get(kStsCd)
	if sts == "" {
		sts = e.fields.get(kSts)
	}
	switch sts {
	case "BOOK":
	case "":
		return p.errf("the entry has no status (Sts)")
	default:
		s.skipped++
		return nil
	}
	amt, ok := e.fields.lookup(kAmt)
	if !ok {
		return p.errf("the entry has no amount (Amt)")
	}
	if !e.hasCcy {
		return p.errf("Amt has no Ccy attribute")
	}
	cur, ok := money.Lookup(e.ccy)
	if !ok || cur.Code != e.ccy {
		return p.errf("currency %q is not an ISO 4217 code with a minor unit", truncate(e.ccy))
	}
	v, err := camtAmount(amt, cur.Exponent)
	if err != nil {
		return p.errf("amount %q: %v", truncate(amt), err)
	}
	switch e.fields.get(kCdtDbt) {
	case "CRDT":
	case "DBIT":
		v = -v
	default:
		return p.errf("CdtDbtInd must be CRDT or DBIT, not %q", truncate(e.fields.get(kCdtDbt)))
	}
	date, err := camtDate(&e.fields)
	if err != nil {
		return p.errf("%v", err)
	}
	ln := Line{Row: e.n + 1, Date: date, Amount: v, Currency: cur.Code}
	ln.ExternalID = "entry-" + strconv.Itoa(e.n)
	for _, c := range [...]string{e.fields.get(kSvcrRef), e.fields.get(kNtryRef), txField(e, kTxSvcrRef)} {
		if c != "" {
			if !printable(c, MaxExternalID) {
				return p.errf("the entry's reference %q is not 1 to %d printable characters", truncate(c), MaxExternalID)
			}
			ln.ExternalID = c
			break
		}
	}
	ln.Reference = camtReference(e)
	s.lines = append(s.lines, ln)
	return nil
}

// txField returns a field of the entry's transaction when it has exactly one.
func txField(e *camtEntry, key string) string {
	if e.txs != 1 {
		return ""
	}
	return e.first.get(key)
}

func camtReference(e *camtEntry) string {
	var cands [5]string
	cands[0] = e.fields.get(kBtchPmtInf)
	if e.txs == 1 {
		f := &e.first
		cands[1], cands[2], cands[3], cands[4] = f.get(kTxEndToEnd), f.get(kTxCdtrRef), f.get(kTxUstrd), f.get(kTxPmtInf)
	} else if e.txs > 1 && e.pmtSame {
		cands[1] = e.pmtInf
	}
	for _, c := range cands {
		if c != "" && !strings.EqualFold(c, "NOTPROVIDED") && printable(c, MaxReference) {
			return c
		}
	}
	return ""
}

// camtAmount parses a camt amount: unsigned and exactly representable in
// the currency's minor unit. Trailing zero decimals beyond the minor unit
// are allowed (500.00 JPY is 500).
func camtAmount(s string, exponent int) (int64, error) {
	if s == "" || s[0] == '-' || s[0] == '+' {
		return 0, money.ErrSyntax
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		frac := s[i+1:]
		for len(frac) > exponent && frac[len(frac)-1] == '0' {
			frac = frac[:len(frac)-1]
		}
		if frac == "" {
			s = s[:i]
		} else {
			s = s[:i+1] + frac
		}
	}
	return money.ParseDecimal(s, exponent)
}

// camtDate returns the booking date, or the value date when there is none,
// as YYYY-MM-DD. A date-time keeps the calendar date the bank wrote.
func camtDate(f *fieldSet) (string, error) {
	for _, k := range [][2]string{{kBookDt, kBookDtTm}, {kValDt, kValDtTm}} {
		if d, ok := f.lookup(k[0]); ok {
			if _, err := time.Parse(dateLayout, d); err != nil {
				return "", fmt.Errorf("date %q must be YYYY-MM-DD", truncate(d))
			}
			return d, nil
		}
		if dt, ok := f.lookup(k[1]); ok {
			if !camtDateTime(dt) {
				return "", fmt.Errorf("date-time %q is not an ISO 8601 date and time", truncate(dt))
			}
			return dt[:10], nil
		}
	}
	return "", errors.New("the entry has no booking or value date")
}

func camtDateTime(s string) bool {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

// normAccount compares account identifiers the way people write them.
func normAccount(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

func (p *camtParser) endStmt() {
	s := p.stmt
	acct := s.fields.get(kIBAN)
	if acct == "" {
		acct = s.fields.get(kOthrID)
	}
	st := CamtStatement{ID: s.fields.get(kStmtID), Account: acct, Currency: s.fields.get(kAcctCcy), Entries: s.entries, Booked: len(s.lines)}
	st.Selected = p.opt.Account == "" || normAccount(acct) == normAccount(p.opt.Account)
	p.accts[normAccount(acct)] = true
	if st.Selected {
		p.res.Lines = append(p.res.Lines, s.lines...)
		p.res.Skipped += s.skipped
	}
	p.res.Statements = append(p.res.Statements, st)
}

func (p *camtParser) finish() error {
	if len(p.res.Statements) == 0 {
		return &CamtError{Line: 1, Msg: "the file holds no statement (Stmt)"}
	}
	if p.opt.Account == "" && len(p.accts) > 1 {
		var as []string
		for a := range p.accts {
			as = append(as, a)
		}
		sort.Strings(as)
		n := len(as)
		for i := range as {
			as[i] = truncate(as[i])
		}
		if n > 3 {
			as = append(as[:3], fmt.Sprintf("and %d more", n-3))
		}
		return &CamtError{Line: 1, Msg: fmt.Sprintf("the file holds statements for %d accounts (%s); choose one", n, strings.Join(as, ", "))}
	}
	if p.opt.Account != "" && !p.accts[normAccount(p.opt.Account)] {
		return &CamtError{Line: 1, Msg: fmt.Sprintf("the file holds no statement for account %q", truncate(p.opt.Account))}
	}
	for _, ln := range p.res.Lines {
		if first, dup := p.ids[ln.ExternalID]; dup {
			return &CamtError{Line: 1, Entry: ln.Row - 1, Msg: fmt.Sprintf("entry reference %q already appears on entry %d", ln.ExternalID, first)}
		}
		p.ids[ln.ExternalID] = ln.Row - 1
	}
	if p.res.Lines == nil {
		p.res.Lines = []Line{}
	}
	return nil
}
