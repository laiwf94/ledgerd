package ledger

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
)

// allChecks runs the invariant checker and the index check.
func allChecks(l *Ledger) []string {
	return append(l.CheckInvariants(), l.CheckIndexes()...)
}

// collect gathers what an iterator yields, stopping after limit keys when
// limit is positive.
func collect[K any](seq func(func(K) bool), limit int) []K {
	var out []K
	for k := range seq {
		out = append(out, k)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// checkOrdset compares s with the sorted reference ref in every way a reader
// can look at it.
func checkOrdset[K interface{ ~uint64 | ~string }](t *testing.T, r *rand.Rand, s *ordset[K], ref []K, probe func() K) {
	t.Helper()
	if p := s.problem(); p != "" {
		t.Fatalf("damaged set: %s", p)
	}
	if s.Len() != len(ref) || !slices.Equal(s.keys(), ref) {
		t.Fatalf("set holds %v, want %v", s.keys(), ref)
	}
	if got := collect(s.ascend(*new(K), true), 0); !slices.Equal(got, ref) {
		t.Fatalf("ascend all = %v, want %v", got, ref)
	}
	rev := slices.Clone(ref)
	slices.Reverse(rev)
	if got := collect(s.descend(*new(K), true), 0); !slices.Equal(got, rev) {
		t.Fatalf("descend all = %v, want %v", got, rev)
	}
	for range 8 {
		// Probe present keys and absent ones, including below and above
		// every key, and stop early sometimes.
		k := probe()
		if len(ref) > 0 && r.Intn(2) == 0 {
			k = ref[r.Intn(len(ref))]
		}
		limit := r.Intn(4) * r.Intn(40)
		i := sort.Search(len(ref), func(i int) bool { return ref[i] > k })
		want := ref[i:]
		if limit > 0 && len(want) > limit {
			want = want[:limit]
		}
		if got := collect(s.ascend(k, false), limit); !slices.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Fatalf("ascend after %v (limit %d) = %v, want %v", k, limit, got, want)
		}
		j := sort.Search(len(ref), func(i int) bool { return ref[i] >= k })
		want = slices.Clone(ref[:j])
		slices.Reverse(want)
		if limit > 0 && len(want) > limit {
			want = want[:limit]
		}
		if got := collect(s.descend(k, false), limit); !slices.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Fatalf("descend before %v (limit %d) = %v, want %v", k, limit, got, want)
		}
		_, in := slices.BinarySearch(ref, k)
		if s.has(k) != in {
			t.Fatalf("has(%v) = %v, want %v", k, !in, in)
		}
	}
}

func TestOrdsetMatchesSortedSlice(t *testing.T) {
	for seed := int64(1); seed <= 40; seed++ {
		r := rand.New(rand.NewSource(seed))
		s := &ordset[uint64]{}
		var ref []uint64
		span := uint64(10 + r.Intn(5000))
		var next uint64 // the next key in-order appends use
		for op := range 3000 {
			var k uint64
			switch mode := r.Intn(10); {
			case mode < 4: // appends past the end, as transfers arrive
				next += uint64(1 + r.Intn(3))
				k = next
			case mode < 7:
				k = uint64(r.Int63n(int64(span)))
			default:
				if len(ref) == 0 {
					continue
				}
				// Remove: mostly the oldest keys, as pending holds settle.
				if r.Intn(2) == 0 {
					k = ref[r.Intn(min(len(ref), 30))]
				} else {
					k = ref[r.Intn(len(ref))]
				}
				i, _ := slices.BinarySearch(ref, k)
				ref = slices.Delete(ref, i, i+1)
				if !s.remove(k) {
					t.Fatalf("seed %d op %d: remove(%d) found nothing", seed, op, k)
				}
				if s.remove(k) {
					t.Fatalf("seed %d op %d: removed %d twice", seed, op, k)
				}
				continue
			}
			i, found := slices.BinarySearch(ref, k)
			if !found {
				ref = slices.Insert(ref, i, k)
			}
			if s.insert(k) == found {
				t.Fatalf("seed %d op %d: insert(%d) reported new=%v", seed, op, k, found)
			}
			if op%97 == 0 {
				checkOrdset(t, r, s, ref, func() uint64 { return uint64(r.Int63n(int64(max(span, next) + 2))) })
			}
		}
		checkOrdset(t, r, s, ref, func() uint64 { return uint64(r.Int63n(int64(max(span, next) + 2))) })
		for len(ref) > 0 {
			k := ref[r.Intn(len(ref))]
			i, _ := slices.BinarySearch(ref, k)
			ref = slices.Delete(ref, i, i+1)
			s.remove(k)
		}
		checkOrdset(t, r, s, ref, func() uint64 { return uint64(r.Intn(10)) })
		if len(s.chunks) != 0 {
			t.Fatalf("seed %d: an empty set keeps %d chunks", seed, len(s.chunks))
		}
	}
}

