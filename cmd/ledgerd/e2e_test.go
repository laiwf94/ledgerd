package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The end-to-end test runs the real server as a child process: the test
// binary re-executes itself with childEnv set and runs main.
const childEnv = "LEDGERD_E2E_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// proc is a ledgerd child process.
type proc struct {
	cmd       *exec.Cmd
	base      string
	recovered map[string]any // the "recovered" log line
	stderr    *bytes.Buffer
	done      chan error
}

// spawn starts ledgerd on dir and waits until it listens.
func spawn(t *testing.T, dir string) *proc {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=1", "LEDGERD_ADDR=127.0.0.1:0", "LEDGERD_API_KEYS="+key,
		"LEDGERD_DATA_DIR="+dir, "LEDGERD_SNAPSHOT_EVERY=500", "LEDGERD_SEGMENT_BYTES=4096", "LEDGERD_ADMIN_TOKEN=")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p := &proc{cmd: cmd, stderr: &bytes.Buffer{}, done: make(chan error, 1)}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	addrc := make(chan string, 1)
	recc := make(chan map[string]any, 1)
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var line map[string]any
			if json.Unmarshal(sc.Bytes(), &line) != nil {
				continue
			}
			switch line["msg"] {
			case "recovered":
				recc <- line
			case "listening":
				addrc <- line["addr"].(string)
			}
		}
		io.Copy(io.Discard, out)
	}()
	go func() { p.done <- cmd.Wait() }()
	select {
	case addr := <-addrc:
		p.base = "http://" + addr
		p.recovered = <-recc
	case err := <-p.done:
		t.Fatalf("ledgerd exited before listening: %v\n%s", err, p.stderr)
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("ledgerd did not start\n%s", p.stderr)
	}
	return p
}

// kill sends SIGKILL and waits for the process to go.
func (p *proc) kill(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-p.done
}

// stop sends SIGTERM and requires a clean exit.
func (p *proc) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("ledgerd did not exit cleanly: %v\n%s", err, p.stderr)
		}
	case <-time.After(60 * time.Second):
		p.cmd.Process.Kill()
		t.Fatalf("ledgerd did not stop\n%s", p.stderr)
	}
}

// verifyDir runs "ledgerd verify -json dir" as a child process.
func verifyDir(t *testing.T, dir string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "verify", "-json", dir)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, out.String()
	case errors.As(err, &ee):
		return ee.ExitCode(), out.String() + errOut.String()
	}
	t.Fatal(err)
	return 0, ""
}

// copyDir copies a stopped server's data directory, as an operator restoring
// it onto another machine would.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// e2eOp is one keyed request a client makes. Post, void and reverse act on
// the transfer an earlier op of the same client created (ref).
type e2eOp struct {
	kind     string // transfer, hold, post, void, reverse
	from, to string
	amount   int64
	ref      int
	key      string
}

func (o e2eOp) request(ids []string) (method, path, body string) {
	postings := fmt.Sprintf(`"postings":[{"account":%q,"direction":"debit","amount":%d},{"account":%q,"direction":"credit","amount":%d}]`,
		o.from, o.amount, o.to, o.amount)
	switch o.kind {
	case "transfer":
		return "POST", "/v1/transfers", `{"currency":"EUR",` + postings + `}`
	case "hold":
		return "POST", "/v1/transfers", `{"currency":"EUR","pending":true,"timeout_seconds":86400,` + postings + `}`
	}
	return "POST", "/v1/transfers/" + ids[o.ref] + "/" + o.kind, `{}`
}

// e2eClient issues its ops one at a time and remembers how far it got.
type e2eClient struct {
	n        int
	rng      *rand.Rand
	accounts []string
	ops      []e2eOp
	ids      []string // transfer IDs returned for ops[i]
	acked    int      // ops[:acked] were answered
	inflight bool     // ops[acked] was sent but never answered
	reversed map[int]bool
}

