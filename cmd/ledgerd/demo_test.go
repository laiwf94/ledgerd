package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDemoScript runs examples/demo.sh against the real binary, twice and
// across a restart, as the README tells users to, then checks the data
// directory offline. The script is the end-to-end walk through every
// account feature over HTTP, so it must not drift from the API.
func TestDemoScript(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s is needed to run examples/demo.sh: %v", tool, err)
			}
			t.Skipf("%s not installed: %v", tool, err)
		}
	}
	script, err := filepath.Abs("../../examples/demo.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "data")

	run := func(base, suffix string) string {
		t.Helper()
		cmd := exec.Command("bash", script)
		cmd.Env = append(os.Environ(), "LEDGERD_URL="+base, "LEDGERD_API_KEY="+key,
			"LEDGERD_DEMO_RUN="+suffix, "LEDGERD_ADMIN_TOKEN=")
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		done := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("demo.sh failed: %v\nstdout:\n%s\nstderr:\n%s", err, &out, &errOut)
			}
		case <-time.After(2 * time.Minute):
			cmd.Process.Kill()
			t.Fatalf("demo.sh did not finish\nstdout:\n%s", &out)
		}
		if errOut.Len() > 0 {
			t.Fatalf("demo.sh wrote to stderr:\n%s", &errOut)
		}
		return out.String()
	}

	// Lines each run must print, in this order: each step's outcome as the
	// README and docs/DESIGN.md describe it.
	want := func(s string) []string {
		acme := "acme-" + s
		return []string{
			"1. Open accounts",
			"is pending",
			"   state: posted",
			"reverses tr_",
			"Idempotent-Replayed: true",
			"   limit_exceeded: account alice-" + s + " may not go negative",
			"   matched           stl-1-" + s + " -> tr_",
			"   not in ledger:    stl-3-" + s + " 12.00",
			"   " + acme + ": open, available 200.00, overdraft limit 0.00",
			"   " + acme + ": frozen (compliance review), available 200.00, overdraft limit 0.00",
			"   refused: account_frozen: account " + acme + " is frozen",
			fmt.Sprintf("   %-18s %-7s available %7.2f", acme, "frozen", 200.0),
			"has_more True)",
			fmt.Sprintf("   %-18s %-7s available %7.2f", "zenith-"+s, "open", 0.0),
			"has_more False)",
			" posted 200.00",
			"   limit takes effect from entry ",
			"   " + acme + ": open (review passed), available -30.00, overdraft limit 50.00",
			"   overdraft -> 50.00   from entry ",
			"approved credit line",
			"   refused: failed_precondition: account " + acme + " has a posted balance of -3000",
			"   " + acme + ": closed (offboarded), available 0.00, overdraft limit 50.00",
			"   refused: account_closed: account " + acme + " is closed",
			"   statement stmt-" + s + ": 5 entries, 4 booked, 1 pending skipped",
			"   matched           bk1-" + s + "   245.50 -> 3 movements by reference_batch",
			"   matched           bk2-" + s + "    -7.39 -> 1 movement by reference_near (ledger -7.36, difference -0.03)",
			"   matched           bk3-" + s + "  -100.00 -> 1 movement by reference",
			"   not in ledger:    bk4-" + s + " -2.50 ACCOUNT MAINTENANCE",
			"matches 3 (near 1), proposed 3, breaks 1",
			"   confirmed 3 matches: 0 proposed, 3 confirmed",
			"   open breaks on operating-" + s + ": 1",
			"   0-2  days: 1 missing in ledger, 0 missing in statement, 0 mismatched, USD 2.50",
			// Each run closes its own break before it ends, so only this run's is open.
			`   ledgerd_recon_open_breaks{bucket="0-2"} 1`,
			"   matched           bk4-" + s + "    -2.50 -> 1 movement by amount_date",
			"matches 4 (near 1), proposed 4, breaks 0",
			"   open breaks on operating-" + s + ": 0",
			"16. Invariants",
			"   ok: True",
		}
	}
	check := func(out, suffix string) {
		t.Helper()
		rest := out
		for _, w := range want(suffix) {
			i := strings.Index(rest, w)
			if i < 0 {
				t.Fatalf("demo output lacks %q after the earlier steps:\n%s", w, out)
			}
			rest = rest[i+len(w):]
		}
		// Each cohort listing has exactly two pages of one account.
		if n := strings.Count(out, "(page as of entry"); n != 2 {
			t.Fatalf("cohort listing printed %d pages, want 2:\n%s", n, out)
		}
	}

	p := spawn(t, dir)
	check(run(p.base, "first"), "first")
	p.stop(t)

	// A second run on the restarted server works on its own accounts and
	// sees the first run's closed account still closed.
	p = spawn(t, dir)
	check(run(p.base, "second"), "second")
	if _, err := send(p.base, "POST", "/v1/accounts/acme-first/unfreeze", `{}`, ""); err == nil ||
		!strings.Contains(err.Error(), ": 409 ") || !strings.Contains(err.Error(), "account_closed") {
		t.Fatalf("unfreezing the first run's closed account after a restart: %v", err)
	}
	// The first run's reconciliations survive the restart as it left them:
	// the reviewed run with its break, and the later run that closed it.
	var runs struct {
		Reconciliations []struct {
			Summary struct {
				Matches   int `json:"matches"`
				Proposed  int `json:"proposed"`
				Confirmed int `json:"confirmed"`
				Breaks    int `json:"breaks"`
			} `json:"summary"`
		} `json:"reconciliations"`
	}
	getJSON(t, p.base, "/v1/reconciliations?account=operating-first", &runs)
	if n := len(runs.Reconciliations); n != 2 {
		t.Fatalf("operating-first has %d saved runs after the restart, want 2", n)
	}
	first, second := runs.Reconciliations[0].Summary, runs.Reconciliations[1].Summary
	if first.Matches != 3 || first.Confirmed != 3 || first.Proposed != 0 || first.Breaks != 1 {
		t.Fatalf("first saved run after the restart: %+v", first)
	}
	if second.Matches != 4 || second.Proposed != 4 || second.Breaks != 0 {
		t.Fatalf("second saved run after the restart: %+v", second)
	}
	var aging struct {
		OpenBreaks int `json:"open_breaks"`
	}
	getJSON(t, p.base, "/v1/reconciliations/breaks/aging", &aging)
	if aging.OpenBreaks != 0 {
		t.Fatalf("%d open breaks after both demo runs closed theirs", aging.OpenBreaks)
	}
	p.stop(t)

	if code, report := verifyDir(t, dir); code != 0 {
		t.Fatalf("verify after the demo: exit %d\n%s", code, report)
	}
}

// getJSON decodes the 200 response to an authenticated GET into v.
func getJSON(t *testing.T, base, path string, v any) {
	t.Helper()
	req, err := http.NewRequest("GET", base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := e2eHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("GET %s: %v in %s", path, err, data)
	}
}
