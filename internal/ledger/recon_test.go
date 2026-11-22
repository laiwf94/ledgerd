package ledger

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
)

// reconFixture opens a settlement account and moves money through it, as a
// bank statement would show it: two payments with references, one without,
// and a withdrawal.
type reconFixture struct {
	*env
	seqs map[string]uint64 // transfer reference or name to its entry
}

func newReconFixture(t *testing.T, persist bool) *reconFixture {
	t.Helper()
	f := &reconFixture{env: newEnv(t, persist), seqs: map[string]uint64{}}
	f.do(openAcct("settle", "USD", Debit))
	f.do(openAcct("equity", "USD", Credit))
	f.do(openAcct("eur", "EUR", Debit))
	pay := func(name, ref string, legs ...PostingSpec) {
		out := f.do(Command{CreateTransfer: &TransferSpec{Currency: "USD", Postings: legs, Reference: ref}})
		f.seqs[name] = out.Transfer.Entries[0].Seq
	}
	pay("pay1", "PAY-1", dr("settle", 1000), cr("equity", 1000))
	f.clock.Advance(24 * time.Hour)
	pay("pay2", "", dr("settle", 250), cr("equity", 250))
	pay("pay3", "PAY-3", dr("settle", 500), cr("equity", 500))
	f.clock.Advance(24 * time.Hour)
	pay("out", "", dr("equity", 75), cr("settle", 75))
	// A hold is not a movement until it posts.
	f.do(hold(60, dr("settle", 5), cr("equity", 5)))
	return f
}

func (f *reconFixture) posting(name string) ReconPosting {
	f.t.Helper()
	seq := f.seqs[name]
	f.l.mu.RLock()
	e := f.l.st.entries[seq-1]
	f.l.mu.RUnlock()
	for _, ln := range e.Lines {
		if ln.Account == "settle" {
			amt := ln.Posted
			if ln.Direction != Debit {
				amt = -amt
			}
			return ReconPosting{Seq: seq, TransferID: e.TransferID, Date: e.Time.UTC().Format(reconDateLayout), Amount: amt}
		}
	}
	f.t.Fatalf("entry %d is not on settle", seq)
	return ReconPosting{}
}

func day(f *reconFixture, n int) string {
	return f.clock.Now().Add(time.Duration(n-2) * 24 * time.Hour).UTC().Format(reconDateLayout)
}

// run is a correct run over the fixture: a reference match, an amount and
// date match, an amount mismatch, a line the ledger lacks and a movement
// the statement lacks.
func (f *reconFixture) run(id string) *ReconSpec {
	p1, p2, p3, out := f.posting("pay1"), f.posting("pay2"), f.posting("pay3"), f.posting("out")
	l3 := ReconLine{Row: 4, ExternalID: "b3", Date: day(f, 1), Amount: 499, Reference: "PAY-3"}
	l4 := ReconLine{Row: 5, ExternalID: "b4", Date: day(f, 2), Amount: 88}
	return &ReconSpec{ID: id, Account: "settle", From: day(f, 0), To: day(f, 2), WindowDays: 2, AsOfSeq: f.l.Head(),
		Matches: []ReconMatchSpec{
			{Line: ReconLine{Row: 2, ExternalID: "b1", Date: day(f, 0), Amount: 1000, Reference: "PAY-1"}, Postings: []ReconPosting{p1}, By: ByReference},
			{Line: ReconLine{Row: 3, ExternalID: "b2", Date: day(f, 2), Amount: 250}, Postings: []ReconPosting{p2}, By: ByAmountDate},
		},
		Breaks: []ReconBreakSpec{
			{Kind: BreakAmountMismatch, Line: &l3, Posting: &p3},
			{Kind: BreakMissingInLedger, Line: &l4},
			{Kind: BreakMissingInStatement, Posting: &out},
		}}
}

func saveCmd(s *ReconSpec) Command { return Command{SaveRecon: s} }
func confirmCmd(run string, all bool, ms ...int) Command {
	return Command{ConfirmRecon: &ReconReviewSpec{Run: run, Matches: ms, All: all}}
}
func reopenCmd(run, note string, ms ...int) Command {
	return Command{ReopenRecon: &ReconReviewSpec{Run: run, Matches: ms, Note: note}}
}

