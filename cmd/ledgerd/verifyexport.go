package main

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/laiwf94/ledgerd/internal/export"
)

// Exit codes of ledgerd verify-export.
const (
	exportVerified = 0 // the file matches its manifest, signed by the trusted key
	exportDamaged  = 1 // cut short, altered, not an export, or a bad signature
	exportIntact   = 2 // the file matches its manifest; no key was given to check the signature
	exportError    = 3 // verify-export could not run
)

const verifyExportUsage = `usage: ledgerd verify-export [-key base64 | -key-file path] [-json] file

Checks a file from GET /v1/export against the signed manifest on its last
line: every byte against the running SHA-256 hashes, the entry and row
counts, that entries come in sequence order up to the export's head and fall
inside its period, and the Ed25519 signature against the trusted public key
from GET /v1/export/key or the server's startup log. file may be - for
standard input.

Exit status: 0 verified, 1 damaged (cut short, altered, not an export or a
bad signature), 2 intact but unsigned (no key given), 3 could not run.
`

func verifyExport(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify-export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, verifyExportUsage) }
	keyArg := fs.String("key", "", "trusted public key, base64")
	keyFile := fs.String("key-file", "", "file holding the trusted public key")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exportVerified
		}
		return exportError
	}
	if fs.NArg() != 1 || *keyArg != "" && *keyFile != "" {
		fs.Usage()
		return exportError
	}
	var trusted ed25519.PublicKey
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			fmt.Fprintln(stderr, "ledgerd verify-export:", err)
			return exportError
		}
		*keyArg = string(b)
	}
	if *keyArg != "" {
		k, err := export.ParsePublicKey(strings.TrimSpace(*keyArg))
		if err != nil {
			fmt.Fprintln(stderr, "ledgerd verify-export:", err)
			return exportError
		}
		trusted = k
	}
	in := stdin
	if name := fs.Arg(0); name != "-" {
		f, err := os.Open(name)
		if err != nil {
			fmt.Fprintln(stderr, "ledgerd verify-export:", err)
			return exportError
		}
		defer f.Close()
		in = f
	}
	rep, err := export.Verify(in, trusted)
	if err != nil {
		fmt.Fprintln(stderr, "ledgerd verify-export:", err)
		return exportError
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(stderr, "ledgerd verify-export:", err)
			return exportError
		}
	} else {
		printExportReport(stdout, rep)
	}
	switch rep.Status {
	case export.StatusVerified:
		return exportVerified
	case export.StatusIntact:
		return exportIntact
	default:
		return exportDamaged
	}
}

func printExportReport(w io.Writer, r *export.Report) {
	fmt.Fprintf(w, "ledgerd verify-export: %s\n", r.Status)
	if m := r.Manifest; m != nil {
		period := "all time"
		if !m.From.IsZero() || !m.To.IsZero() {
			period = fmt.Sprintf("[%s, %s)", timeOrOpen(m.From), timeOrOpen(m.To))
		}
		fmt.Fprintf(w, "manifest: %s, %s, head %d, entries %d to %d, key %s, created %s\n",
			m.Format, period, m.HeadSeq, m.FirstSeq, m.LastSeq, m.KeyID, m.CreatedAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(w, "file: %d entries in %d rows, %d bytes; signature %s\n", r.Entries, r.Rows, r.Bytes, r.Signature)
	if r.Status == export.StatusDamaged && r.IntactEntries > 0 {
		fmt.Fprintf(w, "the first %d entries are as exported\n", r.IntactEntries)
	}
	for _, p := range r.Problems {
		fmt.Fprintln(w, "problem:", p)
	}
}

func timeOrOpen(t time.Time) string {
	if t.IsZero() {
		return "open"
	}
	return t.UTC().Format(time.RFC3339Nano)
}
