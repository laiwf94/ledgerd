package ledger

// Secondary indexes let reads find accounts and transfers by something other
// than their ID: accounts by currency and metadata pair, transfers by state,
// currency, account, reference and metadata pair. Each index maps a key to an
// ordered set of members:
//
//   - accounts are ordered by ID;
//   - transfers are ordered by the sequence number of the journal entry that
//     created them, which is unique (each transfer is created by exactly one
//     posted, pending or reversal entry), never changes, and is the same
//     after a restart. A member costs 8 bytes; the journal entry at that
//     sequence names the transfer.
//
// The indexes are part of the committed state. txn.commit updates them with
// everything it publishes, under the write lock, so a reader holding the read
// lock always sees indexes that agree with the accounts and transfers. Replay
// goes through the same commit, so the log rebuilds them too.
//
// Snapshots do not store the indexes. readSnapshot rebuilds them from the
// accounts and transfers it has just read, in one pass: accounts come in ID
// order and transfers are visited in journal order, so every member is
// appended at the end of its set and nothing is sorted. A stored index could
// not be trusted without checking each member against its record, which is
// the same pass. This is the same choice snapshots make for history rows.
//
// checkIndexes compares the indexes with ones built by a full scan.

import (
	"cmp"
	"fmt"
	"iter"
	"slices"
	"sort"
	"sync"
)

// ordChunk is the most keys an ordset chunk holds. Inserting or removing a
// key moves at most this many keys, whatever the size of the set.
const ordChunk = 256

// ordset is an ordered set of keys kept in sorted chunks of at most ordChunk
// keys each. Appending a key larger than every other, the common case for
// transfers, fills the last chunk and then starts a new one, so sets built in
// order are packed full. It is not safe for concurrent use; the ledger's lock
// guards it.
type ordset[K cmp.Ordered] struct {
	chunks [][]K // non-empty, sorted, and every key is less than the next chunk's first key
	n      int
}

// Len returns the number of keys. A nil set is empty.
func (s *ordset[K]) Len() int {
	if s == nil {
		return 0
	}
	return s.n
}

// chunkFor returns the index of the first chunk whose last key is at least k,
// or len(s.chunks) when k is larger than every key.
func (s *ordset[K]) chunkFor(k K) int {
	return sort.Search(len(s.chunks), func(i int) bool { c := s.chunks[i]; return c[len(c)-1] >= k })
}

// insert adds k and reports whether it was new.
func (s *ordset[K]) insert(k K) bool {
	if nc := len(s.chunks); nc == 0 || k > s.chunks[nc-1][len(s.chunks[nc-1])-1] {
		// Larger than every key: append to the last chunk, or start a new
		// one when it is full. A set that has filled a chunk is likely to
		// fill the next, so that one is allocated whole; small sets grow
		// as they need to.
		switch {
		case nc == 0:
			s.chunks = append(s.chunks, append(make([]K, 0, 4), k))
		case len(s.chunks[nc-1]) == ordChunk:
			s.chunks = append(s.chunks, append(make([]K, 0, ordChunk), k))
		default:
			s.chunks[nc-1] = append(s.chunks[nc-1], k)
		}
		s.n++
		return true
	}
	ci := s.chunkFor(k) // < len(s.chunks): k is not past the last key
	c := s.chunks[ci]
	i, found := slices.BinarySearch(c, k)
	if found {
		return false
	}
	if len(c) == ordChunk {
		// Split the full chunk in two and insert into the half k belongs to.
		half := ordChunk / 2
		right := make([]K, ordChunk-half, ordChunk)
		copy(right, c[half:])
		clear(c[half:]) // the keys moved to right; a string key must not stay reachable from here too
		left := c[:half]
		s.chunks[ci] = left
		s.chunks = slices.Insert(s.chunks, ci+1, right)
		if i <= half {
			s.chunks[ci] = slices.Insert(left, i, k)
		} else {
			s.chunks[ci+1] = slices.Insert(right, i-half, k)
		}
	} else {
		s.chunks[ci] = slices.Insert(c, i, k)
	}
	s.n++
	return true
}

