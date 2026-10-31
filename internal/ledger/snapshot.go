package ledger

// Snapshots make restarts fast. A snapshot is the whole committed state at
// one point in the log: accounts with their totals and versions, every
// transfer, the journal, and the idempotency keys still being kept. Open
// loads the newest valid snapshot and replays only the log records written
// after it, instead of re-running every command ever accepted.
//
// The log says where each snapshot belongs. When the writer decides to take
// one, it first appends a "snapshot" record carrying the snapshot's number
// and the journal head, fsynced like any other record, then copies the state
// as of that record and writes the copy in the background. Recovery skips the
// log up to the matching record and replays from there. A snapshot whose
// record is missing from the log is never trusted.
//
// A snapshot file is written to a temporary name, fsynced, renamed into place
// and the directory fsynced, so a crash leaves either the complete file or no
// file. The format is binary:
//
//	"LDGSNAP1"
//	frame*                        each: uint32 length, uint32 crc32c, payload (big endian)
//
// Frame payloads hold a sequence of items, each a tag byte and its fields
// (unsigned and signed varints, length-prefixed strings). Items never span
// frames. The first item is the header (format version, snapshot number,
// journal head, time, and the number of accounts, transfers, entries and keys
// that follow); then come the accounts, transfers, entries and keys, and last
// a footer with the number of frames before it. Anything else, including
// bytes after the footer, makes the file invalid.

import (
	"bufio"
	"container/heap"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/laiwf94/ledgerd/internal/money"
)

const (
	snapMagic      = "LDGSNAP1"
	snapVersion    = 1
	snapPrefix     = "snapshot-"
	snapSuffix     = ".snap"
	snapTmpSuffix  = ".tmp"
	snapFrameSize  = 64 << 10 // target payload per frame
	snapMaxFrame   = 4 << 20  // largest frame a reader accepts
	snapKeep       = 2        // snapshots kept on disk; older ones are removed
	snapItemHeader = 'H'
	snapItemAcct   = 'A'
	snapItemXfer   = 'T'
	snapItemEntry  = 'E'
	snapItemIdem   = 'I'
	snapItemFooter = 'Z'
)

var snapCRC = crc32.MakeTable(crc32.Castagnoli)

// errSnapshot marks a snapshot file that cannot be used.
var errSnapshot = errors.New("invalid snapshot")

// snapAccount is an account's definition and running totals. Its history rows
// are not stored: they are rebuilt from the journal.
type snapAccount struct {
	Account
	bal     Balance
	version uint64
}

type snapIdem struct {
	id string
	e  idemEntry
}

// snapshot is a copy of the committed state as of one log record.
type snapshot struct {
	num       uint64
	head      uint64
	time      time.Time
	accounts  []snapAccount
	transfers []*Transfer // committed transfers are immutable, so these are shared
	entries   []Entry     // a prefix of the journal, which is append-only
	idem      []snapIdem  // in the order they were stored
}

func snapName(num uint64) string { return fmt.Sprintf("%s%020d%s", snapPrefix, num, snapSuffix) }

// capture copies the state the writer has committed. Only the writer (or
// Close, after the writer has stopped) may call it. The copy costs one pass
// over accounts, transfers and keys; journal entries are shared.
func (l *Ledger) capture(num uint64, now time.Time) *snapshot {
	st := l.st
	s := &snapshot{num: num, head: st.head(), time: now,
		accounts:  make([]snapAccount, 0, len(st.accounts)),
		transfers: make([]*Transfer, 0, len(st.transfers)),
		entries:   st.entries[:len(st.entries):len(st.entries)]}
	for _, a := range st.accounts {
		s.accounts = append(s.accounts, snapAccount{Account: a.Account, bal: a.bal, version: a.version})
	}
	for _, tr := range st.transfers {
		s.transfers = append(s.transfers, tr)
	}
	l.idemMu.Lock()
	s.idem = make([]snapIdem, 0, len(l.idem))
	for _, q := range l.idemFIFO {
		if e, ok := l.idem[q.id]; ok && e.at.Equal(q.at) {
			s.idem = append(s.idem, snapIdem{id: q.id, e: *e})
		}
	}
	l.idemMu.Unlock()
	return s
}

