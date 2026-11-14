package ledger

import (
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The reference model checks listings too. After every round it walks
// GET /v1/accounts and GET /v1/transfers style listings with filters that
// reach every secondary index, in both orders, changing the page size from
// page to page, and requires each walk to be exactly the model's own answer:
// the same accounts or transfers, in the same order, each as the model
// describes it. The model computes its answers by scanning its own maps, so
// a listing that is wrong because an index is wrong shows up here even with
// CheckIndexes turned off, which TestModelCatchesBrokenIndexes proves.

func describeLimits(h []LimitChange) string {
	var b strings.Builder
	for _, c := range h {
		amount := mNoLimit
		if c.Amount != nil {
			amount = *c.Amount
		}
		fmt.Fprintf(&b, "[%s %d @%d %q]", c.Kind, amount, c.EffectiveSeq, c.Reason)
	}
	return b.String()
}

func (a *mAcct) describeLimits() string {
	var b strings.Builder
	for _, c := range a.limits {
		fmt.Fprintf(&b, "[%s %d @%d %q]", c.kind, c.amount, c.seq+1, c.reason)
	}
	return b.String()
}

// The model's filters. They mirror AccountFilter and TransferFilter but are
// evaluated here, against the model.

func mMetaMatch(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

func (m *model) listAccounts(f AccountFilter) []*mAcct {
	var out []*mAcct
	for _, a := range m.accts {
		if (f.Currency == "" || a.cur == f.Currency) && mMetaMatch(a.meta, f.Metadata) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func (m *model) listTransfers(f TransferFilter, desc bool) []*mTransfer {
	var out []*mTransfer
	for _, id := range m.order {
		t := m.xfers[id]
		inCurrency := t.cur == f.Currency || t.fx != nil && t.fx.quote == f.Currency
		if f.State != "" && t.state != f.State || f.Currency != "" && !inCurrency ||
			f.Reference != "" && t.ref != f.Reference || !mMetaMatch(t.meta, f.Metadata) {
			continue
		}
		if f.Account != "" {
			on := false
			for _, p := range t.postings {
				on = on || p.acct == f.Account
			}
			if !on {
				continue
			}
		}
		out = append(out, t)
	}
	if desc {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// pageSize picks the next page's size: mostly small, so walks span many
// pages and cursors, sometimes the maximum.
func pageSize(r *rand.Rand) int {
	return []int{1, 2, 5, 20, 100, MaxListLimit}[r.Intn(6)]
}

// maxPages stops a walk whose cursor never ends.
const maxPages = 100_000

func modelWalkAccounts(l *Ledger, f AccountFilter, r *rand.Rand, head uint64) ([]AccountState, error) {
	var out []AccountState
	after := ""
	for pages := 0; pages < maxPages; pages++ {
		p, err := l.ListAccounts(f, after, pageSize(r))
		if err != nil {
			return nil, err
		}
		if p.AsOfSeq != head {
			return nil, fmt.Errorf("account page as of %d, model is at %d", p.AsOfSeq, head)
		}
		out = append(out, p.Accounts...)
		if p.Next == "" {
			return out, nil
		}
		after = p.Next
	}
	return nil, fmt.Errorf("account listing %+v never ended", f)
}

func modelWalkTransfers(l *Ledger, f TransferFilter, desc bool, r *rand.Rand, head uint64) ([]*Transfer, error) {
	var out []*Transfer
	var from uint64
	for pages := 0; pages < maxPages; pages++ {
		p, err := l.ListTransfers(f, from, desc, pageSize(r))
		if err != nil {
			return nil, err
		}
		if p.AsOfSeq != head {
			return nil, fmt.Errorf("transfer page as of %d, model is at %d", p.AsOfSeq, head)
		}
		out = append(out, p.Transfers...)
		if p.Next == 0 {
			return out, nil
		}
		from = p.Next
	}
	return nil, fmt.Errorf("transfer listing %+v never ended", f)
}

// listingFilters returns account and transfer filters that between them
// read every kind of index set over a run: everything, each currency, each metadata
// pair the generator uses, each state, a few accounts, references and
// combinations, plus values nothing has.
func listingFilters(m *model, r *rand.Rand) ([]AccountFilter, []TransferFilter) {
	afs := []AccountFilter{{}, {Currency: "USD"}, {Currency: "EUR"}, {Currency: "JPY"},
		{Metadata: map[string]string{"tier": "missing"}}}
	for _, tier := range genTiers {
		afs = append(afs, AccountFilter{Metadata: map[string]string{"tier": tier}},
			AccountFilter{Currency: "USD", Metadata: map[string]string{"tier": tier}})
	}
	for _, region := range genRegions {
		afs = append(afs, AccountFilter{Metadata: map[string]string{"region": region}},
			AccountFilter{Metadata: map[string]string{"tier": genTiers[r.Intn(len(genTiers))], "region": region}})
	}

	ids := make([]string, 0, len(m.accts))
	for id := range m.accts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	tfs := []TransferFilter{{}, {Currency: "USD"}, {Currency: "EUR"}, {Currency: "JPY"}, {Account: "acct_missing"},
		{Reference: "ref-missing"}, {Metadata: map[string]string{"kind": "missing"}}}
	for _, s := range []TransferState{StatePending, StatePosted, StateVoided, StateExpired} {
		tfs = append(tfs, TransferFilter{State: s})
	}
	for _, id := range ids {
		tfs = append(tfs, TransferFilter{Account: id})
	}
	for _, ref := range genRefs {
		tfs = append(tfs, TransferFilter{Reference: ref})
	}
	for _, kind := range genKinds {
		tfs = append(tfs, TransferFilter{Metadata: map[string]string{"kind": kind}})
	}
	if len(ids) > 0 {
		pick := func() string { return ids[r.Intn(len(ids))] }
		tfs = append(tfs,
			TransferFilter{Account: pick(), State: StatePending},
			TransferFilter{Account: pick(), Reference: genRefs[r.Intn(len(genRefs))]},
			TransferFilter{State: StatePosted, Metadata: map[string]string{"kind": genKinds[r.Intn(len(genKinds))]}},
			TransferFilter{Currency: "USD", Metadata: map[string]string{"kind": genKinds[r.Intn(len(genKinds))], "batch": fmt.Sprint(r.Intn(3))}})
	}
	// Accounts are few and cheap to list. Transfers grow with the run, so
	// each round walks a random sample of the filters; over a run every
	// one is walked many times.
	r.Shuffle(len(tfs), func(i, j int) { tfs[i], tfs[j] = tfs[j], tfs[i] })
	return afs, tfs[:min(len(tfs), transferWalksPerRound)]
}

const transferWalksPerRound = 8

// checkListings walks every listing filter and compares each walk with the
// model. It reports the first difference.
func checkListings(l *Ledger, m *model, r *rand.Rand) error {
	afs, tfs := listingFilters(m, r)
	for _, f := range afs {
		got, err := modelWalkAccounts(l, f, r, m.head)
		if err != nil {
			return err
		}
		want := m.listAccounts(f)
		if len(got) != len(want) {
			return fmt.Errorf("accounts %+v: listed %d, model has %d (%v)", f, len(got), len(want), accountIDs(got))
		}
		for i := range got {
			if g, w := accountState(&got[i]), want[i].at(m.head); g != w {
				return fmt.Errorf("accounts %+v item %d:\nlisted %s\nmodel  %s", f, i, g, w)
			}
		}
	}
	for _, f := range tfs {
		desc := r.Intn(2) == 0
		got, err := modelWalkTransfers(l, f, desc, r, m.head)
		if err != nil {
			return err
		}
		want := m.listTransfers(f, desc)
		if len(got) != len(want) {
			return fmt.Errorf("transfers %+v desc=%v: listed %d, model has %d", f, desc, len(got), len(want))
		}
		for i := range got {
			if g, w := transferSnapshot(got[i]), m.snapshot(want[i]); g != w {
				return fmt.Errorf("transfers %+v desc=%v item %d:\nlisted %s\nmodel  %s", f, desc, i, g, w)
			}
		}
	}
	return checkFeed(l, m, r)
}

func accountIDs(as []AccountState) []string {
	ids := make([]string, len(as))
	for i := range as {
		ids[i] = as[i].ID
	}
	return ids
}

// Broken index variants. Each one leaves the indexes as a particular bug in
// their upkeep would: it is applied to the live state after every round, so
// the damage keeps growing with the workload as the bug's would. The model
// test, comparing listings only (not CheckIndexes), must catch every one.

func createdState(tr *Transfer) TransferState {
	if tr.Entries[0].Kind == KindPending {
		return StatePending
	}
	return StatePosted
}

var brokenIndexes = map[string]func(st *state){
	// updateTransfer forgets to move a transfer between state sets.
	"state never moves": func(st *state) {
		for _, tr := range st.transfers {
			if from := createdState(tr); tr.State != from {
				drop(st.ix.xferState, tr.State, createdAt(tr))
				add(st.ix.xferState, from, createdAt(tr))
			}
		}
	},
	// The sweep commits expiries without telling the indexes.
	"expiry not indexed": func(st *state) {
		for _, tr := range st.transfers {
			if tr.State == StateExpired {
				drop(st.ix.xferState, StateExpired, createdAt(tr))
				add(st.ix.xferState, StatePending, createdAt(tr))
			}
		}
	},
	// addTransferAccounts indexes only the first posting.
	"only the first posting indexed": func(st *state) {
		for _, tr := range st.transfers {
			for _, p := range tr.Postings[1:] {
				if p.Account != tr.Postings[0].Account {
					drop(st.ix.xferAccount, p.Account, createdAt(tr))
				}
			}
		}
	},
	// Reversals are indexed without their reference.
	"reversal references not indexed": func(st *state) {
		for _, tr := range st.transfers {
			if tr.Reverses != "" && tr.Reference != "" {
				drop(st.ix.xferRef, tr.Reference, createdAt(tr))
			}
		}
	},
	// Metadata on holds is not indexed.
	"pending metadata not indexed": func(st *state) {
		for _, tr := range st.transfers {
			if createdState(tr) == StatePending {
				for k, v := range tr.Metadata {
					drop(st.ix.xferMeta, metaPair{k, v}, createdAt(tr))
				}
			}
		}
	},
	// The newest transfer of each round is left out of the full set.
	"newest transfer left out": func(st *state) {
		if n := st.ix.transfers.Len(); n > 0 {
			keys := st.ix.transfers.keys()
			st.ix.transfers.remove(keys[n-1])
		}
	},
	// addAccount files every account under USD.
	"accounts filed under one currency": func(st *state) {
		for _, a := range st.accounts {
			if a.Currency != "USD" {
				drop(st.ix.acctCurrency, a.Currency, a.ID)
				add(st.ix.acctCurrency, "USD", a.ID)
			}
		}
	},
	// addAccount indexes only the first metadata pair in key order.
	"second account metadata pair not indexed": func(st *state) {
		for _, a := range st.accounts {
			keys := make([]string, 0, len(a.Metadata))
			for k := range a.Metadata {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys[min(1, len(keys)):] {
				drop(st.ix.acctMeta, metaPair{k, a.Metadata[k]}, a.ID)
			}
		}
	},
}

func TestModelCatchesBrokenIndexes(t *testing.T) {
	rounds := 120
	if testing.Short() {
		rounds = 60
	}
	// The control: the damage path itself, with nothing damaged, finds
	// no difference, so what the variants report is their damage.
	if err := runModel(t, modelRun{seed: 1, rounds: rounds, seen: map[string]int{}, damage: func(*state) {}}); err != nil {
		t.Fatalf("undamaged indexes reported: %v", err)
	}
	for _, name := range sortedKeys(brokenIndexes) {
		t.Run(name, func(t *testing.T) {
			caught := false
			for seed := int64(1); seed <= 4 && !caught; seed++ {
				err := runModel(t, modelRun{seed: seed, rounds: rounds, seen: map[string]int{}, damage: brokenIndexes[name]})
				if err != nil {
					t.Logf("seed %d caught it: %v", seed, err)
					caught = true
				}
			}
			if !caught {
				t.Fatalf("the model test never noticed indexes broken by %q", name)
			}
		})
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The model checks the journal feed as well. Each round it walks the feed
// with filters on every kind, currency and a sample of accounts, from the
// start and from a random sequence, changing the page size from page to
// page, and requires the walk to be exactly the model's own journal after
// that sequence filtered the same way: every entry's sequence, kind,
// transfer and lines. The walk must end at the model's head.

// feedFilters returns feed filters that reach every kind of filter: none,
// each kind, several kinds, each currency, accounts, combinations and
// values nothing has.
func feedFilters(m *model, r *rand.Rand) []EntryFilter {
	all := []EntryKind{KindPosted, KindPending, KindPostPending, KindVoidPending, KindExpirePending, KindReversal,
		KindIncreasePending, KindExtendPending}
	fs := []EntryFilter{{}, {Currency: "USD"}, {Currency: "EUR"}, {Currency: "JPY"}, {Currency: "GBP"}, {Account: "acct_missing"},
		{Kinds: all}, {Kinds: []EntryKind{KindPostPending, KindVoidPending, KindExpirePending}}}
	for _, k := range all {
		fs = append(fs, EntryFilter{Kinds: []EntryKind{k}})
	}
	ids := make([]string, 0, len(m.accts))
	for id := range m.accts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		pick := func() string { return ids[r.Intn(len(ids))] }
		for range 4 {
			fs = append(fs, EntryFilter{Account: pick()})
		}
		fs = append(fs,
			EntryFilter{Account: pick(), Kinds: []EntryKind{all[r.Intn(len(all))], all[r.Intn(len(all))]}},
			EntryFilter{Account: pick(), Currency: []string{"USD", "EUR", "JPY"}[r.Intn(3)]},
			EntryFilter{Currency: "EUR", Kinds: []EntryKind{KindPosted, KindReversal}})
	}
	r.Shuffle(len(fs), func(i, j int) { fs[i], fs[j] = fs[j], fs[i] })
	return fs[:min(len(fs), feedWalksPerRound)]
}

const feedWalksPerRound = 8

// feedEntry describes an entry for comparison, with every line's currency
// spelled out.
func feedEntry(e *Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s %s", e.Seq, e.Kind, e.TransferID)
	for i := range e.Lines {
		ln := &e.Lines[i]
		fmt.Fprintf(&b, " [%s %s %s %d %d]", ln.Account, ln.Direction, e.currency(ln), ln.Posted, ln.Pending)
	}
	return b.String()
}

// feed is the model's answer: its journal after after, filtered by f.
func (m *model) feed(f EntryFilter, after uint64) []string {
	owner := make([]string, m.head)
	for _, t := range m.xfers {
		for _, ref := range t.entries {
			owner[ref.Seq-1] = t.id
		}
	}
	var out []string
	for i := after; i < m.head; i++ {
		e := m.journal[i]
		if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, e.kind) {
			continue
		}
		t := m.xfers[owner[i]]
		on, inCur := false, f.Currency == "" || t.cur == f.Currency || t.fx != nil && t.fx.quote == f.Currency
		for _, ln := range e.lines {
			on = on || ln.acct == f.Account
		}
		if f.Account != "" && !on || !inCur {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d %s %s", i+1, e.kind, t.id)
		for _, ln := range e.lines {
			fmt.Fprintf(&b, " [%s %s %s %d %d]", ln.acct, ln.dir, ln.cur, ln.posted, ln.pending)
		}
		out = append(out, b.String())
	}
	return out
}

func modelWalkFeed(l *Ledger, f EntryFilter, after uint64, r *rand.Rand, head uint64) ([]string, error) {
	var out []string
	for pages := 0; pages < maxPages; pages++ {
		p, err := l.Entries(f, after, pageSize(r))
		if err != nil {
			return nil, err
		}
		if p.Head != head {
			return nil, fmt.Errorf("feed page read at head %d, model is at %d", p.Head, head)
		}
		if p.Next < after || p.Next > p.Head {
			return nil, fmt.Errorf("feed %+v after %d: next %d outside [%d, %d]", f, after, p.Next, after, p.Head)
		}
		for i := range p.Entries {
			if s := p.Entries[i].Seq; s <= after || s > p.Next {
				return nil, fmt.Errorf("feed %+v after %d: entry %d on a page that ends at %d", f, after, s, p.Next)
			}
			out = append(out, feedEntry(&p.Entries[i]))
		}
		if !p.HasMore() {
			return out, nil
		}
		after = p.Next
	}
	return nil, fmt.Errorf("feed %+v never ended", f)
}

// checkFeed walks the feed with each of a sample of filters and compares
// each walk with the model. It reports the first difference.
func checkFeed(l *Ledger, m *model, r *rand.Rand) error {
	for _, f := range feedFilters(m, r) {
		var after uint64
		if r.Intn(2) == 0 && m.head > 0 {
			after = uint64(r.Int63n(int64(m.head) + 1))
		}
		got, err := modelWalkFeed(l, f, after, r, m.head)
		if err != nil {
			return err
		}
		want := m.feed(f, after)
		if len(got) != len(want) {
			return fmt.Errorf("feed %+v after %d: %d entries, model has %d", f, after, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				return fmt.Errorf("feed %+v after %d item %d:\nfeed  %s\nmodel %s", f, after, i, got[i], want[i])
			}
		}
	}
	return nil
}
