package wal

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// FS is the file system the log and the ledger's snapshots are stored on.
// OSFS is the real one. The power-loss tests use faultfs, which keeps
// unsynced writes and directory changes in memory and throws some of them
// away on a simulated power cut.
//
// Durability follows POSIX: file data is durable once the file is synced;
// a new, renamed or removed directory entry is durable once its directory is
// synced with SyncDir. Nothing else is promised.
type FS interface {
	// OpenFile opens a file with os.OpenFile's flags. Only O_RDONLY,
	// O_WRONLY, O_RDWR, O_CREATE, O_EXCL, O_TRUNC and O_APPEND are used.
	OpenFile(name string, flag int, perm fs.FileMode) (FSFile, error)
	Stat(name string) (fs.FileInfo, error)
	// ReadDir returns the directory's entries sorted by name.
	ReadDir(name string) ([]fs.DirEntry, error)
	Mkdir(name string, perm fs.FileMode) error
	Remove(name string) error
	Rename(oldname, newname string) error
	Link(oldname, newname string) error
	// SyncDir makes the directory's entries durable.
	SyncDir(name string) error
	// SameFile reports whether two FileInfos from Stat describe the same file.
	SameFile(a, b fs.FileInfo) bool
	// Lock takes an exclusive lock on the named file that no other holder
	// on this file system can take until it is released or its holder dies.
	// With create the file is made if missing; otherwise a missing file is
	// an error matching fs.ErrNotExist. A lock that is held is ErrLocked.
	Lock(name string, create bool) (io.Closer, error)
}

// FSFile is an open file on an FS.
type FSFile interface {
	io.Reader
	io.Writer
	io.Seeker
	Stat() (fs.FileInfo, error)
	Truncate(size int64) error
	Sync() error
	Close() error
}

// OSFS is the operating system's file system.
type OSFS struct{}

func (OSFS) OpenFile(name string, flag int, perm fs.FileMode) (FSFile, error) {
	f, err := os.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err // not a typed nil
	}
	return f, nil
}

func (OSFS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }
func (OSFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (OSFS) Mkdir(name string, perm fs.FileMode) error  { return os.Mkdir(name, perm) }
func (OSFS) Remove(name string) error                   { return os.Remove(name) }
func (OSFS) Rename(oldname, newname string) error       { return os.Rename(oldname, newname) }
func (OSFS) Link(oldname, newname string) error         { return os.Link(oldname, newname) }
func (OSFS) SameFile(a, b fs.FileInfo) bool             { return os.SameFile(a, b) }

func (OSFS) SyncDir(name string) error {
	d, err := os.Open(name)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (OSFS) Lock(name string, create bool) (io.Closer, error) {
	flag := os.O_RDONLY
	if create {
		flag = os.O_RDWR | os.O_CREATE
	}
	f, err := os.OpenFile(name, flag, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ReadFile reads a whole file from fsys.
func ReadFile(fsys FS, name string) ([]byte, error) {
	f, err := fsys.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// MkdirAll creates dir and any missing parents on fsys, and syncs the
// parent of each directory it creates so that the new entries survive a
// power cut. A directory that only exists until the next crash would take
// every record later written into it with it.
func MkdirAll(fsys FS, dir string) error {
	dir = filepath.Clean(dir)
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		st, err := fsys.Stat(d)
		if err == nil {
			if !st.IsDir() {
				return &fs.PathError{Op: "mkdir", Path: d, Err: errors.New("not a directory")}
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		d := missing[i]
		if err := fsys.Mkdir(d, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := fsys.SyncDir(filepath.Dir(d)); err != nil {
			return err
		}
	}
	return nil
}
