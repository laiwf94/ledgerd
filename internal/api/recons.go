package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
	"github.com/laiwf94/ledgerd/internal/recon"
)

// Saved reconciliation runs. POST /v1/accounts/{id}/reconcile?save=true
// stores the report it computes as a run (optionally under id=), which
// GET /v1/reconciliations lists and GET /v1/reconciliations/{id} returns
// with its matches and breaks. POST .../confirm and .../reopen review
// matches. The ledger checks a saved run against the journal before
// accepting it; see internal/ledger/recon.go.

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
	spec := ledger.ReconSpec{ID: id, Account: account, From: rep.From, To: rep.To, WindowDays: rep.WindowDays, AsOfSeq: head}
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