func (e *env) recon(id string) *ReconRun {
	e.t.Helper()
	r, err := e.l.Recon(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func TestSaveReconciliation(t *testing.T) {
	f := newReconFixture(t, false)
	spec := f.run("r1")
	out := f.do(saveCmd(spec))
	if out.Recon == nil || out.Recon.Run != "r1" || out.Recon.Number != 1 || out.Recon.Changed != 2 {
		t.Fatalf("outcome %+v", out.Recon)
	}
	r := f.recon("r1")
	want := ReconCounts{StatementLines: 4, LedgerPostings: 4, Matches: 2, Proposed: 2, Breaks: 3, AmountMismatches: 1,
		MissingInLedger: 1, MissingInStatement: 1}
	if r.Summary != want || r.Currency != "USD" || r.Number != 1 || !r.CreatedAt.Equal(f.clock.Now()) || r.AsOfSeq != spec.AsOfSeq {
		t.Fatalf("run %+v", r)
	}
	if len(r.Matches) != 2 || r.Matches[0].ID != 1 || r.Matches[1].Status != MatchProposed || r.Matches[1].DaysApart != 1 ||
		r.Matches[0].DaysApart != 0 || !r.Matches[1].ChangedAt.IsZero() {
		t.Fatalf("matches %+v", r.Matches)
	}
	if len(r.Breaks) != 3 || r.Breaks[2].ID != 3 || r.Breaks[2].Kind != BreakMissingInStatement || r.Breaks[2].Posting.Amount != -75 {
		t.Fatalf("breaks %+v", r.Breaks)
	}
	f.fail(saveCmd(f.run("r1")), CodeAlreadyExists)
	f.invariants()

	// Listing: oldest first, by account, paged by number.
	f.do(saveCmd(&ReconSpec{ID: "empty", Account: "eur", From: day(f, 0), To: day(f, 0), AsOfSeq: f.l.Head()}))
	f.do(saveCmd(f.run("r2")))
	page, err := f.l.Recons(ReconFilter{}, 0, 2)
	if err != nil || len(page.Runs) != 2 || page.Runs[0].ID != "r1" || page.Runs[1].ID != "empty" || !page.HasMore || page.Next != 2 {
		t.Fatalf("first page %+v %v", page, err)
	}
	if page.Runs[0].Matches != nil || page.Runs[0].Breaks != nil {
		t.Fatal("a listing carries matches")
	}
	page, err = f.l.Recons(ReconFilter{}, page.Next, 2)
	if err != nil || len(page.Runs) != 1 || page.Runs[0].ID != "r2" || page.HasMore || page.Next != 3 {
		t.Fatalf("second page %+v %v", page, err)
	}
	page, err = f.l.Recons(ReconFilter{Account: "settle"}, 0, 1)
	if err != nil || len(page.Runs) != 1 || page.Runs[0].ID != "r1" || !page.HasMore || page.Next != 2 {
		t.Fatalf("settle page %+v %v", page, err)
	}
	page, err = f.l.Recons(ReconFilter{Account: "settle"}, page.Next, 1)
	if err != nil || len(page.Runs) != 1 || page.Runs[0].ID != "r2" || page.Next != 3 || page.HasMore {
		t.Fatalf("settle page 2 %+v %v", page, err)
	}
	if _, err := f.l.Recons(ReconFilter{}, 0, 0); codeOf(err) != CodeInvalid {
		t.Fatalf("limit 0: %v", err)
	}
	if _, err := f.l.Recon("nope"); codeOf(err) != CodeNotFound {
		t.Fatalf("missing run: %v", err)
	}
	if r := f.recon("empty"); r.Summary != (ReconCounts{}) || r.Matches == nil || len(r.Matches) != 0 {
		t.Fatalf("empty run %+v", r)
	}
	f.invariants()
}

// A run is accepted only when it describes the journal truthfully.
func TestSaveReconciliationChecksTheJournal(t *testing.T) {
	f := newReconFixture(t, false)
	for _, tc := range []struct {
		name string
		code Code
		edit func(s *ReconSpec)
	}{
		{"wrong amount", CodePrecondition, func(s *ReconSpec) { s.Matches[0].Postings[0].Amount = 999; s.Matches[0].Line.Amount = 999 }},
		{"wrong transfer", CodePrecondition, func(s *ReconSpec) { s.Matches[0].Postings[0].TransferID = s.Matches[1].Postings[0].TransferID }},
		{"wrong date", CodePrecondition, func(s *ReconSpec) { s.Matches[0].Postings[0].Date = "2001-01-01" }},
		{"wrong sign", CodePrecondition, func(s *ReconSpec) { s.Breaks[2].Posting.Amount = 75 }},
		{"not a posted movement", CodePrecondition, func(s *ReconSpec) { s.Breaks[2].Posting.Seq = f.l.Head() }}, // the hold
		{"posting twice", CodePrecondition, func(s *ReconSpec) { *s.Breaks[0].Posting = s.Matches[0].Postings[0] }},
		{"line twice", CodePrecondition, func(s *ReconSpec) { s.Breaks[1].Line.ExternalID = "b1" }},
		{"line amount", CodePrecondition, func(s *ReconSpec) { s.Matches[1].Line.Amount = 251 }},
		{"reference", CodePrecondition, func(s *ReconSpec) { s.Matches[0].Line.Reference = "PAY-9" }},
		{"reference match without one", CodePrecondition, func(s *ReconSpec) { s.Matches[1].By = ByReference }},
		{"outside window", CodePrecondition, func(s *ReconSpec) { s.WindowDays = 0 }},
		{"mismatch amounts equal", CodePrecondition, func(s *ReconSpec) { s.Breaks[0].Line.Amount = 500 }},
		{"mismatch references differ", CodePrecondition, func(s *ReconSpec) { s.Breaks[0].Line.Reference = "PAY-1" }},
		{"missing movement outside period", CodePrecondition, func(s *ReconSpec) { s.To = s.From }},
		{"as of the future", CodePrecondition, func(s *ReconSpec) { s.AsOfSeq = f.l.Head() + 1 }},
		{"no account", CodeNotFound, func(s *ReconSpec) { s.Account = "ghost" }},
		{"seq past as of", CodeInvalid, func(s *ReconSpec) { s.AsOfSeq = f.seqs["pay1"] }},
		{"bad id", CodeInvalid, func(s *ReconSpec) { s.ID = "-x" }},
		{"bad date", CodeInvalid, func(s *ReconSpec) { s.From = "2001-9-9" }},
		{"backwards", CodeInvalid, func(s *ReconSpec) { s.From, s.To = s.To, s.From }},
		{"too long", CodeInvalid, func(s *ReconSpec) { s.From = "2000-01-01" }},
		{"window", CodeInvalid, func(s *ReconSpec) { s.WindowDays = MaxReconWindow + 1 }},
		{"header row", CodeInvalid, func(s *ReconSpec) { s.Matches[0].Line.Row = 1 }},
		{"two movements", CodeInvalid, func(s *ReconSpec) {
			s.Matches[0].Postings = append(s.Matches[0].Postings, s.Matches[1].Postings[0])
		}},
		{"kind", CodeInvalid, func(s *ReconSpec) { s.Breaks[0].Kind = "lost" }},
		{"shape", CodeInvalid, func(s *ReconSpec) { s.Breaks[1].Posting = s.Breaks[0].Posting }},
		{"matched by", CodeInvalid, func(s *ReconSpec) { s.Matches[0].By = "luck" }},
		{"control characters", CodeInvalid, func(s *ReconSpec) { s.Matches[0].Line.ExternalID = "a\nb" }},
	} {
		s := f.run("r-" + strings.ReplaceAll(tc.name, " ", "-"))
		tc.edit(s)
		err := f.fail(saveCmd(s), tc.code)
		if testing.Verbose() {
			t.Logf("%s: %s", tc.name, err.Message)
		}
	}
	if _, err := f.l.Recons(ReconFilter{}, 0, 10); err != nil {
		t.Fatal(err)
	}
	if n := len(f.l.st.recons); n != 0 {
		t.Fatalf("%d refused runs were stored", n)
	}
	f.do(saveCmd(f.run("ok")))
	f.invariants()
}

func TestReconciliationLimits(t *testing.T) {
	f := newReconFixture(t, false)
	f.do(saveCmd(f.run("r1")))
	// A run too large for one log record is refused before it is logged.
	big := &ReconSpec{ID: "big", Account: "settle", From: day(f, 0), To: day(f, 0), AsOfSeq: f.l.Head()}
	for i := range MaxReconLines {
		big.Breaks = append(big.Breaks, ReconBreakSpec{Kind: BreakMissingInLedger, Line: &ReconLine{Row: i + 2,
			ExternalID: fmt.Sprintf("%064d", i), Date: day(f, 0), Amount: 1, Reference: strings.Repeat("r", MaxReference)}})
	}
	if c := saveCmd(big); c.Validate() != nil {
		err := c.Validate()
		t.Fatalf("a run of %d lines: %v", MaxReconLines, err)
	}
	big.Breaks = append(big.Breaks, big.Breaks[0])
	if c := saveCmd(big); c.Validate() == nil {
		t.Fatal("too many lines accepted")
	} else if err := c.Validate(); err == nil || !strings.Contains(err.Message, "at most 10000 statement lines") {
		t.Fatalf("too many lines: %v", err)
	}
	// Counts against the whole store, with the runs a txn adds counted.
	f.l.mu.Lock()
	saved := f.l.st.recons
	full := make([]*reconRun, MaxReconRuns)
	for i := range full {
		full[i] = saved[0]
	}
	f.l.st.recons = full
	f.l.mu.Unlock()
	f.fail(saveCmd(f.run("r2")), CodeLimitExceeded)
	f.l.mu.Lock()
	f.l.st.recons, f.l.st.reconItems = saved, MaxReconItems-4
	f.l.mu.Unlock()
	f.fail(saveCmd(f.run("r2")), CodeLimitExceeded) // its 5 items do not fit
	f.l.mu.Lock()
	f.l.st.reconItems = MaxReconItems - 10
	f.l.mu.Unlock()
	f.do(saveCmd(f.run("r2")))
	if f.l.st.reconItems != MaxReconItems-5 {
		t.Fatalf("items %d", f.l.st.reconItems)
	}
}

func TestReviewReconciliation(t *testing.T) {
	f := newReconFixture(t, false)
	f.do(saveCmd(f.run("r1")))
	f.do(saveCmd(f.run("r2")))
	f.clock.Advance(time.Hour)
	confirmAt := f.clock.Now()
	c := confirmCmd("r1", false, 2)
	c.ConfirmRecon.Note = "seen on the bank portal"
	if out := f.do(c); out.Recon.Changed != 1 || out.Recon.Number != 1 {
		t.Fatalf("confirm %+v", out.Recon)
	}
	r := f.recon("r1")
	m := r.Matches[1]
	if m.Status != MatchConfirmed || !m.ChangedAt.Equal(confirmAt) || m.Note != "seen on the bank portal" || r.Matches[0].Status != MatchProposed {
		t.Fatalf("after confirm %+v", r.Matches)
	}
	if r.Summary.Proposed != 1 || r.Summary.Confirmed != 1 || r.Summary.Reopened != 0 {
		t.Fatalf("summary %+v", r.Summary)
	}
	if f.recon("r2").Summary.Proposed != 2 {
		t.Fatal("reviewing one run changed another")
	}
	f.fail(confirmCmd("r1", false, 2), CodePrecondition) // already confirmed
	f.fail(confirmCmd("r1", false, 3), CodeNotFound)     // no such match
	f.fail(confirmCmd("ghost", false, 1), CodeNotFound)  // no such run
	f.fail(confirmCmd("r1", false), CodeInvalid)         // nothing named
	f.fail(confirmCmd("r1", true, 1), CodeInvalid)       // both
	f.fail(confirmCmd("r1", false, 1, 1), CodeInvalid)   // twice
	f.fail(reopenCmd("r1", strings.Repeat("x", 501), 1), CodeInvalid)

	// Reopening undoes a match; a reopened match can be confirmed again.
	f.clock.Advance(time.Hour)
	if out := f.do(reopenCmd("r1", "the bank reversed it", 2, 1)); out.Recon.Changed != 2 {
		t.Fatalf("reopen %+v", out.Recon)
	}
	f.fail(reopenCmd("r1", "", 1), CodePrecondition)
	r = f.recon("r1")
	if r.Summary.Reopened != 2 || r.Summary.Proposed != 0 || r.Matches[0].Note != "the bank reversed it" || !r.Matches[0].ChangedAt.Equal(f.clock.Now()) {
		t.Fatalf("after reopen %+v", r)
	}
	if out := f.do(confirmCmd("r1", true)); out.Recon.Changed != 2 {
		t.Fatalf("confirm all %+v", out.Recon)
	}
	if out := f.do(confirmCmd("r1", true)); out.Recon.Changed != 0 {
		t.Fatalf("confirming all again changed %d", out.Recon.Changed)
	}
	if s := f.recon("r1").Summary; s.Confirmed != 2 || s.Proposed != 0 || s.Reopened != 0 {
		t.Fatalf("summary %+v", s)
	}
	// A view handed out earlier never changes.
	if m.Status != MatchConfirmed || r.Summary.Reopened != 2 {
		t.Fatal("a returned view changed")
	}

	// Commands in one group commit see each other: a save and a review
	// queued together.
	ctx := context.Background()
	done := make(chan error, 2)
	r3 := f.run("r3")
	go func() { _, err := f.l.Do(ctx, saveCmd(r3), nil); done <- err }()
	go func() {
		for {
			_, err := f.l.Do(ctx, confirmCmd("r3", false, 1), nil)
			if codeOf(err) != CodeNotFound {
				done <- err
				return
			}
		}
	}()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if s := f.recon("r3").Summary; s.Confirmed != 1 {
		t.Fatalf("r3 %+v", s)
	}
	f.invariants()
}

// Runs survive restarts exactly: replayed from the log, loaded from a
// snapshot, and checked by ledgerd verify.
func TestReconciliationsSurviveRestarts(t *testing.T) {
	f := newReconFixture(t, true)
	ctx := context.Background()
	if _, err := f.l.Do(ctx, saveCmd(f.run("r1")), key("api", "save", "save")); err != nil {
		t.Fatal(err)
	}
	f.l.Do(ctx, saveCmd(f.run("r1")), key("api", "dup", "dup")) // a keyed refusal
	f.do(saveCmd(&ReconSpec{ID: "empty", Account: "eur", From: day(f, 0), To: day(f, 0), AsOfSeq: f.l.Head()}))
	f.do(confirmCmd("r1", false, 1))
	c := reopenCmd("r1", "later", 2)
	if _, err := f.l.Do(ctx, c, key("api", "reopen", "reopen")); err != nil {
		t.Fatal(err)
	}
	before := f.recon("r1")

	f.restart() // replays the log
	if got := f.recon("r1"); !reflect.DeepEqual(got, before) {
		t.Fatalf("after replay\n%+v\nwant\n%+v", got, before)
	}
	out, err := f.l.Do(ctx, c, key("api", "reopen", "reopen"))
	if err != nil || !out.Replayed || out.Recon.Changed != 1 || out.Recon.Run != "r1" {
		t.Fatalf("keyed replay %+v %v", out, err)
	}
	if _, err := f.l.Do(ctx, saveCmd(f.run("r1")), key("api", "dup", "dup")); codeOf(err) != CodeAlreadyExists {
		t.Fatalf("keyed refusal: %v", err)
	}

	f.opts.SnapshotEvery = 0
	f.restart() // writes a snapshot at close
	f.opts.SnapshotEvery = -1
	f.restart() // loads it
	if nums := snapshotFiles(t, f.dir); len(nums) == 0 {
		t.Fatal("no snapshot")
	}
	if got := f.recon("r1"); !reflect.DeepEqual(got, before) {
		t.Fatalf("after snapshot\n%+v\nwant\n%+v", got, before)
	}
	full := openAt(t, copyDir(t, f.dir, false), f.clock, nil)
	sameState(t, "snapshot and log", f.l, full)
	out, err = f.l.Do(ctx, saveCmd(f.run("r1")), key("api", "save", "save"))
	if err != nil || !out.Replayed || out.Recon.Number != 1 || out.Recon.Changed != 2 {
		t.Fatalf("keyed save from the snapshot: %+v %v", out, err)
	}
	f.invariants()
	full.Close()
	f.l.Close()
	r := mustVerify(t, f.dir)
	if r.Status != StatusSound {
		t.Fatalf("verify: %s %q %q", r.Status, r.Damage, r.Repairable)
	}
}

// A run that no longer agrees with the journal is reported by the invariant
// checker and refused in a snapshot.
func TestReconciliationCorruptionIsCaught(t *testing.T) {
	f := newReconFixture(t, true)
	f.do(saveCmd(f.run("r1")))
	f.do(confirmCmd("r1", false, 1))
	f.invariants()
	for _, tc := range []struct {
		name, want string
		snap       bool // the snapshot format can hold the damage
		edit       func(r *reconRun)
	}{
		{"amount", "does not match what entry", true, func(r *reconRun) { r.def.spec.Breaks[2].Posting.Amount = -76 }},
		{"currency", "says EUR", true, func(r *reconRun) { r.def.currency = "EUR" }},
		{"status", "malformed status", true, func(r *reconRun) { r.status[1] = matchState{status: "maybe"} }},
		{"note", "malformed status", true, func(r *reconRun) { r.status[1] = matchState{note: "proposed with a note"} }},
		{"counts", "counts", false, func(r *reconRun) { r.def.counts.Matches = 7 }},
		{"statuses", "statuses for", false, func(r *reconRun) { r.status = r.status[:1] }},
		{"number", "number", false, func(r *reconRun) { r.def.number = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.l.mu.Lock()
			orig := f.l.st.recons[0]
			d := *orig.def
			d.spec.Breaks = append([]ReconBreakSpec(nil), d.spec.Breaks...)
			p := *d.spec.Breaks[2].Posting
			d.spec.Breaks[2].Posting = &p
			bad := &reconRun{def: &d, status: append([]matchState(nil), orig.status...)}
			tc.edit(bad)
			f.l.st.recons[0] = bad
			f.l.mu.Unlock()
			defer func() {
				f.l.mu.Lock()
				f.l.st.recons[0] = orig
				f.l.mu.Unlock()
			}()
			v := f.l.CheckInvariants()
			if len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), tc.want) {
				t.Fatalf("violations %q, want %q", v, tc.want)
			}
			if !tc.snap {
				return
			}
			s := f.l.capture(99, f.clock.Now())
			dir := t.TempDir()
			if _, err := writeSnapshot(wal.OSFS{}, dir, s); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readSnapshot(wal.OSFS{}, filepath.Join(dir, snapName(99))); err == nil {
				t.Fatal("a snapshot with a corrupt run was accepted")
			}
		})
	}
	f.invariants()
	// The snapshot reader refuses a run naming another account's movements.
	s := f.l.capture(98, f.clock.Now())
	d := *s.recons[0].def
	d.spec.Account, d.currency = "eur", "EUR"
	s.recons = []*reconRun{{def: &d}}
	dir := t.TempDir()
	if _, err := writeSnapshot(wal.OSFS{}, dir, s); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSnapshot(wal.OSFS{}, filepath.Join(dir, snapName(98))); err == nil || !strings.Contains(err.Error(), "reconciliation") {
		t.Fatalf("wrong account accepted: %v", err)
	}
	os.RemoveAll(dir)
}