func TestOrdsetStrings(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	s := &ordset[string]{}
	var ref []string
	word := func() string { return fmt.Sprintf("acct-%04d", r.Intn(3000)) }
	for op := range 4000 {
		k := word()
		i, found := slices.BinarySearch(ref, k)
		if r.Intn(4) == 0 {
			if found {
				ref = slices.Delete(ref, i, i+1)
			}
			if s.remove(k) != found {
				t.Fatalf("op %d: remove(%q) disagrees", op, k)
			}
			continue
		}
		if !found {
			ref = slices.Insert(ref, i, k)
		}
		s.insert(k)
	}
	checkOrdset(t, r, s, ref, word)
	// Removed string keys must not stay reachable from a chunk's spare
	// capacity.
	for _, c := range s.chunks {
		for _, k := range c[len(c):cap(c)] {
			if k != "" {
				t.Fatalf("a removed key %q is still referenced", k)
			}
		}
	}
}

func TestOrdsetPacking(t *testing.T) {
	s := &ordset[uint64]{}
	const n = 100_000
	for k := uint64(1); k <= n; k++ {
		s.insert(k)
	}
	if want := (n + ordChunk - 1) / ordChunk; len(s.chunks) != want {
		t.Fatalf("in-order appends left %d chunks, want %d full ones", len(s.chunks), want)
	}
	// Settling most of the oldest keys must not leave a trail of nearly
	// empty chunks.
	for k := uint64(1); k <= n; k++ {
		if k%50 != 0 {
			s.remove(k)
		}
	}
	if s.Len() != n/50 {
		t.Fatalf("len %d", s.Len())
	}
	if p := s.problem(); p != "" {
		t.Fatal(p)
	}
	if limit := 2 * (s.Len()/(ordChunk/4) + 1); len(s.chunks) > limit {
		t.Fatalf("%d keys are spread over %d chunks, want at most %d", s.Len(), len(s.chunks), limit)
	}
	// Inserting into the middle of full chunks splits them.
	s = &ordset[uint64]{}
	for k := uint64(0); k < 4*ordChunk; k++ {
		s.insert(2 * k)
	}
	for k := uint64(0); k < 4*ordChunk; k++ {
		s.insert(2*k + 1)
	}
	if p := s.problem(); p != "" || s.Len() != 8*ordChunk {
		t.Fatalf("after splits: %q, len %d", p, s.Len())
	}
}

func TestOrdsetProblem(t *testing.T) {
	cases := map[string]*ordset[uint64]{
		"empty chunk":   {chunks: [][]uint64{{1}, {}}, n: 1},
		"order":         {chunks: [][]uint64{{1, 3}, {2}}, n: 3},
		"duplicate":     {chunks: [][]uint64{{1, 1}}, n: 2},
		"count":         {chunks: [][]uint64{{1, 2}}, n: 3},
		"chunk too big": {chunks: [][]uint64{make([]uint64, ordChunk+1)}, n: ordChunk + 1},
	}
	for name, s := range cases {
		if s.problem() == "" {
			t.Errorf("%s: not reported", name)
		}
	}
}

