package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

// Exit codes of ledgerd verify.
const (
	exitSound      = 0 // every check passed
	exitDamaged    = 1 // the server would refuse to start, or acknowledged data is lost
	exitRepairable = 2 // nothing acknowledged is lost; a normal start repairs it
	exitError      = 3 // verify could not run: bad usage, directory in use or unreadable
)

const verifyUsage = `usage: ledgerd verify [-json] [dir]

Checks a data directory offline, without starting the server and without
changing any file: every checksum in every log segment and snapshot, that
segments follow each other without a gap, that each snapshot keeps every
invariant and equals the state the log replays to at its record, and that
the fully replayed ledger keeps every invariant. dir defaults to
LEDGERD_DATA_DIR. The directory must not be in use by a running server.

Exit status: 0 sound, 1 damaged, 2 repairable (a normal start repairs it
without losing an acknowledged write), 3 verify could not run.
`

// verify runs the verify subcommand and returns the process exit code.
func verify(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, verifyUsage) }
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitSound
		}
		return exitError
	}
	dir := getenv("LEDGERD_DATA_DIR")
	switch fs.NArg() {
	case 0:
	case 1:
		dir = fs.Arg(0)
	default:
		fs.Usage()
		return exitError
	}
	if dir == "" {
		fmt.Fprintln(stderr, "ledgerd verify: no directory given and LEDGERD_DATA_DIR is not set")
		return exitError
	}
	rep, err := ledger.Verify(dir)
	if err != nil {
		fmt.Fprintln(stderr, "ledgerd verify:", err)
		return exitError
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(stderr, "ledgerd verify:", err)
			return exitError
		}
	} else {
		printReport(stdout, rep)
	}
	switch rep.Status {
	case ledger.StatusSound:
		return exitSound
	case ledger.StatusRepairable:
		return exitRepairable
	default:
		return exitDamaged
	}
}

func printReport(w io.Writer, r *ledger.VerifyReport) {
	fmt.Fprintf(w, "ledgerd verify %s\n", r.Dir)
	if lg := r.Log; lg != nil && !lg.Empty {
		kind := "segments"
		if len(lg.Segments) == 1 {
			kind = "segment"
		}
		if lg.Legacy {
			kind = "single-file log from an earlier version; the next start converts it"
		}
		fmt.Fprintf(w, "log: records %d to %d, %d %s\n", lg.FirstIndex, max(lg.NextIndex, 1)-1, len(lg.Segments), kind)
		for _, s := range lg.Segments {
			state := "active"
			if s.Sealed {
				state = "sealed"
			}
			verdict := "ok"
			switch {
			case s.Problem != "":
				verdict = "DAMAGED: " + s.Problem
			case s.Torn > 0:
				verdict = fmt.Sprintf("ok, torn tail of %d bytes", s.Torn)
			}
			fmt.Fprintf(w, "  %s  %s  %d records  %d bytes  %s\n", s.Name, state, s.Records, s.Bytes, verdict)
		}
	} else {
		fmt.Fprintln(w, "log: none")
	}
	if len(r.Snapshots) == 0 {
		fmt.Fprintln(w, "snapshots: none")
	} else {
		fmt.Fprintln(w, "snapshots:")
	}
	for _, s := range r.Snapshots {
		verdict := "ok"
		switch {
		case s.Problem != "":
			verdict = "BAD: " + s.Problem
		case s.Matched && s.Number == r.ReplayFrom:
			verdict = fmt.Sprintf("ok, replay started here (record %d)", s.Record)
		case s.Matched:
			verdict = fmt.Sprintf("ok, equals the log at record %d", s.Record)
		}
		fmt.Fprintf(w, "  %s  head %d  %d bytes  %s\n", s.Name, s.Head, s.Bytes, verdict)
	}
	if len(r.Damage) == 0 {
		from := "the first record"
		if r.ReplayFrom != 0 {
			from = fmt.Sprintf("snapshot %d", r.ReplayFrom)
		}
		fmt.Fprintf(w, "state: replayed from %s to journal head %d: %d accounts, %d transfers, %d open pending; every invariant holds\n",
			from, r.Head, r.Accounts, r.Transfers, r.OpenPending)
	}
	for _, d := range r.Repairable {
		fmt.Fprintln(w, "repairable:", d)
	}
	for _, d := range r.Damage {
		fmt.Fprintln(w, "damaged:", d)
	}
	fmt.Fprintln(w, "result:", r.Status)
}