// The invariant checker sees runs as of the head it froze while reviews and
// new runs commit during the check.
func TestReconciliationInvariantsDuringReview(t *testing.T) {
	f := newReconFixture(t, false)
	f.do(saveCmd(f.run("r1")))
	ctx := context.Background()
	f.l.auditStep = func(step string) {
		if step != "frozen" {
			return
		}
		if _, err := f.l.Do(ctx, confirmCmd("r1", true), nil); err != nil {
			t.Error(err)
		}
		if _, err := f.l.Do(ctx, saveCmd(f.run("r2")), nil); err != nil {
			t.Error(err)
		}
		f.l.mu.RLock()
		v := f.l.st.view
		f.l.mu.RUnlock()
		if saved := v.reconSaved[1]; saved == nil || saved.status != nil || len(v.reconsAsOf()) != 1 {
			t.Errorf("the view did not keep the run as of its head: %+v", saved)
		}
	}
	if v := f.l.CheckInvariants(); len(v) > 0 {
		t.Fatal(v)
	}
	f.l.auditStep = nil
	if s := f.recon("r1").Summary; s.Confirmed != 2 {
		t.Fatalf("%+v", s)
	}
	f.invariants()
}

// writeSmallRecons adds reconciliation runs to writeSmallSnapshot's ledger:
// a keyed save, a keyed refusal, a run left as saved and one with a
// confirmed and a reopened match.
func writeSmallRecons(t *testing.T, e *env) {
	t.Helper()
	ctx := context.Background()
	e.do(openAcct("rc", "USD", Debit))
	e.do(openAcct("rc_src", "USD", Credit))
	a := e.do(Command{CreateTransfer: &TransferSpec{Currency: "USD", Reference: "R-1", Postings: []PostingSpec{dr("rc", 120), cr("rc_src", 120)}}}).Transfer
	b := e.do(move(dr("rc_src", 30), cr("rc", 30))).Transfer
	date := e.clock.Now().UTC().Format(reconDateLayout)
	pa := ReconPosting{Seq: a.Entries[0].Seq, TransferID: a.ID, Date: date, Amount: 120}
	pb := ReconPosting{Seq: b.Entries[0].Seq, TransferID: b.ID, Date: date, Amount: -30}
	spec := &ReconSpec{ID: "rc-1", Account: "rc", From: date, To: date, WindowDays: 1, AsOfSeq: e.l.Head(),
		Matches: []ReconMatchSpec{
			{Line: ReconLine{Row: 2, ExternalID: "x1", Date: date, Amount: 120, Reference: "R-1"}, Postings: []ReconPosting{pa}, By: ByReference},
			{Line: ReconLine{Row: 3, ExternalID: "x2", Date: date, Amount: -30}, Postings: []ReconPosting{pb}, By: ByAmountDate},
		},
		Breaks: []ReconBreakSpec{{Kind: BreakMissingInLedger, Line: &ReconLine{Row: 4, ExternalID: "x3", Date: date, Amount: 9}}}}
	if _, err := e.l.Do(ctx, saveCmd(spec), key("k11", "save", "save")); err != nil {
		t.Fatal(err)
	}
	e.l.Do(ctx, saveCmd(spec), key("k11", "save-again", "save-again"))
	plain := *spec
	plain.ID = "rc-2"
	e.do(saveCmd(&plain))
	e.clock.Advance(time.Second)
	c := confirmCmd("rc-1", false, 1)
	c.ConfirmRecon.Note = "ok"
	if _, err := e.l.Do(ctx, c, key("k11", "confirm", "confirm")); err != nil {
		t.Fatal(err)
	}
	e.do(reopenCmd("rc-1", "", 2))
	// A run with batches: two movements under one reference, and two
	// without one settled together.
	var parts []ReconPosting
	for _, leg := range []struct {
		ref string
		amt int64
	}{{"B-1", 40}, {"B-1", 60}, {"", 5}, {"", 7}} {
		tr := e.do(Command{CreateTransfer: &TransferSpec{Currency: "USD", Reference: leg.ref, Postings: []PostingSpec{dr("rc", leg.amt), cr("rc_src", leg.amt)}}}).Transfer
		parts = append(parts, ReconPosting{Seq: tr.Entries[0].Seq, TransferID: tr.ID, Date: e.clock.Now().UTC().Format(reconDateLayout), Amount: leg.amt})
	}
	date = e.clock.Now().UTC().Format(reconDateLayout)
	e.do(saveCmd(&ReconSpec{ID: "rc-3", Account: "rc", From: date, To: date, WindowDays: 1, AsOfSeq: e.l.Head(),
		Matches: []ReconMatchSpec{
			{Line: ReconLine{Row: 2, ExternalID: "y1", Date: date, Amount: 100, Reference: "B-1"}, Postings: parts[:2], By: ByReferenceBatch},
			{Line: ReconLine{Row: 3, ExternalID: "y2", Date: date, Amount: 12}, Postings: parts[2:], By: ByAmountDateBatch},
		}}))
	e.do(confirmCmd("rc-3", false, 2))
}

