package wal

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
)

// The manifest names the segments that make up the log, oldest first, by
// the index of each one's first record:
//
//	ledgerd-wal 1
//	1
//	48211
//	crc <crc32c of the lines above, 8 hex digits>
//
// Every segment but the last is sealed; the last one takes appends. The
// manifest is the authority on which segment files belong to the log: it is
// replaced atomically (temporary file, fsync, rename, directory fsync) when
// a segment is added or dropped, and a segment file it does not list is left
// over from an interrupted roll or drop.
const (
	manifestName  = "MANIFEST"
	manifestMagic = "ledgerd-wal 1"
	lockName      = "LOCK"
)

// errAfterRename marks a manifest write that failed after the rename, when
// the directory may hold either version.
var errAfterRename = errors.New("manifest renamed but the directory fsync failed")

func encodeManifest(firsts []uint64) []byte {
	b := []byte(manifestMagic + "\n")
	for _, f := range firsts {
		b = strconv.AppendUint(b, f, 10)
		b = append(b, '\n')
	}
	sum := crc32.Checksum(b, crcTable)
	b = append(b, "crc "...)
	b = appendHex32(b, sum)
	return append(b, '\n')
}

func decodeManifest(data []byte) ([]uint64, error) {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, errors.New("manifest is truncated")
	}
	last := bytes.LastIndexByte(data[:len(data)-1], '\n') + 1
	body, tail := data[:last], data[last:len(data)-1]
	if !bytes.HasPrefix(tail, []byte("crc ")) {
		return nil, errors.New("manifest has no checksum line")
	}
	sum, ok := parseHex32(tail[4:])
	if !ok || crc32.Checksum(body, crcTable) != sum {
		return nil, errors.New("manifest checksum mismatch")
	}
	lines := bytes.Split(body[:max(len(body)-1, 0)], []byte("\n"))
	if len(lines) < 2 || string(lines[0]) != manifestMagic {
		return nil, errors.New("manifest has an unknown format or no segments")
	}
	firsts := make([]uint64, 0, len(lines)-1)
	for _, ln := range lines[1:] {
		f, err := strconv.ParseUint(string(ln), 10, 64)
		if err != nil || f == 0 || (len(firsts) > 0 && f <= firsts[len(firsts)-1]) {
			return nil, fmt.Errorf("manifest lists a bad segment %q", ln)
		}
		firsts = append(firsts, f)
	}
	if !bytes.Equal(encodeManifest(firsts), data) {
		return nil, errors.New("manifest is not in canonical form")
	}
	return firsts, nil
}

// readManifest returns the segments listed in dir's manifest. A missing
// manifest is reported with an error matching os.ErrNotExist.
func readManifest(dir string) ([]uint64, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	firsts, err := decodeManifest(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return firsts, nil
}

// writeManifest atomically replaces dir's manifest.
func writeManifest(dir string, firsts []uint64, wrap func(File) File) error {
	tmpPath := filepath.Join(dir, manifestName+".tmp")
	osf, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	var f File = osf
	if wrap != nil {
		f = wrap(osf)
	}
	if _, err := f.Write(encodeManifest(firsts)); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, manifestName)); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("%w: %v", errAfterRename, err)
	}
	return nil
}

// SegmentPaths returns the paths of the segments listed in the manifest of
// the log in dir, oldest first. The last one is the segment taking appends.
func SegmentPaths(dir string) ([]string, error) {
	firsts, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(firsts))
	for i, f := range firsts {
		paths[i] = filepath.Join(dir, segName(f))
	}
	return paths, nil
}

func appendHex32(b []byte, v uint32) []byte {
	const digits = "0123456789abcdef"
	for s := 28; s >= 0; s -= 4 {
		b = append(b, digits[(v>>uint(s))&0xf])
	}
	return b
}

// parseHex32 accepts exactly 8 lower-case hex digits, so each value has one
// encoding.
func parseHex32(b []byte) (uint32, bool) {
	if len(b) != 8 {
		return 0, false
	}
	var v uint32
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint32(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint32(c-'a'+10)
		default:
			return 0, false
		}
	}
	return v, true
}
