package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
	"github.com/laiwf94/ledgerd/internal/money"
	"github.com/laiwf94/ledgerd/internal/recon"
)

// Saved reconciliation runs. POST /v1/accounts/{id}/reconcile?save=true
// stores the report it computes as a run (optionally under id=), which
// GET /v1/reconciliations lists and GET /v1/reconciliations/{id} returns
// with its matches and breaks. POST .../confirm and .../reopen review
// matches. The ledger checks a saved run against the journal before
// accepting it; see internal/ledger/recon.go.

// toleranceParams reads a reconcile request's tolerances for near-matches:
// tolerance= an amount in the account's currency (0.50 USD) and
// tolerance_pct= a percentage of each line with at most two decimals (0.25
// for a quarter of a percent), up to 10.
func toleranceParams(r *http.Request, currency string) (recon.Options, error) {
	var opt recon.Options
	q := r.URL.Query()
	if v := q.Get("tolerance"); v != "" {
		cur, ok := money.Lookup(currency)
		if !ok {
			return opt, badRequest("account currency %s has no minor unit", currency)
		}
		n, err := money.ParseDecimal(v, cur.Exponent)
		if err != nil || len(v) > maxQueryValueBytes || n < 0 {
			return opt, badRequest("tolerance must be an amount in %s of zero or more, with at most %d decimals", currency, cur.Exponent)
		}
		opt.Tolerance = n
	}
	if v := q.Get("tolerance_pct"); v != "" {
		bp, err := money.ParseDecimal(v, 2)
		if err != nil || len(v) > maxQueryValueBytes || bp < 0 || bp > recon.MaxToleranceBasisPoints {
			return opt, badRequest("tolerance_pct must be a percentage from 0 to %d with at most 2 decimals", recon.MaxToleranceBasisPoints/100)
		}
		opt.ToleranceBP = bp
	}
	return opt, nil
}

// saveParams reads save= and id= from a reconcile request.
func saveParams(r *http.Request) (save bool, id string, err error) {
	q := r.URL.Query()
	switch q.Get("save") {
	case "", "false":
	case "true":
		save = true
	default:
		return false, "", badRequest("save must be true or false")
	}
	if q.Has("id") {
		if id = q.Get("id"); !save {
			return false, "", badRequest("id names a saved run; give save=true with it")
		}
		if !ledger.ValidID(id) {
			return false, "", badRequest("%q is not a valid reconciliation id", id)
		}
	}
	return save, id, nil
}

func newRunID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "rec_" + hex.EncodeToString(b[:])
}

func utcDate(t time.Time) string { return t.UTC().Format(dateLayout) }

// reconSpec turns a report into the run the ledger stores.
func reconSpec(id, account string, rep recon.Report, lines []recon.Line, postings []recon.Posting, head uint64) ledger.ReconSpec {
	byRow := make(map[int]*recon.Line, len(lines))
	for i := range lines {
		byRow[lines[i].Row] = &lines[i]
	}
	bySeq := make(map[uint64]*recon.Posting, len(postings))
	for i := range postings {
		bySeq[postings[i].Seq] = &postings[i]
	}
	line := func(row int) ledger.ReconLine {
		ln := byRow[row]
		return ledger.ReconLine{Row: ln.Row, ExternalID: ln.ExternalID, Date: ln.Date, Amount: ln.Amount, Reference: ln.Reference}
	}
	posting := func(seq uint64) ledger.ReconPosting {
		p := bySeq[seq]
		return ledger.ReconPosting{Seq: p.Seq, TransferID: p.TransferID, Date: utcDate(p.Time), Amount: p.Amount}
	}
	spec := ledger.ReconSpec{ID: id, Account: account, From: rep.From, To: rep.To, WindowDays: rep.WindowDays,
		Tolerance: rep.Tolerance, ToleranceBP: rep.ToleranceBP, AsOfSeq: head}
	for _, m := range rep.Matched {
		ms := ledger.ReconMatchSpec{Line: line(m.Row), By: ledger.MatchedBy(m.By)}
		if m.Postings == nil {
			ms.Postings = []ledger.ReconPosting{posting(m.Seq)}
		}
		for _, p := range m.Postings {
			ms.Postings = append(ms.Postings, posting(p.Seq))
		}
		spec.Matches = append(spec.Matches, ms)
	}
	for _, m := range rep.AmountMismatches {
		ln, p := line(m.Row), posting(m.Seq)
		spec.Breaks = append(spec.Breaks, ledger.ReconBreakSpec{Kind: ledger.BreakAmountMismatch, Line: &ln, Posting: &p})
	}
	for _, l := range rep.MissingInLedger {
		ln := line(l.Row)
		spec.Breaks = append(spec.Breaks, ledger.ReconBreakSpec{Kind: ledger.BreakMissingInLedger, Line: &ln})
	}
	for _, mp := range rep.MissingInStatement {
		p := posting(mp.Seq)
		spec.Breaks = append(spec.Breaks, ledger.ReconBreakSpec{Kind: ledger.BreakMissingInStatement, Posting: &p})
	}
	return spec
}