// next appends more ops to the script when the client has run out.
func (c *e2eClient) next() {
	pick := func() (string, string) {
		i := c.rng.IntN(len(c.accounts))
		j := (i + 1 + c.rng.IntN(len(c.accounts)-1)) % len(c.accounts)
		return c.accounts[i], c.accounts[j]
	}
	add := func(o e2eOp) {
		o.key = fmt.Sprintf("e2e-c%d-op%d", c.n, len(c.ops))
		c.ops = append(c.ops, o)
		c.ids = append(c.ids, "")
	}
	from, to := pick()
	amount := 1 + c.rng.Int64N(10_000)
	switch r := c.rng.IntN(10); {
	case r < 6:
		add(e2eOp{kind: "transfer", from: from, to: to, amount: amount})
	case r < 9:
		add(e2eOp{kind: "hold", from: from, to: to, amount: amount})
		kind := "post"
		if r == 8 {
			kind = "void"
		}
		add(e2eOp{kind: kind, from: from, to: to, amount: amount, ref: len(c.ops) - 1})
	default:
		for i := len(c.ops) - 1; i >= 0; i-- {
			if o := c.ops[i]; o.kind == "transfer" && !c.reversed[i] {
				c.reversed[i] = true
				add(e2eOp{kind: "reverse", from: o.from, to: o.to, amount: o.amount, ref: i})
				return
			}
		}
		add(e2eOp{kind: "transfer", from: from, to: to, amount: amount})
	}
}

type e2eResult struct {
	status   int
	id       string
	replayed bool
}

var e2eHTTP = &http.Client{Timeout: 30 * time.Second}

func send(base, method, path, body, idemKey string) (e2eResult, error) {
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		return e2eResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := e2eHTTP.Do(req)
	if err != nil {
		return e2eResult{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return e2eResult{}, err
	}
	var v struct {
		ID string `json:"id"`
	}
	json.Unmarshal(data, &v)
	if resp.StatusCode/100 != 2 {
		return e2eResult{}, errHTTP{fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, data)}
	}
	return e2eResult{status: resp.StatusCode, id: v.ID, replayed: resp.Header.Get("Idempotent-Replayed") == "true"}, nil
}

// errHTTP marks a request the server answered with an error, as opposed to
// one that got no answer.
type errHTTP struct{ error }

func (c *e2eClient) do(base string, i int) (e2eResult, error) {
	method, path, body := c.ops[i].request(c.ids)
	return send(base, method, path, body, c.ops[i].key)
}

// model folds every applied op into per-account totals, the four numbers
// GET /v1/accounts reports.
type totals struct{ DebitsPosted, CreditsPosted, DebitsPending, CreditsPending int64 }

func model(clients []*e2eClient) map[string]totals {
	m := map[string]totals{}
	for _, c := range clients {
		for _, o := range c.ops[:c.acked] {
			f, to := m[o.from], m[o.to]
			switch o.kind {
			case "transfer":
				f.DebitsPosted += o.amount
				to.CreditsPosted += o.amount
			case "hold":
				f.DebitsPending += o.amount
				to.CreditsPending += o.amount
			case "post":
				f.DebitsPending -= o.amount
				to.CreditsPending -= o.amount
				f.DebitsPosted += o.amount
				to.CreditsPosted += o.amount
			case "void":
				f.DebitsPending -= o.amount
				to.CreditsPending -= o.amount
			case "reverse":
				f.CreditsPosted += o.amount
				to.DebitsPosted += o.amount
			}
			m[o.from], m[o.to] = f, to
		}
	}
	return m
}

func balances(t *testing.T, base string, accounts []string) map[string]totals {
	t.Helper()
	got := map[string]totals{}
	for _, a := range accounts {
		req, _ := http.NewRequest("GET", base+"/v1/accounts/"+a, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := e2eHTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Balance struct {
				DebitsPosted   int64 `json:"debits_posted"`
				CreditsPosted  int64 `json:"credits_posted"`
				DebitsPending  int64 `json:"debits_pending"`
				CreditsPending int64 `json:"credits_pending"`
			} `json:"balance"`
		}
		err = json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET account %s: %d %v", a, resp.StatusCode, err)
		}
		got[a] = totals(v.Balance)
	}
	return got
}