// writeSnapshot stores s in dir and removes snapshots older than the newest
// snapKeep. It returns the size of the file.
func writeSnapshot(dir string, s *snapshot) (int64, error) {
	sort.Slice(s.accounts, func(i, j int) bool { return s.accounts[i].ID < s.accounts[j].ID })
	sort.Slice(s.transfers, func(i, j int) bool { return s.transfers[i].ID < s.transfers[j].ID })
	final := filepath.Join(dir, snapName(s.num))
	tmp := final + snapTmpSuffix
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	fail := func(err error) (int64, error) {
		f.Close()
		os.Remove(tmp)
		return 0, err
	}
	w := &snapWriter{w: bufio.NewWriterSize(f, 1<<20)}
	if _, err := w.w.WriteString(snapMagic); err != nil {
		return fail(err)
	}
	w.size = int64(len(snapMagic))
	if err := w.encode(s); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := syncDir(dir); err != nil {
		return 0, err
	}
	nums, err := listSnapshots(dir)
	if err != nil {
		return w.size, err
	}
	for _, n := range nums {
		if n < s.num && countNewer(nums, n) >= snapKeep {
			if err := os.Remove(filepath.Join(dir, snapName(n))); err != nil {
				return w.size, err
			}
		}
	}
	return w.size, nil
}

func countNewer(nums []uint64, n uint64) int {
	c := 0
	for _, m := range nums {
		if m > n {
			c++
		}
	}
	return c
}

// listSnapshots returns the numbers of the snapshot files in dir, newest
// first, and removes leftover temporary files.
func listSnapshots(dir string) ([]uint64, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var nums []uint64
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, snapPrefix) {
			continue
		}
		if strings.HasSuffix(name, snapSuffix+snapTmpSuffix) {
			os.Remove(filepath.Join(dir, name)) // an interrupted write; never renamed, so never used
			continue
		}
		digits, ok := strings.CutSuffix(strings.TrimPrefix(name, snapPrefix), snapSuffix)
		if !ok || len(digits) != 20 {
			continue
		}
		n, err := strconv.ParseUint(digits, 10, 64)
		if err != nil {
			continue
		}
		nums = append(nums, n)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] > nums[j] })
	return nums, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// snapWriter frames items as they are encoded.
type snapWriter struct {
	w      *bufio.Writer
	buf    []byte
	frames uint64
	size   int64
	err    error
}

func (w *snapWriter) flush() {
	if w.err != nil || len(w.buf) == 0 {
		return
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(w.buf)))
	binary.BigEndian.PutUint32(hdr[4:], crc32.Checksum(w.buf, snapCRC))
	if _, err := w.w.Write(hdr[:]); err != nil {
		w.err = err
		return
	}
	if _, err := w.w.Write(w.buf); err != nil {
		w.err = err
		return
	}
	w.size += int64(len(hdr) + len(w.buf))
	w.frames++
	w.buf = w.buf[:0]
}

// item ends an item, starting a new frame once the current one is full.
func (w *snapWriter) item() {
	if len(w.buf) >= snapFrameSize {
		w.flush()
	}
}

