// Package faultfs is an in-memory file system that loses what a real disk
// may lose when the power goes: everything that was not synced.
//
// It keeps two versions of everything. Reads see the current one. The
// durable one is what a file held when it was last synced and which entries
// a directory held when it was last synced with SyncDir. Writes, truncations
// and directory changes made since then are kept as a list of pending
// operations, in the order they were made.
//
// PowerCut, or an operation budget set with CutAfter running out, turns the
// power off. From then on every call fails with ErrPowerLost, and PowerCut
// returns a new FS holding what came back: for each file its durable bytes
// plus a random choice of its pending writes, and for each directory its
// durable entries plus a random choice of its pending changes. The choice is
// made per file and per directory, from these:
//
//   - none of the pending operations survive
//   - all of them survive
//   - a prefix survives, and its last write may be torn: only a prefix of
//     its bytes reaches the disk
//   - a random subset survives, in the order it was made
//   - a random subset survives, applied in a random order (writes reaching
//     the disk out of order); files only, since a rename applied before
//     the creation it depends on would leave one file under two names,
//     which no file system does
//
// That is everything POSIX allows except a single write torn into pieces
// that are not a prefix (sectors persisted out of order within one write).
// The ledger's log assumes a torn write leaves a prefix, as ext4 and XFS do
// for appends.
//
// A rename is one operation: after a cut the new name refers to either the
// old file or the new one, never to nothing. Hard links share one file.
// Locks live in memory and do not survive a cut.
//
// The random choices come from the seed given to New, so a run with the
// same seed and the same sequence of calls loses the same things.
package faultfs

import (
	"errors"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
)

// ErrPowerLost is returned by every call once the power is off.
var ErrPowerLost = errors.New("faultfs: the power is off")

// Mode is how a cut treats one file's or one directory's pending operations.
type Mode int

const (
	DropAll   Mode = iota // nothing pending survives
	KeepAll               // everything survives
	TornTail              // a prefix survives; its last write may be cut short
	Subset                // a random subset survives, in order
	Reordered             // a random subset survives, applied in a random order
	numModes
)

func (m Mode) String() string {
	return [...]string{"drop-all", "keep-all", "torn-tail", "subset", "reordered"}[m]
}

// Stats counts what a power cut did.
type Stats struct {
	// Files and Dirs count the files and directories that had pending
	// operations, by the mode chosen for them. Directories are never
	// Reordered.
	Files, Dirs [numModes]int
	// Writes and DirOps count pending operations; the Lost counts are
	// those that did not survive.
	Writes, LostWrites int
	DirOps, LostDirOps int
	// TornWrites counts writes that survived only in part.
	TornWrites int
	// OutOfOrder counts files whose surviving writes were applied in an
	// order other than the one they were made in.
	OutOfOrder int
}

// Add adds o to s.
func (s *Stats) Add(o Stats) {
	for i := range s.Files {
		s.Files[i] += o.Files[i]
		s.Dirs[i] += o.Dirs[i]
	}
	s.Writes += o.Writes
	s.LostWrites += o.LostWrites
	s.DirOps += o.DirOps
	s.LostDirOps += o.LostDirOps
	s.TornWrites += o.TornWrites
	s.OutOfOrder += o.OutOfOrder
}

type write struct {
	off   int64
	data  []byte
	trunc bool // a truncation to off; data is unused
}

type opKind int

const (
	opAdd opKind = iota // name -> n, replacing any entry
	opRemove
	opRename // from -> name, atomically
)

type dirOp struct {
	kind opKind
	name string
	from string
	n    *node
}

type node struct {
	ino   uint64
	isDir bool
	mode  fs.FileMode

	data    []byte  // what reads see
	durable []byte  // as of the last sync
	pending []write // since the last sync, oldest first

	ents    map[string]*node
	durEnts map[string]*node
	dirOps  []dirOp
}

// FS is a file system in memory. It is safe for concurrent use.
type FS struct {
	mu     sync.Mutex
	rng    *rand.Rand
	root   *node
	inos   uint64
	locks  map[*node]bool
	off    bool
	budget int // mutating operations left before the power goes; -1 for no limit
	image  *FS
	cut    Stats // what the cut that produced this FS did
}