// indexWorkload drives a ledger with accounts in several currencies with
// metadata, and transfers with references and metadata that are posted,
// held, captured, voided, expired and reversed.
func indexWorkload(t testing.TB, e *env, seed int64, n int) {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	ctx := context.Background()
	currencies := []string{"USD", "EUR", "JPY"}
	accts := map[string][]string{}
	for i := range 12 {
		cur := currencies[i%len(currencies)]
		id := fmt.Sprintf("%s-%02d", strings.ToLower(cur), i)
		spec := &AccountSpec{ID: id, Currency: cur, NormalBalance: Credit}
		if i%2 == 0 {
			spec.Metadata = map[string]string{"tier": []string{"gold", "silver"}[i%4/2], "region": "eu"}
		}
		e.do(Command{CreateAccount: spec})
		accts[cur] = append(accts[cur], id)
	}
	var pending, posted []string
	for i := range n {
		cur := currencies[r.Intn(len(currencies))]
		ids := accts[cur]
		from, to := ids[r.Intn(len(ids))], ids[r.Intn(len(ids))]
		if from == to {
			continue
		}
		var cmd Command
		switch k := r.Intn(10); {
		case k < 5:
			s := &TransferSpec{Currency: cur, Postings: []PostingSpec{dr(from, 1+r.Int63n(50)), cr(to, 0)}}
			s.Postings[1].Amount = s.Postings[0].Amount
			if r.Intn(3) == 0 {
				s.Reference = fmt.Sprintf("inv-%d", r.Intn(20))
			}
			if r.Intn(3) == 0 {
				s.Metadata = map[string]string{"order": fmt.Sprint(r.Intn(15)), "channel": "web"}
			}
			if r.Intn(3) == 0 {
				s.Pending, s.TimeoutSeconds = true, int64(1+r.Intn(30))
			}
			cmd = Command{CreateTransfer: s}
		case k < 7 && len(pending) > 0:
			id := pending[r.Intn(len(pending))]
			if r.Intn(2) == 0 {
				cmd = postCmd(id, 0)
			} else {
				cmd = voidCmd(id)
			}
		case k < 8 && len(posted) > 0:
			cmd = Command{Reverse: &ReverseSpec{TransferID: posted[r.Intn(len(posted))], Reference: fmt.Sprintf("refund-%d", i%5)}}
		default:
			e.clock.Advance(time.Duration(r.Intn(8)) * time.Second)
			if _, err := e.l.ExpireDue(ctx); err != nil {
				t.Fatal(err)
			}
			continue
		}
		out, err := e.l.Do(ctx, cmd, nil)
		if err != nil {
			continue // a refused command (expired hold, fully reversed transfer) changes nothing
		}
		if tr := out.Transfer; tr != nil {
			switch {
			case cmd.CreateTransfer != nil && tr.State == StatePending:
				pending = append(pending, tr.ID)
			case tr.State == StatePosted && tr.Reverses == "":
				posted = append(posted, tr.ID)
			}
		}
	}
	// End with an open hold in each currency, so every state is present.
	for _, cur := range currencies {
		ids := accts[cur]
		e.do(Command{CreateTransfer: &TransferSpec{Currency: cur, Pending: true, TimeoutSeconds: 3600,
			Reference: "open-hold", Postings: []PostingSpec{dr(ids[0], 1), cr(ids[1], 1)}}})
	}
}

// wantIndexes computes, through the public read API and a brute-force scan,
// what every index must hold, as transfer IDs and account IDs.
func wantIndexes(l *Ledger) map[string][]string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	want := map[string][]string{}
	for id, a := range l.st.accounts {
		want["account"] = append(want["account"], id)
		want["account currency "+a.Currency] = append(want["account currency "+a.Currency], id)
		for k, v := range a.Metadata {
			want["account meta "+k+"="+v] = append(want["account meta "+k+"="+v], id)
		}
	}
	for id, tr := range l.st.transfers {
		keys := []string{"transfer", "transfer state " + string(tr.State), "transfer currency " + tr.Currency}
		for _, p := range tr.Postings {
			keys = append(keys, "transfer account "+p.Account)
		}
		if tr.Reference != "" {
			keys = append(keys, "transfer reference "+tr.Reference)
		}
		for k, v := range tr.Metadata {
			keys = append(keys, "transfer meta "+k+"="+v)
		}
		for _, k := range keys {
			want[k] = append(want[k], id)
		}
	}
	for k := range want {
		sort.Strings(want[k])
	}
	return want
}

// gotIndexes reads the indexes back as the same IDs, checking that each
// transfer set comes out in creation order.
func gotIndexes(t testing.TB, l *Ledger) map[string][]string {
	t.Helper()
	l.mu.RLock()
	defer l.mu.RUnlock()
	got := map[string][]string{}
	ix := l.st.ix
	accts := func(name string, s *ordset[string]) {
		got[name] = s.keys()
	}
	xfers := func(name string, s *ordset[uint64]) {
		var prev uint64
		for seq := range s.ascend(0, true) {
			if seq <= prev {
				t.Fatalf("%s: %d after %d", name, seq, prev)
			}
			prev = seq
			tr, ok := l.st.transferAt(seq)
			if !ok {
				t.Fatalf("%s: entry %d did not create a transfer", name, seq)
			}
			got[name] = append(got[name], tr.ID)
		}
		sort.Strings(got[name])
	}
	accts("account", &ix.accounts)
	for k, s := range ix.acctCurrency {
		accts("account currency "+k, s)
	}
	for k, s := range ix.acctMeta {
		accts("account meta "+k.String(), s)
	}
	xfers("transfer", &ix.transfers)
	for k, s := range ix.xferState {
		xfers("transfer state "+string(k), s)
	}
	for k, s := range ix.xferCurrency {
		xfers("transfer currency "+k, s)
	}
	for k, s := range ix.xferAccount {
		xfers("transfer account "+k, s)
	}
	for k, s := range ix.xferRef {
		xfers("transfer reference "+k, s)
	}
	for k, s := range ix.xferMeta {
		xfers("transfer meta "+k.String(), s)
	}
	return got
}