func compareBalances(t *testing.T, base string, accounts []string, clients []*e2eClient) {
	t.Helper()
	want, got := model(clients), balances(t, base, accounts)
	for _, a := range accounts {
		if got[a] != want[a] {
			t.Errorf("account %s: server has %+v, the acknowledged history gives %+v", a, got[a], want[a])
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

func checkInvariants(t *testing.T, base string) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/v1/admin/invariants", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := e2eHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		OK         bool     `json:"ok"`
		Violations []string `json:"violations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || resp.StatusCode != 200 || !v.OK {
		t.Fatalf("invariants: %d %v %v", resp.StatusCode, err, v.Violations)
	}
}

// replayAcknowledged resends every op a client was answered for with its
// Idempotency-Key. Each must come back as a replay of the same result,
// which proves the restored server still holds it. The op that was in
// flight is resent too and may be either a replay (it was written before
// the kill) or new; afterwards it counts as acknowledged.
func replayAcknowledged(t *testing.T, base string, clients []*e2eClient) (inflightKept, inflightLost int) {
	t.Helper()
	var mu sync.Mutex
	var wg sync.WaitGroup
	errc := make(chan error, len(clients))
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < c.acked; i++ {
				res, err := c.do(base, i)
				if err != nil {
					errc <- fmt.Errorf("client %d op %d (%s): %v", c.n, i, c.ops[i].kind, err)
					return
				}
				if !res.replayed || (c.ids[i] != "" && res.id != c.ids[i]) {
					errc <- fmt.Errorf("client %d op %d (%s, key %s) was acknowledged as %s but the restored server answered replayed=%v id=%s",
						c.n, i, c.ops[i].kind, c.ops[i].key, c.ids[i], res.replayed, res.id)
					return
				}
			}
			if !c.inflight {
				return
			}
			res, err := c.do(base, c.acked)
			if err != nil {
				errc <- fmt.Errorf("client %d in-flight op %d: %v", c.n, c.acked, err)
				return
			}
			mu.Lock()
			if res.replayed {
				inflightKept++
			} else {
				inflightLost++
			}
			mu.Unlock()
			c.ids[c.acked] = res.id
			c.acked++
			c.inflight = false
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
	return inflightKept, inflightLost
}

// files lists the snapshot files and log segments in a data directory.
func files(dir string) (snaps, segs []string) {
	names, _ := os.ReadDir(dir)
	for _, e := range names {
		if strings.HasPrefix(e.Name(), "snapshot-") && strings.HasSuffix(e.Name(), ".snap") {
			snaps = append(snaps, e.Name())
		}
	}
	names, _ = os.ReadDir(filepath.Join(dir, "wal"))
	for _, e := range names {
		if strings.HasSuffix(e.Name(), ".seg") {
			segs = append(segs, e.Name())
		}
	}
	sort.Strings(snaps)
	sort.Strings(segs)
	return snaps, segs
}

// TestKillNineRestoreVerify is the end-to-end crash test. Each round runs
// a real ledgerd process under load from concurrent clients, kills it with
// SIGKILL once it has written new snapshots and dropped segments, copies
// its data directory somewhere else, runs ledgerd verify on the copy and
// starts the next server on the copy. That server must still hold every
// acknowledged request (resending each with its Idempotency-Key replays the
// original answer) and its balances must equal the acknowledged history.
// The last copy is shut down cleanly, must then verify as sound, and must
// restart to the same balances.
func TestKillNineRestoreVerify(t *testing.T) {
	const rounds, nClients, perRound = 3, 8, 1500
	accounts := []string{"cash", "bank", "clearing", "fees", "alice", "bob", "carol", "dave", "erin", "frank"}
	clients := make([]*e2eClient, nClients)
	for i := range clients {
		clients[i] = &e2eClient{n: i, rng: rand.New(rand.NewPCG(uint64(i), 42)), accounts: accounts, reversed: map[int]bool{}}
	}
	root := t.TempDir()
	dir := filepath.Join(root, "data-0")
	var verifyCodes []int
	var kept, lost int

	for round := 0; round <= rounds; round++ {
		p := spawn(t, dir)
		t.Logf("round %d: recovered head %v in %v ms from %s", round, p.recovered["head"], p.recovered["duration_ms"], filepath.Base(dir))
		if round == 0 {
			for i, a := range accounts {
				normal := "debit"
				if i%2 == 1 {
					normal = "credit"
				}
				if _, err := send(p.base, "POST", "/v1/accounts", fmt.Sprintf(`{"id":%q,"currency":"EUR","normal_balance":%q}`, a, normal), ""); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			k, l := replayAcknowledged(t, p.base, clients)
			kept, lost = kept+k, lost+l
			compareBalances(t, p.base, accounts, clients)
			checkInvariants(t, p.base)
		}
		if round == rounds {
			p.stop(t)
			break
		}

		// Load the server, then kill it once it has done real storage work.
		snaps0, _ := files(dir)
		var acked atomic.Int64
		var killed atomic.Bool
		var wg sync.WaitGroup
		errc := make(chan error, nClients)
		for _, c := range clients {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					if c.acked == len(c.ops) {
						c.next()
					}
					res, err := c.do(p.base, c.acked)
					if err != nil {
						var he errHTTP
						if errors.As(err, &he) || !killed.Load() {
							errc <- fmt.Errorf("client %d op %d (%s): %v", c.n, c.acked, c.ops[c.acked].kind, err)
						}
						c.inflight = true
						return
					}
					c.ids[c.acked] = res.id
					c.acked++
					acked.Add(1)
				}
			}()
		}
		deadline := time.Now().Add(3 * time.Minute)
		for {
			snaps, segs := files(dir)
			newSnap := len(snaps) > 0 && (len(snaps0) == 0 || snaps[len(snaps)-1] > snaps0[len(snaps0)-1])
			dropped := len(segs) > 0 && segs[0] != "00000000000000000001.seg"
			if acked.Load() >= perRound && newSnap && dropped {
				break
			}
			if time.Now().After(deadline) {
				p.cmd.Process.Kill()
				t.Fatalf("round %d: no snapshot or segment drop after %d requests (snapshots %v, %d segments)", round, acked.Load(), snaps, len(segs))
			}
			time.Sleep(10 * time.Millisecond)
		}
		killed.Store(true)
		p.kill(t)
		wg.Wait()
		close(errc)
		for err := range errc {
			t.Error(err)
		}
		if t.Failed() {
			t.FailNow()
		}
		inflight := 0
		for _, c := range clients {
			if c.inflight {
				inflight++
			}
		}
		snaps, segs := files(dir)
		t.Logf("round %d: killed after %d acknowledged requests, %d in flight; %d snapshots, %d segments on disk",
			round, acked.Load(), inflight, len(snaps), len(segs))

		// Restore: copy the directory and check the copy offline.
		next := filepath.Join(root, fmt.Sprintf("data-%d", round+1))
		copyDir(t, dir, next)
		code, out := verifyDir(t, next)
		if code != exitSound && code != exitRepairable {
			t.Fatalf("round %d: verify exit %d on the copied directory:\n%s", round, code, out)
		}
		var rep struct {
			Repairable []string `json:"repairable"`
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("round %d: verify -json: %v\n%s", round, err, out)
		}
		reasons := rep.Repairable
		if len(reasons) > 3 {
			reasons = append(reasons[:3:3], fmt.Sprintf("and %d more", len(rep.Repairable)-3))
		}
		t.Logf("round %d: verify on the copy: exit %d %q", round, code, reasons)
		verifyCodes = append(verifyCodes, code)
		dir = next
	}
	t.Logf("verify exit codes after each kill: %v; in-flight requests found written: %d, not written: %d", verifyCodes, kept, lost)

	// The cleanly stopped copy is sound and restarts to the same balances.
	if code, out := verifyDir(t, dir); code != exitSound {
		t.Fatalf("verify after clean shutdown: exit %d\n%s", code, out)
	}
	p := spawn(t, dir)
	compareBalances(t, p.base, accounts, clients)
	checkInvariants(t, p.base)
	p.stop(t)
	total := 0
	for _, c := range clients {
		total += c.acked
	}
	if total < rounds*perRound {
		t.Fatalf("only %d requests acknowledged", total)
	}
}