// remove deletes k and reports whether it was there. A chunk that falls
// below a quarter full is merged into a neighbour when the two fit in one.
func (s *ordset[K]) remove(k K) bool {
	ci := s.chunkFor(k)
	if ci == len(s.chunks) {
		return false
	}
	c := s.chunks[ci]
	i, found := slices.BinarySearch(c, k)
	if !found {
		return false
	}
	c = slices.Delete(c, i, i+1) // clears the vacated slot, so a string key is not kept alive
	s.n--
	switch {
	case len(c) == 0:
		s.chunks = slices.Delete(s.chunks, ci, ci+1)
		return true
	case len(c) >= ordChunk/4:
		s.chunks[ci] = c
		return true
	}
	s.chunks[ci] = c
	if ci+1 < len(s.chunks) && len(c)+len(s.chunks[ci+1]) <= ordChunk {
		s.chunks[ci] = append(c, s.chunks[ci+1]...)
		s.chunks = slices.Delete(s.chunks, ci+1, ci+2)
	} else if ci > 0 && len(c)+len(s.chunks[ci-1]) <= ordChunk {
		s.chunks[ci-1] = append(s.chunks[ci-1], c...)
		s.chunks = slices.Delete(s.chunks, ci, ci+1)
	}
	return true
}

// has reports whether k is in the set.
func (s *ordset[K]) has(k K) bool {
	ci := s.chunkFor(k)
	if ci == len(s.chunks) {
		return false
	}
	_, found := slices.BinarySearch(s.chunks[ci], k)
	return found
}

// ascend yields the keys greater than after in increasing order, or every
// key when all is true.
func (s *ordset[K]) ascend(after K, all bool) iter.Seq[K] {
	return func(yield func(K) bool) {
		if s == nil {
			return
		}
		ci, i := 0, 0
		if !all {
			ci = s.chunkFor(after)
			if ci < len(s.chunks) {
				i, _ = slices.BinarySearch(s.chunks[ci], after)
				if i < len(s.chunks[ci]) && s.chunks[ci][i] == after {
					i++
				}
			}
		}
		for ; ci < len(s.chunks); ci, i = ci+1, 0 {
			for _, k := range s.chunks[ci][i:] {
				if !yield(k) {
					return
				}
			}
		}
	}
}

// descend yields the keys less than before in decreasing order, or every key
// when all is true.
func (s *ordset[K]) descend(before K, all bool) iter.Seq[K] {
	return func(yield func(K) bool) {
		if s == nil || len(s.chunks) == 0 {
			return
		}
		ci := len(s.chunks) - 1
		i := len(s.chunks[ci]) // keys [0, i) of chunk ci are left to yield
		if !all {
			ci = s.chunkFor(before)
			if ci == len(s.chunks) {
				ci--
				i = len(s.chunks[ci])
			} else {
				i, _ = slices.BinarySearch(s.chunks[ci], before)
			}
		}
		for ; ci >= 0; ci-- {
			c := s.chunks[ci]
			if i < 0 {
				i = len(c)
			}
			for j := i - 1; j >= 0; j-- {
				if !yield(c[j]) {
					return
				}
			}
			i = -1
		}
	}
}

// keys returns every key in order.
func (s *ordset[K]) keys() []K {
	if s == nil {
		return nil
	}
	out := make([]K, 0, s.n)
	for _, c := range s.chunks {
		out = append(out, c...)
	}
	return out
}

// problem describes how the set's structure is broken, or returns "".
func (s *ordset[K]) problem() string {
	n := 0
	var prev K
	for ci, c := range s.chunks {
		if len(c) == 0 || len(c) > ordChunk {
			return fmt.Sprintf("chunk %d holds %d keys", ci, len(c))
		}
		for i, k := range c {
			if (ci > 0 || i > 0) && k <= prev {
				return fmt.Sprintf("key %v is out of order after %v", k, prev)
			}
			prev = k
		}
		n += len(c)
	}
	if n != s.n {
		return fmt.Sprintf("counts %d keys but holds %d", s.n, n)
	}
	return ""
}