// listRecons pages through saved runs, oldest first, optionally those of
// one account. A page resumes after next_after.
func (s *Server) listRecons(w http.ResponseWriter, r *http.Request) error {
	q, err := parseListQuery(r, func(name string) bool { return name == "account" || name == "after" })
	if err != nil {
		return err
	}
	if q.cursor != "" {
		return badRequest("the reconciliation listing resumes from after, not a cursor")
	}
	after, err := queryUint(r, "after", 0, ledger.MaxReconRuns)
	if err != nil {
		return err
	}
	var f ledger.ReconFilter
	if v, ok := q.filters["account"]; ok {
		if !ledger.ValidID(v) {
			return badRequest("%q is not a valid account id", v)
		}
		f.Account = v
	}
	page, err := s.l.Recons(f, after, q.limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"reconciliations": page.Runs, "has_more": page.HasMore, "next_after": page.Next})
	return nil
}

func (s *Server) getRecon(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	run, err := s.l.Recon(id)
	if err != nil {
		return err
	}
	if run.Matches == nil {
		run.Matches = []ledger.ReconMatch{}
	}
	if run.Breaks == nil {
		run.Breaks = []ledger.ReconBreak{}
	}
	writeJSON(w, http.StatusOK, run)
	return nil
}

// reconAging reports the saved runs' open breaks in age buckets, as of
// as_of= (a date, today in UTC by default), optionally for one account.
func (s *Server) reconAging(w http.ResponseWriter, r *http.Request) error {
	vals, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return badRequest("invalid query string: %v", err)
	}
	for name, vs := range vals {
		if name != "account" && name != "as_of" {
			return badRequest("unknown query parameter %q", name)
		}
		if len(vs) != 1 {
			return badRequest("%s may be given only once", name)
		}
		if len(vs[0]) > maxQueryValueBytes {
			return badRequest("%s is longer than %d bytes", name, maxQueryValueBytes)
		}
	}
	account := vals.Get("account")
	if vals.Has("account") && !ledger.ValidID(account) {
		return badRequest("%q is not a valid account id", account)
	}
	asOf := vals.Get("as_of")
	if vals.Has("as_of") && asOf == "" {
		return badRequest("as_of must be a date (YYYY-MM-DD)")
	}
	rep, err := s.l.ReconAging(account, asOf)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rep)
	return nil
}

// reviewBody is what confirm and reopen take.
type reviewBody struct {
	Matches []int  `json:"matches,omitempty"`
	All     bool   `json:"all,omitempty"`
	Note    string `json:"note,omitempty"`
}

// reviewRecon confirms matches, or reopens them when reopen is set, and
// returns how many changed and the run's summary.
func (s *Server) reviewRecon(reopen bool) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		var body reviewBody
		if err := decode(r, &body, false); err != nil {
			return err
		}
		spec := &ledger.ReconReviewSpec{Run: id, Matches: body.Matches, All: body.All, Note: body.Note}
		cmd := ledger.Command{ConfirmRecon: spec}
		if reopen {
			cmd = ledger.Command{ReopenRecon: spec}
		}
		out, err := s.do(w, r, cmd, body)
		if err != nil {
			return err
		}
		run, err := s.l.Recon(id)
		if err != nil {
			return err
		}
		run.Matches, run.Breaks = nil, nil
		writeJSON(w, http.StatusOK, map[string]any{"changed": out.Recon.Changed, "reconciliation": run})
		return nil
	}
}