// New returns an empty file system whose power cuts are driven by seed.
func New(seed int64) *FS {
	f := &FS{rng: rand.New(rand.NewSource(seed)), locks: map[*node]bool{}, budget: -1}
	f.root = f.newNode(true, fs.ModeDir|0o755)
	f.root.durEnts = map[string]*node{}
	return f
}

var _ wal.FS = (*FS)(nil)

func (f *FS) newNode(dir bool, mode fs.FileMode) *node {
	f.inos++
	n := &node{ino: f.inos, isDir: dir, mode: mode}
	if dir {
		n.ents = map[string]*node{}
		n.durEnts = map[string]*node{}
	}
	return n
}

// CutAfter turns the power off when n more operations that change the disk
// (writes, truncations, syncs, creations, removals, renames, links and
// directory syncs) have run: the next one after them fails with
// ErrPowerLost. A negative n removes the limit.
func (f *FS) CutAfter(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.budget = n
}

// Off reports whether the power is off.
func (f *FS) Off() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.off
}

// PowerCut turns the power off, if it is not off already, and returns the
// file system as it comes back. Open files and locks of f are dead.
func (f *FS) PowerCut() *FS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powerOff()
	return f.image
}

// CutStats returns what the power cut that produced f lost. It is zero for
// a file system made by New.
func (f *FS) CutStats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cut
}

func (f *FS) powerOff() {
	if f.off {
		return
	}
	f.off = true
	img := &FS{rng: rand.New(rand.NewSource(f.rng.Int63())), locks: map[*node]bool{}, budget: -1, inos: f.inos}
	c := &cutter{rng: f.rng, done: map[*node]*node{}, stats: &img.cut}
	img.root = c.node(f.root)
	f.image = img
}

// step is called, holding mu, before every operation that changes the disk.
func (f *FS) step() error {
	if f.off {
		return ErrPowerLost
	}
	if f.budget == 0 {
		f.powerOff()
		return ErrPowerLost
	}
	if f.budget > 0 {
		f.budget--
	}
	return nil
}

// cutter builds what survives a cut.
type cutter struct {
	rng   *rand.Rand
	done  map[*node]*node // hard links share one surviving file
	stats *Stats
}

func (c *cutter) node(n *node) *node {
	if m, ok := c.done[n]; ok {
		return m
	}
	m := &node{ino: n.ino, isDir: n.isDir, mode: n.mode}
	c.done[n] = m
	if n.isDir {
		ents := c.dir(n)
		m.ents = make(map[string]*node, len(ents))
		m.durEnts = make(map[string]*node, len(ents))
		for _, name := range sortedNames(ents) {
			child := c.node(ents[name])
			m.ents[name], m.durEnts[name] = child, child
		}
		return m
	}
	m.data = c.file(n)
	m.durable = append([]byte(nil), m.data...)
	return m
}

func (c *cutter) dir(n *node) map[string]*node {
	ents := make(map[string]*node, len(n.durEnts))
	for k, v := range n.durEnts {
		ents[k] = v
	}
	if len(n.dirOps) == 0 {
		return ents
	}
	mode := Mode(c.rng.Intn(int(Reordered)))
	c.stats.Dirs[mode]++
	ops := c.choose(mode, len(n.dirOps))
	c.stats.DirOps += len(n.dirOps)
	c.stats.LostDirOps += len(n.dirOps) - len(ops)
	for _, i := range ops {
		op := n.dirOps[i]
		switch op.kind {
		case opAdd:
			ents[op.name] = op.n
		case opRemove:
			delete(ents, op.name)
		case opRename:
			delete(ents, op.from)
			ents[op.name] = op.n
		}
	}
	return ents
}

func (c *cutter) file(n *node) []byte {
	data := append([]byte(nil), n.durable...)
	if len(n.pending) == 0 {
		return data
	}
	mode := Mode(c.rng.Intn(int(numModes)))
	c.stats.Files[mode]++
	ops := c.choose(mode, len(n.pending))
	c.stats.Writes += len(n.pending)
	c.stats.LostWrites += len(n.pending) - len(ops)
	for i := 1; i < len(ops); i++ {
		if ops[i] < ops[i-1] {
			c.stats.OutOfOrder++
			break
		}
	}
	for k, i := range ops {
		w := n.pending[i]
		if w.trunc {
			data = resize(data, w.off)
			continue
		}
		b := w.data
		if mode == TornTail && k == len(ops)-1 && len(b) > 1 && c.rng.Intn(2) == 0 {
			b = b[:1+c.rng.Intn(len(b)-1)]
			c.stats.TornWrites++
		}
		end := w.off + int64(len(b))
		if int64(len(data)) < end {
			data = resize(data, end)
		}
		copy(data[w.off:], b)
	}
	return data
}