// metaPair is a metadata key and value.
type metaPair struct{ key, value string }

func (p metaPair) String() string { return p.key + "=" + p.value }

// indexes are the ledger's secondary indexes. Account members are IDs;
// transfer members are creating-entry sequence numbers.
type indexes struct {
	accounts     ordset[string]
	acctCurrency map[string]*ordset[string]
	acctMeta     map[metaPair]*ordset[string]

	transfers    ordset[uint64]
	xferState    map[TransferState]*ordset[uint64]
	xferCurrency map[string]*ordset[uint64]
	xferAccount  map[string]*ordset[uint64]
	xferRef      map[string]*ordset[uint64]
	xferMeta     map[metaPair]*ordset[uint64]
}

func newIndexes() *indexes {
	return &indexes{
		acctCurrency: map[string]*ordset[string]{},
		acctMeta:     map[metaPair]*ordset[string]{},
		xferState:    map[TransferState]*ordset[uint64]{},
		xferCurrency: map[string]*ordset[uint64]{},
		xferAccount:  map[string]*ordset[uint64]{},
		xferRef:      map[string]*ordset[uint64]{},
		xferMeta:     map[metaPair]*ordset[uint64]{},
	}
}

// add puts k into the set for key, creating the set on first use.
func add[M comparable, K cmp.Ordered](m map[M]*ordset[K], key M, k K) {
	s := m[key]
	if s == nil {
		s = &ordset[K]{}
		m[key] = s
	}
	s.insert(k)
}

// drop takes k out of the set for key, and the set out of the map once it is
// empty, so keys nothing uses any more cost nothing.
func drop[M comparable, K cmp.Ordered](m map[M]*ordset[K], key M, k K) {
	s := m[key]
	if s == nil {
		return
	}
	s.remove(k)
	if s.n == 0 {
		delete(m, key)
	}
}

// createdAt returns the sequence of the entry that created a transfer.
func createdAt(tr *Transfer) uint64 { return tr.Entries[0].Seq }

func (ix *indexes) addAccount(a *Account) {
	ix.accounts.insert(a.ID)
	add(ix.acctCurrency, a.Currency, a.ID)
	for k, v := range a.Metadata {
		add(ix.acctMeta, metaPair{k, v}, a.ID)
	}
}

func (ix *indexes) addTransfer(tr *Transfer) {
	ix.addTransferKind(tr)
	ix.addTransferAccounts(tr)
	ix.addTransferLabels(tr)
}

// The transfer indexes fall into three groups that share no sets, so
// addTransfers can fill them at the same time.

func (ix *indexes) addTransferKind(tr *Transfer) {
	seq := createdAt(tr)
	ix.transfers.insert(seq)
	add(ix.xferState, tr.State, seq)
	add(ix.xferCurrency, tr.Currency, seq)
}

func (ix *indexes) addTransferAccounts(tr *Transfer) {
	seq := createdAt(tr)
	for _, p := range tr.Postings {
		add(ix.xferAccount, p.Account, seq)
	}
}

func (ix *indexes) addTransferLabels(tr *Transfer) {
	seq := createdAt(tr)
	if tr.Reference != "" {
		add(ix.xferRef, tr.Reference, seq)
	}
	for k, v := range tr.Metadata {
		add(ix.xferMeta, metaPair{k, v}, seq)
	}
}

// addTransfers indexes transfers given in creation order, filling the three
// groups of indexes on separate goroutines. Loading a snapshot uses it.
func (ix *indexes) addTransfers(trs []*Transfer) {
	var wg sync.WaitGroup
	for _, fill := range []func(*Transfer){ix.addTransferKind, ix.addTransferAccounts, ix.addTransferLabels} {
		wg.Go(func() {
			for _, tr := range trs {
				fill(tr)
			}
		})
	}
	wg.Wait()
}

// updateTransfer reflects a new version of a transfer. Only the state of a
// committed transfer can change; its currency, postings' accounts,
// reference and metadata are fixed when it is created.
func (ix *indexes) updateTransfer(old, tr *Transfer) {
	if old.State != tr.State {
		seq := createdAt(tr)
		drop(ix.xferState, old.State, seq)
		add(ix.xferState, tr.State, seq)
	}
}