func compareIndexes(t testing.TB, what string, l *Ledger) {
	t.Helper()
	want, got := wantIndexes(l), gotIndexes(t, l)
	for k, w := range want {
		if !slices.Equal(w, got[k]) {
			t.Fatalf("%s: index %s holds %v, want %v", what, k, got[k], w)
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			t.Fatalf("%s: index %s holds %v, want nothing", what, k, g)
		}
	}
	if v := l.CheckIndexes(); len(v) > 0 {
		t.Fatalf("%s: %v", what, v)
	}
}

func TestIndexesFollowEveryChange(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		e := newEnv(t, true)
		e.opts.SnapshotEvery = int(10 + seed*7)
		e.restart()
		indexWorkload(t, e, seed, 400)
		compareIndexes(t, fmt.Sprintf("seed %d live", seed), e.l)
		live := e.l

		// Every state must have appeared, or the test proves less than it
		// claims.
		live.mu.RLock()
		for _, s := range []TransferState{StatePending, StatePosted, StateVoided, StateExpired} {
			if live.st.ix.xferState[s].Len() == 0 {
				t.Fatalf("seed %d: no %s transfers", seed, s)
			}
		}
		if len(live.st.ix.xferRef) == 0 || len(live.st.ix.xferMeta) == 0 || len(live.st.ix.acctMeta) == 0 {
			t.Fatalf("seed %d: the workload set no references or metadata", seed)
		}
		live.mu.RUnlock()

		// The log alone, and the newest snapshot plus the log, must rebuild
		// the same indexes.
		if err := e.l.Close(); err != nil {
			t.Fatal(err)
		}
		for _, snaps := range []bool{false, true} {
			opts := e.opts
			opts.Dir = copyDir(t, e.dir, snaps)
			opts.SnapshotEvery = -1
			l, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			what := fmt.Sprintf("seed %d reopened (snapshots %v)", seed, snaps)
			compareIndexes(t, what, l)
			sameState(t, what, live, l)
			l.Close()
		}
	}
}

func TestSnapshotRebuildsIndexes(t *testing.T) {
	e := newEnv(t, true)
	indexWorkload(t, e, 42, 300)
	e.restart() // a clean shutdown writes a snapshot of everything
	if nums := snapshotFiles(t, e.dir); len(nums) == 0 {
		t.Fatal("no snapshot was written")
	}
	_, st, err := readSnapshot(wal.OSFS{}, newestSnapshot(t, e.dir))
	if err != nil {
		t.Fatal(err)
	}
	if v := checkIndexes(st); len(v) > 0 {
		t.Fatalf("snapshot indexes: %v", v)
	}
	e.l.mu.RLock()
	d := indexDiff(e.l.st.ix, st.ix)
	e.l.mu.RUnlock()
	if d != "" {
		t.Fatal(d)
	}
	// Transfers in a snapshot were stored in ID order; their index sets must
	// still be appended in creation order, packing chunks full.
	if n := st.ix.transfers.Len(); len(st.ix.transfers.chunks) != (n+ordChunk-1)/ordChunk {
		t.Fatalf("%d transfers rebuilt into %d chunks", n, len(st.ix.transfers.chunks))
	}
}

func newestSnapshot(t testing.TB, dir string) string {
	t.Helper()
	nums := snapshotFiles(t, dir)
	return dir + "/" + snapName(nums[0])
}

