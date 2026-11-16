package export

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// KeyFile is the name of the signing key's file in a data directory.
const KeyFile = "export.key"

// ParseSeed reads a signing key seed: 32 bytes in base64.
func ParseSeed(s string) ([]byte, error) {
	b, err := decodeBase64(strings.TrimSpace(s))
	if err != nil || len(b) != SeedSize {
		return nil, fmt.Errorf("export: a signing key is %d bytes of base64", SeedSize)
	}
	return b, nil
}

// LoadOrCreateKey reads the signing key seed in path, or makes a random one
// and stores it there first: written to a temporary file with mode 0600,
// synced, renamed into place and the directory synced, so a crash leaves
// either no key or the whole key. It reports whether it created the key.
func LoadOrCreateKey(path string) (*Signer, bool, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		seed, err := ParseSeed(string(b))
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
		s, err := NewSigner(seed)
		return s, false, err
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	seed := make([]byte, SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, false, err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, false, err
	}
	_, err = f.WriteString(EncodeKey(seed) + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err == nil {
		err = syncDir(filepath.Dir(path))
	}
	if err != nil {
		os.Remove(tmp)
		return nil, false, fmt.Errorf("storing the export signing key: %w", err)
	}
	s, err := NewSigner(seed)
	return s, true, err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