// choose returns the indexes of the pending operations that survive, in the
// order they are applied.
func (c *cutter) choose(mode Mode, n int) []int {
	var out []int
	switch mode {
	case DropAll:
	case KeepAll:
		for i := 0; i < n; i++ {
			out = append(out, i)
		}
	case TornTail:
		k := c.rng.Intn(n + 1)
		for i := 0; i < k; i++ {
			out = append(out, i)
		}
	case Subset, Reordered:
		for i := 0; i < n; i++ {
			if c.rng.Intn(2) == 0 {
				out = append(out, i)
			}
		}
		if mode == Reordered {
			c.rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		}
	}
	return out
}

func resize(b []byte, size int64) []byte {
	if int64(len(b)) >= size {
		return b[:size]
	}
	return append(b, make([]byte, size-int64(len(b)))...)
}

func sortedNames(m map[string]*node) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// split cleans name and returns its parent directory's path and its base.
func split(name string) (string, string) {
	p := path.Clean("/" + name)
	return path.Dir(p), path.Base(p)
}

// lookup resolves a path. It holds mu.
func (f *FS) lookup(op, name string) (*node, error) {
	n := f.root
	for _, part := range strings.Split(strings.Trim(path.Clean("/"+name), "/"), "/") {
		if part == "" {
			continue
		}
		if !n.isDir {
			return nil, &fs.PathError{Op: op, Path: name, Err: errors.New("not a directory")}
		}
		next, ok := n.ents[part]
		if !ok {
			return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
		}
		n = next
	}
	return n, nil
}

// parent resolves the directory that holds name, and name's base.
func (f *FS) parent(op, name string) (*node, string, error) {
	dir, base := split(name)
	if base == "/" {
		return nil, "", &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	d, err := f.lookup(op, dir)
	if err != nil {
		return nil, "", err
	}
	if !d.isDir {
		return nil, "", &fs.PathError{Op: op, Path: name, Err: errors.New("not a directory")}
	}
	return d, base, nil
}

func (d *node) add(name string, n *node) {
	d.ents[name] = n
	d.dirOps = append(d.dirOps, dirOp{kind: opAdd, name: name, n: n})
}

func (f *FS) OpenFile(name string, flag int, perm fs.FileMode) (wal.FSFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off {
		return nil, &fs.PathError{Op: "open", Path: name, Err: ErrPowerLost}
	}
	d, base, err := f.parent("open", name)
	if err != nil {
		return nil, err
	}
	n, ok := d.ents[base]
	switch {
	case ok && flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0:
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrExist}
	case !ok && flag&os.O_CREATE == 0:
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	case ok && n.isDir && flag&(os.O_WRONLY|os.O_RDWR) != 0:
		return nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("is a directory")}
	case !ok:
		if err := f.step(); err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		n = f.newNode(false, perm&fs.ModePerm)
		d.add(base, n)
	}
	h := &file{fs: f, n: n, name: name, flag: flag}
	if flag&os.O_TRUNC != 0 && flag&(os.O_WRONLY|os.O_RDWR) != 0 && len(n.data) > 0 {
		if err := f.step(); err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		n.truncate(0)
	}
	return h, nil
}

func (n *node) truncate(size int64) {
	n.data = resize(n.data, size)
	n.pending = append(n.pending, write{off: size, trunc: true})
}

func (f *FS) Stat(name string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: ErrPowerLost}
	}
	n, err := f.lookup("stat", name)
	if err != nil {
		return nil, err
	}
	return n.info(path.Base(path.Clean("/" + name))), nil
}

func (f *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: ErrPowerLost}
	}
	d, err := f.lookup("readdir", name)
	if err != nil {
		return nil, err
	}
	if !d.isDir {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("not a directory")}
	}
	var out []fs.DirEntry
	for _, k := range sortedNames(d.ents) {
		out = append(out, fs.FileInfoToDirEntry(d.ents[k].info(k)))
	}
	return out, nil
}