func (w *snapWriter) u(v uint64)   { w.buf = binary.AppendUvarint(w.buf, v) }
func (w *snapWriter) i(v int64)    { w.buf = binary.AppendVarint(w.buf, v) }
func (w *snapWriter) tag(t byte)   { w.buf = append(w.buf, t) }
func (w *snapWriter) str(s string) { w.u(uint64(len(s))); w.buf = append(w.buf, s...) }
func (w *snapWriter) flag(b bool)  { w.buf = append(w.buf, boolByte(b)) }
func (w *snapWriter) when(t time.Time) {
	if t.IsZero() {
		w.flag(false)
		return
	}
	w.flag(true)
	w.i(t.Unix())
	w.u(uint64(t.Nanosecond()))
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func (w *snapWriter) meta(m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w.u(uint64(len(keys)))
	for _, k := range keys {
		w.str(k)
		w.str(m[k])
	}
}

func (w *snapWriter) balance(b Balance) {
	w.i(b.DebitsPosted)
	w.i(b.CreditsPosted)
	w.i(b.DebitsPending)
	w.i(b.CreditsPending)
}

func (w *snapWriter) account(a *Account) {
	w.str(a.ID)
	w.str(a.Currency)
	w.i(int64(a.Exponent))
	w.str(string(a.NormalBalance))
	w.flag(a.NonNegative)
	w.flag(a.NonPositive)
	w.meta(a.Metadata)
	w.when(a.CreatedAt)
}

func (w *snapWriter) transfer(t *Transfer) {
	w.str(t.ID)
	w.str(string(t.State))
	w.str(t.Currency)
	w.u(uint64(len(t.Postings)))
	for _, p := range t.Postings {
		w.str(p.Account)
		w.str(string(p.Direction))
		w.i(p.Amount)
		w.i(p.Posted)
		w.i(p.Reversed)
	}
	w.str(t.Reference)
	w.meta(t.Metadata)
	w.when(t.CreatedAt)
	w.when(t.ExpiresAt)
	w.str(t.Reverses)
	w.u(uint64(len(t.Reversals)))
	for _, r := range t.Reversals {
		w.str(r)
	}
	w.u(uint64(len(t.Entries)))
	for _, e := range t.Entries {
		w.u(e.Seq)
		w.str(string(e.Kind))
	}
}

func (w *snapWriter) encode(s *snapshot) error {
	w.tag(snapItemHeader)
	w.u(snapVersion)
	w.u(s.num)
	w.u(s.head)
	w.when(s.time)
	w.u(uint64(len(s.accounts)))
	w.u(uint64(len(s.transfers)))
	w.u(uint64(len(s.entries)))
	w.u(uint64(len(s.idem)))
	w.item()
	for i := range s.accounts {
		a := &s.accounts[i]
		w.tag(snapItemAcct)
		w.account(&a.Account)
		w.balance(a.bal)
		w.u(a.version)
		w.item()
	}
	for _, t := range s.transfers {
		w.tag(snapItemXfer)
		w.transfer(t)
		w.item()
	}
	for i := range s.entries {
		e := &s.entries[i]
		w.tag(snapItemEntry)
		w.u(e.Seq)
		w.when(e.Time)
		w.str(string(e.Kind))
		w.str(e.TransferID)
		w.str(e.Currency)
		w.u(uint64(len(e.Lines)))
		for _, ln := range e.Lines {
			w.str(ln.Account)
			w.str(string(ln.Direction))
			w.i(ln.Posted)
			w.i(ln.Pending)
		}
		w.item()
	}
	for _, q := range s.idem {
		w.tag(snapItemIdem)
		w.str(q.id)
		w.buf = append(w.buf, q.e.hash[:]...)
		w.when(q.e.at)
		var flags byte
		if q.e.err != nil {
			flags |= 1
		}
		if q.e.out.Account != nil {
			flags |= 2
		}
		if q.e.out.Transfer != nil {
			flags |= 4
		}
		w.buf = append(w.buf, flags)
		if q.e.err != nil {
			w.str(string(q.e.err.Code))
			w.str(q.e.err.Message)
		}
		if a := q.e.out.Account; a != nil {
			w.account(&a.Account)
			w.u(a.Version)
			w.balance(a.Balance.Balance)
			w.u(a.AsOfSeq)
		}
		if t := q.e.out.Transfer; t != nil {
			w.transfer(t)
		}
		w.i(int64(q.e.out.Expired))
		w.item()
	}
	w.flush()
	w.tag(snapItemFooter)
	w.u(w.frames)
	w.flush()
	if w.err != nil {
		return w.err
	}
	return w.w.Flush()
}

// snapReader decodes one frame at a time. Every read is bounds-checked, so
// a damaged file is an error, never a panic or a huge allocation.
type snapReader struct {
	r      *bufio.Reader
	frame  []byte
	p      []byte // unread part of frame
	frames uint64
	err    error
	ids    map[string]string // interned account IDs
	syms   map[string]string // interned currency codes

	// Slabs the journal's lines and the transfers' postings and entry
	// references are carved from: a million entries cost a few hundred
	// allocations instead of a million.
	lines    []Line
	postings []Posting
	refs     []EntryRef
}

const snapSlab = 4096

func carve[T any](slab *[]T, n int) []T {
	if cap(*slab)-len(*slab) < n {
		*slab = make([]T, 0, max(snapSlab, n))
	}
	s := (*slab)[len(*slab) : len(*slab)+n : len(*slab)+n]
	*slab = (*slab)[:len(*slab)+n]
	return s
}

func (r *snapReader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("%w: %s", errSnapshot, fmt.Sprintf(format, args...))
	}
}