// TestBrokenIndexesAreCaught damages the indexes in each way a bug in their
// upkeep could, and checks that the full-scan check and the state comparison
// both notice.
func TestBrokenIndexesAreCaught(t *testing.T) {
	cases := map[string]struct {
		damage func(st *state)
		want   string
	}{
		"transfer left out of its state": {func(st *state) {
			s := st.ix.xferState[StatePosted]
			s.remove(s.keys()[0])
		}, "transfers by state posted is missing"},
		"state not moved": {func(st *state) {
			seq := st.ix.xferState[StateVoided].keys()[0]
			st.ix.xferState[StatePending].insert(seq)
		}, "transfers by state pending holds"},
		"stray account member": {func(st *state) {
			add(st.ix.xferAccount, "eur-01", st.ix.xferAccount["usd-00"].keys()[0])
		}, "transfers by account eur-01 holds"},
		"lost reference": {func(st *state) {
			for k := range st.ix.xferRef {
				delete(st.ix.xferRef, k)
				break
			}
		}, "transfers by reference"},
		"empty set kept": {func(st *state) {
			st.ix.xferRef["nothing"] = &ordset[uint64]{}
		}, "is empty but kept"},
		"metadata not indexed": {func(st *state) {
			s := st.ix.acctMeta[metaPair{"region", "eu"}]
			s.remove(s.keys()[0])
		}, "accounts by metadata region=eu is missing"},
		"account under the wrong currency": {func(st *state) {
			st.ix.acctCurrency["USD"].remove("usd-00")
			add(st.ix.acctCurrency, "JPY", "usd-00")
		}, "accounts by currency USD is missing usd-00"},
		"chunk out of order": {func(st *state) {
			c := st.ix.transfers.chunks[0]
			c[0], c[1] = c[1], c[0]
		}, "index transfers is damaged"},
		"count off": {func(st *state) {
			st.ix.accounts.n++
		}, "index accounts is damaged"},
	}
	for name, c := range cases {
		e := newEnv(t, false)
		indexWorkload(t, e, 3, 200)
		e.l.mu.Lock()
		before := scanIndexes(e.l.st)
		c.damage(e.l.st)
		e.l.mu.Unlock()
		v := e.l.CheckIndexes()
		if !strings.Contains(strings.Join(v, "\n"), c.want) {
			t.Errorf("%s: check reported %q, want %q", name, v, c.want)
		}
		e.l.mu.RLock()
		d := indexDiff(before, e.l.st.ix)
		e.l.mu.RUnlock()
		if d == "" {
			t.Errorf("%s: indexDiff found nothing", name)
		}
	}
}

func TestStateDiffComparesIndexes(t *testing.T) {
	e := newEnv(t, true)
	indexWorkload(t, e, 5, 150)
	e.restart()
	s, st, err := readSnapshot(wal.OSFS{}, newestSnapshot(t, e.dir))
	if err != nil {
		t.Fatal(err)
	}
	e.l.mu.Lock()
	defer e.l.mu.Unlock()
	if d := stateDiff(e.l, s, st); d != "" {
		t.Fatalf("an exact snapshot differs: %s", d)
	}
	st.ix.xferState[StatePosted].remove(st.ix.xferState[StatePosted].keys()[0])
	if d := stateDiff(e.l, s, st); !strings.Contains(d, "secondary indexes differ") {
		t.Fatalf("stateDiff = %q", d)
	}
}

func TestTransferAt(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 100), cr("alice", 100)))
	h := e.do(hold(60, dr("alice", 30), cr("bob", 30))).Transfer
	e.do(postCmd(h.ID, 0)) // entry 3 belongs to h but did not create it
	e.l.mu.RLock()
	defer e.l.mu.RUnlock()
	for seq, want := range map[uint64]string{1: "tr_000001", 2: h.ID} {
		if tr, ok := e.l.st.transferAt(seq); !ok || tr.ID != want {
			t.Errorf("transferAt(%d) = %v %v, want %s", seq, tr, ok, want)
		}
	}
	for _, seq := range []uint64{0, 3, 4} {
		if tr, ok := e.l.st.transferAt(seq); ok {
			t.Errorf("transferAt(%d) = %s", seq, tr.ID)
		}
	}
}

func TestSnapshotRefusesTransferWithoutEntries(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	e.do(move(dr("bank", 100), cr("alice", 100)))
	e.l.mu.RLock()
	s := e.l.capture(99, e.clock.Now())
	e.l.mu.RUnlock()
	bare := s.transfers[0].clone()
	bare.ID, bare.Entries = "tr_bare", nil
	s.transfers = append(s.transfers, bare)
	dir := t.TempDir()
	if _, err := writeSnapshot(wal.OSFS{}, dir, s); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSnapshot(wal.OSFS{}, dir+"/"+snapName(99)); err == nil || !strings.Contains(err.Error(), "lists no entries") {
		t.Fatalf("err = %v", err)
	}
}