func (f *FS) Mkdir(name string, perm fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off {
		return &fs.PathError{Op: "mkdir", Path: name, Err: ErrPowerLost}
	}
	d, base, err := f.parent("mkdir", name)
	if err != nil {
		return err
	}
	if _, ok := d.ents[base]; ok {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	if err := f.step(); err != nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: err}
	}
	d.add(base, f.newNode(true, fs.ModeDir|perm&fs.ModePerm))
	return nil
}

func (f *FS) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off {
		return &fs.PathError{Op: "remove", Path: name, Err: ErrPowerLost}
	}
	d, base, err := f.parent("remove", name)
	if err != nil {
		return err
	}
	n, ok := d.ents[base]
	if !ok {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	if n.isDir && len(n.ents) > 0 {
		return &fs.PathError{Op: "remove", Path: name, Err: errors.New("directory not empty")}
	}
	if err := f.step(); err != nil {
		return &fs.PathError{Op: "remove", Path: name, Err: err}
	}
	delete(d.ents, base)
	d.dirOps = append(d.dirOps, dirOp{kind: opRemove, name: base})
	return nil
}

// Rename renames a file within one directory. Moving between directories is
// not supported.
func (f *FS) Rename(oldname, newname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off {
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: ErrPowerLost}
	}
	od, obase, err := f.parent("rename", oldname)
	if err != nil {
		return err
	}
	nd, nbase, err := f.parent("rename", newname)
	if err != nil {
		return err
	}
	if od != nd {
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: errors.New("faultfs: rename across directories")}
	}
	n, ok := od.ents[obase]
	if !ok {
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: fs.ErrNotExist}
	}
	if t, ok := nd.ents[nbase]; ok && t.isDir {
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: errors.New("is a directory")}
	}
	if obase == nbase {
		return nil
	}
	if err := f.step(); err != nil {
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: err}
	}
	delete(od.ents, obase)
	od.ents[nbase] = n
	od.dirOps = append(od.dirOps, dirOp{kind: opRename, name: nbase, from: obase, n: n})
	return nil
}

func (f *FS) Link(oldname, newname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: ErrPowerLost}
	}
	n, err := f.lookup("link", oldname)
	if err != nil {
		return err
	}
	if n.isDir {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: errors.New("is a directory")}
	}
	d, base, err := f.parent("link", newname)
	if err != nil {
		return err
	}
	if _, ok := d.ents[base]; ok {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: fs.ErrExist}
	}
	if err := f.step(); err != nil {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: err}
	}
	d.add(base, n)
	return nil
}

func (f *FS) SyncDir(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off {
		return &fs.PathError{Op: "sync", Path: name, Err: ErrPowerLost}
	}
	d, err := f.lookup("sync", name)
	if err != nil {
		return err
	}
	if !d.isDir {
		return &fs.PathError{Op: "sync", Path: name, Err: errors.New("not a directory")}
	}
	if err := f.step(); err != nil {
		return &fs.PathError{Op: "sync", Path: name, Err: err}
	}
	d.durEnts = make(map[string]*node, len(d.ents))
	for k, v := range d.ents {
		d.durEnts[k] = v
	}
	d.dirOps = nil
	return nil
}

func (f *FS) SameFile(a, b fs.FileInfo) bool {
	x, ok1 := a.(*info)
	y, ok2 := b.(*info)
	return ok1 && ok2 && x.n == y.n
}

func (f *FS) Lock(name string, create bool) (io.Closer, error) {
	flag := os.O_RDONLY
	if create {
		flag = os.O_RDWR | os.O_CREATE
	}
	h, err := f.OpenFile(name, flag, 0o600)
	if err != nil {
		return nil, err
	}
	n := h.(*file).n
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.locks[n] {
		return nil, wal.ErrLocked
	}
	f.locks[n] = true
	return &lockHandle{fs: f, n: n}, nil
}

type lockHandle struct {
	fs   *FS
	n    *node
	once sync.Once
}

func (l *lockHandle) Close() error {
	l.once.Do(func() {
		l.fs.mu.Lock()
		delete(l.fs.locks, l.n)
		l.fs.mu.Unlock()
	})
	return nil
}

// ReadFile returns the current contents of a file, for tests.
func (f *FS) ReadFile(name string) ([]byte, error) {
	return wal.ReadFile(f, name)
}