// transferAt returns the transfer created by the journal entry at seq.
func (st *state) transferAt(seq uint64) (*Transfer, bool) {
	if seq == 0 || seq > st.head() {
		return nil, false
	}
	tr, ok := st.transfers[st.entries[seq-1].TransferID]
	if !ok || createdAt(tr) != seq {
		return nil, false
	}
	return tr, true
}

// scanIndexes builds the indexes from scratch from the accounts and
// transfers, in map order, so it shares nothing with the in-order paths.
func scanIndexes(st *state) *indexes {
	ix := newIndexes()
	for _, a := range st.accounts {
		ix.addAccount(&a.Account)
	}
	for _, tr := range st.transfers {
		ix.addTransfer(tr)
	}
	return ix
}

// checkIndexes compares st's indexes with a full scan and describes each
// difference, at most maxViolations of them. The caller must hold a lock
// that keeps st from changing.
func checkIndexes(st *state) []string {
	var out []string
	bad := func(format string, args ...any) {
		if len(out) < maxViolations {
			out = append(out, fmt.Sprintf(format, args...))
		}
	}
	diffIndexes(bad, scanIndexes(st), st.ix)
	return out
}

// diffIndexes reports through bad every difference between the indexes a
// full scan built (want) and the ones being checked (got).
func diffIndexes(bad func(string, ...any), want, got *indexes) {
	compareSet(bad, "accounts", &want.accounts, &got.accounts)
	compareMap(bad, "accounts by currency", want.acctCurrency, got.acctCurrency)
	compareMap(bad, "accounts by metadata", want.acctMeta, got.acctMeta)
	compareSet(bad, "transfers", &want.transfers, &got.transfers)
	compareMap(bad, "transfers by state", want.xferState, got.xferState)
	compareMap(bad, "transfers by currency", want.xferCurrency, got.xferCurrency)
	compareMap(bad, "transfers by account", want.xferAccount, got.xferAccount)
	compareMap(bad, "transfers by reference", want.xferRef, got.xferRef)
	compareMap(bad, "transfers by metadata", want.xferMeta, got.xferMeta)
}

func compareMap[M comparable, K cmp.Ordered](bad func(string, ...any), name string, want, got map[M]*ordset[K]) {
	for key, w := range want {
		compareSet(bad, fmt.Sprintf("%s %v", name, key), w, got[key])
	}
	for key, g := range got {
		if _, ok := want[key]; !ok {
			if g.n == 0 {
				bad("index %s %v is empty but kept", name, key)
			} else {
				compareSet(bad, fmt.Sprintf("%s %v", name, key), nil, g)
			}
		}
	}
}

func compareSet[K cmp.Ordered](bad func(string, ...any), name string, want, got *ordset[K]) {
	if got != nil {
		if p := got.problem(); p != "" {
			bad("index %s is damaged: %s", name, p)
			return
		}
	}
	w, g := want.keys(), got.keys()
	i, j := 0, 0
	for i < len(w) || j < len(g) {
		switch {
		case j == len(g) || (i < len(w) && w[i] < g[j]):
			bad("index %s is missing %v", name, w[i])
			i++
		case i == len(w) || g[j] < w[i]:
			bad("index %s holds %v, which does not belong", name, g[j])
			j++
		default:
			i, j = i+1, j+1
		}
	}
}

// indexDiff describes the first difference between two sets of indexes, or
// returns "".
func indexDiff(a, b *indexes) string {
	var first string
	bad := func(format string, args ...any) {
		if first == "" {
			first = fmt.Sprintf(format, args...)
		}
	}
	diffIndexes(bad, a, b)
	return first
}

// CheckIndexes compares every secondary index with a full scan of the
// accounts and transfers and describes each difference (empty when they
// agree). It holds the read lock for the whole scan, so commits wait for it;
// it is meant for tests and offline checks, not for a busy server.
func (l *Ledger) CheckIndexes() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return checkIndexes(l.st)
}