// Batch matches join one statement line to several movements: by reference
// when every transfer carries the line's reference, by amount and date when
// every movement is within the window. Either way the movements must move
// the line's way and sum to it exactly.
func TestSaveBatchReconciliation(t *testing.T) {
	f := newReconFixture(t, true)
	pay := func(name, ref string, amt int64) {
		out := f.do(Command{CreateTransfer: &TransferSpec{Currency: "USD", Reference: ref, Postings: []PostingSpec{dr("settle", amt), cr("equity", amt)}}})
		f.seqs[name] = out.Transfer.Entries[0].Seq
	}
	pay("po1", "PO-9", 100)
	pay("po2", "PO-9", 200)
	pay("po3", "PO-9", 300)
	pay("c1", "", 11)
	pay("c2", "", 22)
	batchRun := func(id string) *ReconSpec {
		return &ReconSpec{ID: id, Account: "settle", From: day(f, 2), To: day(f, 2), WindowDays: 1, AsOfSeq: f.l.Head(),
			Matches: []ReconMatchSpec{
				{Line: ReconLine{Row: 2, ExternalID: "po", Date: day(f, 3), Amount: 600, Reference: "PO-9"},
					Postings: []ReconPosting{f.posting("po1"), f.posting("po2"), f.posting("po3")}, By: ByReferenceBatch},
				{Line: ReconLine{Row: 3, ExternalID: "card", Date: day(f, 2), Amount: 33},
					Postings: []ReconPosting{f.posting("c1"), f.posting("c2")}, By: ByAmountDateBatch},
			}}
	}
	f.do(saveCmd(batchRun("b1")))
	r := f.recon("b1")
	if r.Summary.Matches != 2 || r.Summary.StatementLines != 2 || r.Summary.LedgerPostings != 5 {
		t.Fatalf("summary %+v", r.Summary)
	}
	if m := r.Matches[0]; m.By != ByReferenceBatch || len(m.Postings) != 3 || m.DaysApart != 1 {
		t.Fatalf("reference batch %+v", m)
	}
	if m := r.Matches[1]; m.By != ByAmountDateBatch || len(m.Postings) != 2 || m.DaysApart != 0 {
		t.Fatalf("amount and date batch %+v", m)
	}
	for _, tc := range []struct {
		name string
		code Code
		edit func(s *ReconSpec)
	}{
		{"sum", CodePrecondition, func(s *ReconSpec) { s.Matches[0].Line.Amount = 599 }},
		{"one movement", CodeInvalid, func(s *ReconSpec) { s.Matches[1].Postings = s.Matches[1].Postings[:1]; s.Matches[1].Line.Amount = 11 }},
		{"too many movements", CodeInvalid, func(s *ReconSpec) {
			s.Matches[0].Postings = make([]ReconPosting, MaxMatchPostings+1)
			for i := range s.Matches[0].Postings {
				s.Matches[0].Postings[i] = f.posting("po1")
			}
		}},
		{"single with two", CodeInvalid, func(s *ReconSpec) { s.Matches[1].By = ByAmountDate }},
		{"order", CodePrecondition, func(s *ReconSpec) {
			p := s.Matches[0].Postings
			p[0], p[2] = p[2], p[0]
		}},
		{"repeated", CodePrecondition, func(s *ReconSpec) {
			s.Matches[0].Postings = []ReconPosting{f.posting("po1"), f.posting("po1"), f.posting("po2")}
			s.Matches[0].Line.Amount = 400
		}},
		{"reference", CodePrecondition, func(s *ReconSpec) {
			s.Matches[0].Postings = append(s.Matches[0].Postings, f.posting("c1"))
			s.Matches[0].Line.Amount += 11
			s.Matches[1].Postings = []ReconPosting{f.posting("pay2"), f.posting("c2")}
			s.Matches[1].Line.Amount = 272
		}},
		{"no reference on the line", CodePrecondition, func(s *ReconSpec) { s.Matches[0].Line.Reference = "" }},
		{"window", CodePrecondition, func(s *ReconSpec) { s.Matches[1].Line.Date = day(f, 4) }},
		{"sign", CodePrecondition, func(s *ReconSpec) {
			s.Matches[1].Postings = []ReconPosting{f.posting("out"), f.posting("c1"), f.posting("c2")}
			s.Matches[1].Line.Amount = 33 - 75
		}},
	} {
		s := batchRun("b-" + strings.ReplaceAll(tc.name, " ", "-"))
		tc.edit(s)
		err := f.fail(saveCmd(s), tc.code)
		if testing.Verbose() {
			t.Logf("%s: %s", tc.name, err.Message)
		}
	}
	f.do(confirmCmd("b1", false, 1))
	before := f.recon("b1")
	f.invariants()
	f.restart()
	if got := f.recon("b1"); !reflect.DeepEqual(got, before) {
		t.Fatalf("after replay\n%+v\nwant\n%+v", got, before)
	}
	f.opts.SnapshotEvery = 0
	f.restart()
	f.opts.SnapshotEvery = -1
	f.restart()
	if got := f.recon("b1"); !reflect.DeepEqual(got, before) {
		t.Fatalf("after snapshot\n%+v\nwant\n%+v", got, before)
	}
	f.invariants()
}