// WriteFile creates or replaces a file and makes it and its directory entry
// durable, for tests that set up a disk.
func (f *FS) WriteFile(name string, data []byte) error {
	h, err := f.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = h.Write(data)
	if err == nil {
		err = h.Sync()
	}
	h.Close()
	if err == nil {
		err = f.SyncDir(path.Dir(path.Clean("/" + name)))
	}
	return err
}

type file struct {
	fs     *FS
	n      *node
	name   string
	flag   int
	off    int64
	closed bool
}

func (h *file) check(op string) error {
	switch {
	case h.closed:
		return &fs.PathError{Op: op, Path: h.name, Err: fs.ErrClosed}
	case h.fs.off:
		return &fs.PathError{Op: op, Path: h.name, Err: ErrPowerLost}
	}
	return nil
}

func (h *file) writable() bool { return h.flag&(os.O_WRONLY|os.O_RDWR) != 0 }

func (h *file) Read(p []byte) (int, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if err := h.check("read"); err != nil {
		return 0, err
	}
	if h.n.isDir || h.flag&os.O_WRONLY != 0 {
		return 0, &fs.PathError{Op: "read", Path: h.name, Err: errors.New("bad file descriptor")}
	}
	if h.off >= int64(len(h.n.data)) {
		return 0, io.EOF
	}
	k := copy(p, h.n.data[h.off:])
	h.off += int64(k)
	return k, nil
}

func (h *file) Write(p []byte) (int, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if err := h.check("write"); err != nil {
		return 0, err
	}
	if !h.writable() {
		return 0, &fs.PathError{Op: "write", Path: h.name, Err: errors.New("bad file descriptor")}
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := h.fs.step(); err != nil {
		return 0, &fs.PathError{Op: "write", Path: h.name, Err: err}
	}
	n := h.n
	if h.flag&os.O_APPEND != 0 {
		h.off = int64(len(n.data))
	}
	end := h.off + int64(len(p))
	if int64(len(n.data)) < end {
		n.data = resize(n.data, end)
	}
	copy(n.data[h.off:], p)
	n.pending = append(n.pending, write{off: h.off, data: append([]byte(nil), p...)})
	h.off = end
	return len(p), nil
}

func (h *file) Seek(offset int64, whence int) (int64, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if err := h.check("seek"); err != nil {
		return 0, err
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = h.off
	case io.SeekEnd:
		base = int64(len(h.n.data))
	default:
		return 0, &fs.PathError{Op: "seek", Path: h.name, Err: fs.ErrInvalid}
	}
	if base+offset < 0 {
		return 0, &fs.PathError{Op: "seek", Path: h.name, Err: fs.ErrInvalid}
	}
	h.off = base + offset
	return h.off, nil
}

func (h *file) Stat() (fs.FileInfo, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if err := h.check("stat"); err != nil {
		return nil, err
	}
	return h.n.info(path.Base(path.Clean("/" + h.name))), nil
}

func (h *file) Truncate(size int64) error {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if err := h.check("truncate"); err != nil {
		return err
	}
	if !h.writable() || size < 0 {
		return &fs.PathError{Op: "truncate", Path: h.name, Err: fs.ErrInvalid}
	}
	if err := h.fs.step(); err != nil {
		return &fs.PathError{Op: "truncate", Path: h.name, Err: err}
	}
	h.n.truncate(size)
	return nil
}

func (h *file) Sync() error {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if err := h.check("sync"); err != nil {
		return err
	}
	if err := h.fs.step(); err != nil {
		return &fs.PathError{Op: "sync", Path: h.name, Err: err}
	}
	n := h.n
	if !n.isDir {
		n.durable = append(n.durable[:0], n.data...)
		n.pending = nil
	}
	return nil
}

func (h *file) Close() error {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.closed {
		return &fs.PathError{Op: "close", Path: h.name, Err: fs.ErrClosed}
	}
	h.closed = true
	return nil
}

type info struct {
	name string
	n    *node
	size int64
}

func (n *node) info(name string) *info {
	return &info{name: name, n: n, size: int64(len(n.data))}
}

func (i *info) Name() string       { return i.name }
func (i *info) Size() int64        { return i.size }
func (i *info) Mode() fs.FileMode  { return i.n.mode }
func (i *info) ModTime() time.Time { return time.Time{} }
func (i *info) IsDir() bool        { return i.n.isDir }
func (i *info) Sys() any           { return nil }