// next loads the next frame once the current one is used up. It reports
// false at a clean end of file.
func (r *snapReader) next() bool {
	if r.err != nil {
		return false
	}
	if len(r.p) > 0 {
		return true
	}
	var hdr [8]byte
	if _, err := io.ReadFull(r.r, hdr[:]); err != nil {
		if !errors.Is(err, io.EOF) {
			r.fail("truncated frame header")
		}
		return false
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n == 0 || n > snapMaxFrame {
		r.fail("frame %d has length %d", r.frames, n)
		return false
	}
	if cap(r.frame) < int(n) {
		r.frame = make([]byte, n)
	}
	r.frame = r.frame[:n]
	if _, err := io.ReadFull(r.r, r.frame); err != nil {
		r.fail("truncated frame %d", r.frames)
		return false
	}
	if crc32.Checksum(r.frame, snapCRC) != binary.BigEndian.Uint32(hdr[4:]) {
		r.fail("frame %d checksum mismatch", r.frames)
		return false
	}
	r.p = r.frame
	r.frames++
	return true
}

func (r *snapReader) byte1() byte {
	if r.err != nil || len(r.p) == 0 {
		r.fail("item cut short")
		return 0
	}
	b := r.p[0]
	r.p = r.p[1:]
	return b
}

func (r *snapReader) u() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.p)
	if n <= 0 {
		r.fail("bad unsigned varint")
		return 0
	}
	r.p = r.p[n:]
	return v
}

func (r *snapReader) i() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.p)
	if n <= 0 {
		r.fail("bad varint")
		return 0
	}
	r.p = r.p[n:]
	return v
}

// count reads a length that must fit in what is left of the frame, given
// each element takes at least min bytes.
func (r *snapReader) count(min int) int {
	n := r.u()
	if n > uint64(len(r.p)/min) {
		r.fail("count %d is larger than the frame", n)
		return 0
	}
	return int(n)
}

func (r *snapReader) raw() []byte {
	n := r.count(1)
	if r.err != nil {
		return nil
	}
	b := r.p[:n]
	r.p = r.p[n:]
	return b
}

func (r *snapReader) str() string { return string(r.raw()) }

// acct reads an account ID that must name a known account, and returns the
// shared copy of the string so the journal holds one copy per account.
func (r *snapReader) acct() string {
	b := r.raw()
	if s, ok := r.ids[string(b)]; ok {
		return s
	}
	r.fail("unknown account %q", b)
	return ""
}

func (r *snapReader) flag() bool {
	switch r.byte1() {
	case 0:
		return false
	case 1:
		return true
	}
	r.fail("bad flag")
	return false
}

func (r *snapReader) when() time.Time {
	if !r.flag() {
		return time.Time{}
	}
	sec := r.i()
	ns := r.u()
	if ns >= 1e9 {
		r.fail("bad nanoseconds")
	}
	return time.Unix(sec, int64(ns)).UTC()
}

// The enumerations decode to the package's own constants, so a million
// entries do not carry a million copies of "debit".

func (r *snapReader) dir() Direction {
	switch b := r.raw(); string(b) {
	case string(Debit):
		return Debit
	case string(Credit):
		return Credit
	default:
		r.fail("bad direction %q", b)
		return ""
	}
}

var (
	snapKinds  = []EntryKind{KindPosted, KindPending, KindPostPending, KindVoidPending, KindExpirePending, KindReversal}
	snapStates = []TransferState{StatePending, StatePosted, StateVoided, StateExpired}
)

func (r *snapReader) kind() EntryKind {
	b := r.raw()
	for _, k := range snapKinds {
		if string(b) == string(k) {
			return k
		}
	}
	r.fail("bad entry kind %q", b)
	return ""
}

func (r *snapReader) state() TransferState {
	b := r.raw()
	for _, s := range snapStates {
		if string(b) == string(s) {
			return s
		}
	}
	r.fail("bad transfer state %q", b)
	return ""
}

// sym reads a short string that repeats, such as a currency code, keeping
// one copy of each value.
func (r *snapReader) sym() string {
	b := r.raw()
	if s, ok := r.syms[string(b)]; ok {
		return s
	}
	if len(r.syms) >= 1024 {
		return string(b)
	}
	s := string(b)
	r.syms[s] = s
	return s
}

func (r *snapReader) meta() map[string]string {
	n := r.count(2)
	if n == 0 {
		return nil
	}
	m := make(map[string]string, n)
	for j := 0; j < n && r.err == nil; j++ {
		k := r.str()
		m[k] = r.str()
	}
	if len(m) != n {
		r.fail("duplicate metadata key")
	}
	return m
}

func (r *snapReader) balance() Balance {
	return Balance{DebitsPosted: r.i(), CreditsPosted: r.i(), DebitsPending: r.i(), CreditsPending: r.i()}
}