// writeSmallNearRecons adds a run with tolerances and a near-match of each
// kind, one of them confirmed, to the small snapshot.
func writeSmallNearRecons(t *testing.T, e *env) {
	t.Helper()
	fee := e.do(Command{CreateTransfer: &TransferSpec{Currency: "USD", Reference: "FEE-1", Postings: []PostingSpec{dr("rc", 1000), cr("rc_src", 1000)}}}).Transfer
	fx := e.do(move(dr("rc", 2000), cr("rc_src", 2000))).Transfer
	date := e.clock.Now().UTC().Format(reconDateLayout)
	e.do(saveCmd(&ReconSpec{ID: "rc-near", Account: "rc", From: date, To: date, WindowDays: 1, Tolerance: 3, ToleranceBP: 50, AsOfSeq: e.l.Head(),
		Matches: []ReconMatchSpec{
			{Line: ReconLine{Row: 2, ExternalID: "n1", Date: date, Amount: 997, Reference: "FEE-1"},
				Postings: []ReconPosting{{Seq: fee.Entries[0].Seq, TransferID: fee.ID, Date: date, Amount: 1000}}, By: ByReferenceNear},
			{Line: ReconLine{Row: 3, ExternalID: "n2", Date: date, Amount: 2009},
				Postings: []ReconPosting{{Seq: fx.Entries[0].Seq, TransferID: fx.ID, Date: date, Amount: 2000}}, By: ByAmountDateNear},
		}}))
	e.do(confirmCmd("rc-near", false, 2))
}