func (r *snapReader) account() Account {
	a := Account{ID: r.str(), Currency: r.sym(), Exponent: int(r.i()), NormalBalance: r.dir(),
		NonNegative: r.flag(), NonPositive: r.flag(), Metadata: r.meta(), CreatedAt: r.when()}
	if r.err == nil {
		if c, ok := money.Lookup(a.Currency); !ok || c.Exponent != a.Exponent || !ValidID(a.ID) {
			r.fail("bad account %q", a.ID)
		}
	}
	return a
}

func (r *snapReader) transfer() *Transfer {
	t := &Transfer{ID: r.str(), State: r.state(), Currency: r.sym()}
	if n := r.count(5); n > 0 {
		t.Postings = carve(&r.postings, n)
		for j := range t.Postings {
			t.Postings[j] = Posting{Account: r.acct(), Direction: r.dir(), Amount: r.i(), Posted: r.i(), Reversed: r.i()}
		}
	}
	t.Reference = r.str()
	t.Metadata = r.meta()
	t.CreatedAt = r.when()
	t.ExpiresAt = r.when()
	t.Reverses = r.str()
	if n := r.count(1); n > 0 {
		t.Reversals = make([]string, n)
		for j := range t.Reversals {
			t.Reversals[j] = r.str()
		}
	}
	if n := r.count(2); n > 0 {
		t.Entries = carve(&r.refs, n)
		for j := range t.Entries {
			t.Entries[j] = EntryRef{Seq: r.u(), Kind: r.kind()}
		}
	}
	return t
}

// itemTag starts the next item, loading a new frame if needed.
func (r *snapReader) itemTag() byte {
	if !r.next() {
		r.fail("file ends before the footer")
		return 0
	}
	return r.byte1()
}

func (r *snapReader) expect(tag byte) {
	if got := r.itemTag(); got != tag && r.err == nil {
		r.fail("found item %q where %q belongs", got, tag)
	}
}

// entryIndex finds seq in a transfer's entry references, which are few.
func entryIndex(t *Transfer, seq uint64) int {
	for i, ref := range t.Entries {
		if ref.Seq == seq {
			return i
		}
	}
	return 0 // unreachable: owner is only set from these references
}