// A near-match joins a line to one movement whose amount differs from it
// by no more than the run's tolerance: the larger of an absolute amount
// and a percentage of the line. The ledger checks the difference against
// the run's own tolerance, and the run keeps both through replay and
// snapshots.
func TestSaveNearReconciliation(t *testing.T) {
	f := newReconFixture(t, true)
	p1, p2 := f.posting("pay1"), f.posting("pay2")
	nearRun := func(id string) *ReconSpec {
		return &ReconSpec{ID: id, Account: "settle", From: day(f, 0), To: day(f, 2), WindowDays: 1, Tolerance: 5, ToleranceBP: 100, AsOfSeq: f.l.Head(),
			Matches: []ReconMatchSpec{
				// 1% of 9.91 is 0.09, more than 0.05.
				{Line: ReconLine{Row: 2, ExternalID: "n1", Date: day(f, 0), Amount: 991, Reference: "PAY-1"}, Postings: []ReconPosting{p1}, By: ByReferenceNear},
				// 1% of 2.53 is 0.02, so 0.05 applies.
				{Line: ReconLine{Row: 3, ExternalID: "n2", Date: day(f, 2), Amount: 253}, Postings: []ReconPosting{p2}, By: ByAmountDateNear},
			}}
	}
	f.do(saveCmd(nearRun("near")))
	r := f.recon("near")
	if r.Tolerance != 5 || r.ToleranceBP != 100 || r.Summary.NearMatches != 2 || r.Summary.Matches != 2 {
		t.Fatalf("run %+v", r)
	}
	if m := r.Matches[0]; m.By != ByReferenceNear || m.Difference != -9 || m.DaysApart != 0 {
		t.Fatalf("reference near-match %+v", m)
	}
	if m := r.Matches[1]; m.By != ByAmountDateNear || m.Difference != 3 || m.DaysApart != 1 {
		t.Fatalf("amount and date near-match %+v", m)
	}
	// Exact matches carry no difference.
	f.do(saveCmd(f.run("exact")))
	if r := f.recon("exact"); r.Matches[0].Difference != 0 || r.Summary.NearMatches != 0 || r.Tolerance != 0 {
		t.Fatalf("exact run %+v", r)
	}
	for _, tc := range []struct {
		name string
		code Code
		edit func(s *ReconSpec)
	}{
		{"past the tolerance", CodePrecondition, func(s *ReconSpec) { s.Matches[0].Line.Amount = 990 }},
		{"past the absolute tolerance", CodePrecondition, func(s *ReconSpec) { s.Matches[1].Line.Amount = 256 }},
		{"no difference", CodePrecondition, func(s *ReconSpec) { s.Matches[1].Line.Amount = 250 }},
		{"no tolerance", CodePrecondition, func(s *ReconSpec) { s.Tolerance, s.ToleranceBP = 0, 0 }},
		{"negative tolerance", CodeInvalid, func(s *ReconSpec) { s.Tolerance = -1 }},
		{"negative percentage", CodeInvalid, func(s *ReconSpec) { s.ToleranceBP = -1 }},
		{"percentage too large", CodeInvalid, func(s *ReconSpec) { s.ToleranceBP = MaxReconToleranceBP + 1 }},
		{"two movements", CodeInvalid, func(s *ReconSpec) {
			s.Matches[1].Postings = append(s.Matches[1].Postings, f.posting("pay3"))
			s.Matches[1].Line.Amount = 752
		}},
		{"reference", CodePrecondition, func(s *ReconSpec) { s.Matches[0].Line.Reference = "PAY-3" }},
		{"no reference on the line", CodePrecondition, func(s *ReconSpec) { s.Matches[0].Line.Reference = "" }},
		{"window", CodePrecondition, func(s *ReconSpec) { s.Matches[1].Line.Date = day(f, 4) }},
		{"sign", CodePrecondition, func(s *ReconSpec) {
			s.Matches[1].Postings = []ReconPosting{f.posting("out")}
			s.Matches[1].Line.Amount = 72
		}},
		{"kind", CodeInvalid, func(s *ReconSpec) { s.Matches[1].By = "amount_near" }},
	} {
		s := nearRun("n-" + strings.ReplaceAll(tc.name, " ", "-"))
		tc.edit(s)
		err := f.fail(saveCmd(s), tc.code)
		if testing.Verbose() {
			t.Logf("%s: %s", tc.name, err.Message)
		}
	}
	// The largest percentage and the largest amounts do not overflow.
	big := nearRun("big")
	big.Tolerance, big.ToleranceBP = math.MaxInt64, MaxReconToleranceBP
	f.do(saveCmd(big))

	f.do(confirmCmd("near", false, 1))
	before := f.recon("near")
	f.invariants()
	f.restart()
	if got := f.recon("near"); !reflect.DeepEqual(got, before) {
		t.Fatalf("after replay\n%+v\nwant\n%+v", got, before)
	}
	f.opts.SnapshotEvery = 0
	f.restart()
	f.opts.SnapshotEvery = -1
	f.restart()
	if got := f.recon("near"); !reflect.DeepEqual(got, before) {
		t.Fatalf("after snapshot\n%+v\nwant\n%+v", got, before)
	}
	f.invariants()
}

// A version 11 snapshot, written before runs had tolerances, still loads:
// its runs have none.
func TestReconSnapshotVersion11(t *testing.T) {
	old := snapWriteVersion
	snapWriteVersion = 11
	t.Cleanup(func() { snapWriteVersion = old })
	dir, clock := writeSmallSnapshot(t, 11)
	snapWriteVersion = old
	fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
	full := openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "v11", fromSnap, full)
	page, err := fromSnap.Recons(ReconFilter{}, 0, MaxListLimit)
	if err != nil || len(page.Runs) == 0 {
		t.Fatalf("%+v %v", page, err)
	}
	for _, r := range page.Runs {
		if r.Tolerance != 0 || r.ToleranceBP != 0 || r.Summary.NearMatches != 0 {
			t.Fatalf("%+v", r)
		}
	}
	if v := allChecks(fromSnap); len(v) > 0 {
		t.Fatal(v)
	}
}