// readSnapshot decodes and checks a snapshot file and builds the state it
// holds, including each account's history rows. The idempotency keys are
// returned in the order they were stored.
func readSnapshot(path string) (*snapshot, *state, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	r := &snapReader{r: bufio.NewReaderSize(f, 1<<20), ids: map[string]string{}, syms: map[string]string{}}
	var magic [len(snapMagic)]byte
	if _, err := io.ReadFull(r.r, magic[:]); err != nil || string(magic[:]) != snapMagic {
		return nil, nil, fmt.Errorf("%w: not a snapshot file", errSnapshot)
	}

	s := &snapshot{}
	r.expect(snapItemHeader)
	if v := r.u(); v != snapVersion && r.err == nil {
		r.fail("format version %d is not supported", v)
	}
	s.num, s.head, s.time = r.u(), r.u(), r.when()
	nAccounts, nTransfers, nEntries, nIdem := r.u(), r.u(), r.u(), r.u()
	if r.err == nil && nEntries != s.head {
		r.fail("head %d but %d entries", s.head, nEntries)
	}
	// Every item takes several bytes, so the file's size bounds the counts
	// and a damaged count cannot make the reader allocate without limit.
	if fi, err := f.Stat(); err == nil && r.err == nil && max(nAccounts, nTransfers, nEntries, nIdem) > uint64(fi.Size()) {
		r.fail("counts exceed the file size")
	}
	if r.err != nil {
		return nil, nil, r.err
	}

	st := newState()
	st.transfers = make(map[string]*Transfer, nTransfers)
	// owner[seq-1] is the transfer an entry belongs to; each entry is listed
	// by exactly one transfer.
	owner := make([]*Transfer, nEntries)
	for k := uint64(0); k < nAccounts && r.err == nil; k++ {
		r.expect(snapItemAcct)
		a := &account{Account: r.account(), bal: r.balance(), version: r.u()}
		if r.err != nil {
			break
		}
		if _, dup := st.accounts[a.ID]; dup {
			r.fail("account %s appears twice", a.ID)
			break
		}
		st.accounts[a.ID] = a
		r.ids[a.ID] = a.ID
	}
	for k := uint64(0); k < nTransfers && r.err == nil; k++ {
		r.expect(snapItemXfer)
		t := r.transfer()
		if r.err != nil {
			break
		}
		n := len(st.transfers)
		st.transfers[t.ID] = t
		if len(st.transfers) == n || !ValidID(t.ID) {
			r.fail("transfer %q appears twice or is not a valid ID", t.ID)
			break
		}
		for _, ref := range t.Entries {
			if ref.Seq == 0 || ref.Seq > nEntries || owner[ref.Seq-1] != nil {
				r.fail("transfer %s lists entry %d, which is out of range or listed twice", t.ID, ref.Seq)
				break
			}
			owner[ref.Seq-1] = t
		}
		if t.State == StatePending {
			st.open++
			st.expiries = append(st.expiries, expiry{at: t.ExpiresAt, id: t.ID})
		}
	}
	heap.Init(&st.expiries)

	// The journal. Each account's history and totals are rebuilt by folding
	// its entries, and must come out equal to what the snapshot recorded.
	folded := make(map[string]*Balance, len(st.accounts))
	if nEntries > 0 {
		st.entries = make([]Entry, 0, nEntries)
	}
	for k := uint64(0); k < nEntries && r.err == nil; k++ {
		r.expect(snapItemEntry)
		e := Entry{Seq: r.u(), Time: r.when(), Kind: r.kind()}
		tid := r.raw()
		e.Currency = r.sym()
		if r.err != nil {
			break
		}
		tr := owner[k]
		if e.Seq != k+1 || tr == nil || string(tid) != tr.ID || tr.Entries[entryIndex(tr, e.Seq)].Kind != e.Kind {
			r.fail("entry %d does not match the transfer that lists it", k+1)
			break
		}
		e.TransferID = tr.ID
		n := r.count(4)
		if r.err == nil && n == 0 {
			r.fail("entry %d has no lines", e.Seq)
		}
		e.Lines = carve(&r.lines, n)
		for j := range e.Lines {
			ln := Line{Account: r.acct(), Direction: r.dir(), Posted: r.i(), Pending: r.i()}
			if r.err != nil {
				break
			}
			e.Lines[j] = ln
			b := folded[ln.Account]
			if b == nil {
				b = &Balance{}
				folded[ln.Account] = b
			}
			nb, ferr := b.with(ln)
			if ferr != nil {
				r.fail("entry %d: %v", e.Seq, ferr)
				break
			}
			*b = nb
			a := st.accounts[ln.Account]
			a.rows = append(a.rows, row{seq: e.Seq, line: j, balance: nb})
		}
		st.entries = append(st.entries, e)
	}
	if r.err == nil {
		for id, a := range st.accounts {
			var f Balance
			if b := folded[id]; b != nil {
				f = *b
			}
			if f != a.bal || a.version != uint64(len(a.rows)) {
				r.fail("account %s totals or version do not match its journal entries", id)
				break
			}
		}
	}

	s.idem = make([]snapIdem, 0, min(nIdem, 1<<20))
	for k := uint64(0); k < nIdem && r.err == nil; k++ {
		r.expect(snapItemIdem)
		var q snapIdem
		q.id = r.str()
		if r.err == nil && len(r.p) < len(q.e.hash) {
			r.fail("idempotency key cut short")
			break
		}
		copy(q.e.hash[:], r.p)
		r.p = r.p[len(q.e.hash):]
		q.e.at = r.when()
		flags := r.byte1()
		if flags&^7 != 0 {
			r.fail("bad idempotency flags")
			break
		}
		if flags&1 != 0 {
			q.e.err = &Error{Code: Code(r.sym()), Message: r.str()}
		}
		if flags&2 != 0 {
			as := &AccountState{Account: r.account()}
			as.Version = r.u()
			b := r.balance()
			as.Balance = b.View(as.NormalBalance)
			as.AsOfSeq = r.u()
			q.e.out.Account = as
		}
		if flags&4 != 0 {
			q.e.out.Transfer = r.transfer()
		}
		q.e.out.Expired = int(r.i())
		s.idem = append(s.idem, q)
	}

	r.expect(snapItemFooter)
	frames := r.frames - 1 // the footer's own frame
	if got := r.u(); r.err == nil && got != frames {
		r.fail("footer counts %d frames, file has %d", got, frames)
	}
	if r.err == nil && len(r.p) > 0 {
		r.fail("trailing data in the footer")
	}
	if r.err == nil {
		if _, err := r.r.ReadByte(); err != io.EOF {
			r.fail("data after the footer")
		}
	}
	if r.err != nil {
		return nil, nil, r.err
	}
	return s, st, nil
}
