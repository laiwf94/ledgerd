package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/webhook"
)

// exampleReceiver runs the examples/webhook binary and keeps every log line
// of every process it started, so counts survive a kill.
type exampleReceiver struct {
	bin, addr, out, secret string

	mu    sync.Mutex
	cmd   *exec.Cmd
	done  chan error
	lines []map[string]any
}

func (r *exampleReceiver) start(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		cmd := exec.Command(r.bin, "-addr", r.addr, "-out", r.out, "-secret", r.secret)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		listening := make(chan struct{})
		go func() {
			sc := bufio.NewScanner(stdout)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			once := sync.Once{}
			for sc.Scan() {
				var line map[string]any
				if json.Unmarshal(sc.Bytes(), &line) != nil {
					continue
				}
				r.mu.Lock()
				r.lines = append(r.lines, line)
				r.mu.Unlock()
				if line["msg"] == "listening" {
					once.Do(func() { close(listening) })
				}
			}
			io.Copy(io.Discard, stdout)
		}()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-listening:
			r.mu.Lock()
			r.cmd, r.done = cmd, done
			r.mu.Unlock()
			return
		case err := <-done:
			// The port may still be held by the killed process for a moment.
			if time.Now().After(deadline) || !strings.Contains(stderr.String(), "address already in use") {
				t.Fatalf("the example receiver exited before listening: %v\n%s", err, &stderr)
			}
			time.Sleep(50 * time.Millisecond)
		case <-time.After(20 * time.Second):
			cmd.Process.Kill()
			t.Fatalf("the example receiver did not start\n%s", &stderr)
		}
	}
}

func (r *exampleReceiver) signal(t *testing.T, sig syscall.Signal) error {
	t.Helper()
	r.mu.Lock()
	cmd, done := r.cmd, r.done
	r.cmd = nil
	r.mu.Unlock()
	if cmd == nil {
		return nil
	}
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("the example receiver did not exit after %v", sig)
		return nil
	}
}

// sums adds up a numeric field over the log lines with the given message,
// from line from on, and returns it with the number of lines it is in.
func (r *exampleReceiver) sums(msg, field string, from int) (sum, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.lines[min(from, len(r.lines)):] {
		if l["msg"] == msg {
			n++
			if v, ok := l[field].(float64); ok {
				sum += int(v)
			}
		}
	}
	return sum, n
}

func (r *exampleReceiver) logLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

func (r *exampleReceiver) find(msg string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, l := range r.lines {
		if l["msg"] == msg {
			out = append(out, l)
		}
	}
	return out
}

type hookState struct {
	Cursor      uint64              `json:"delivered_through_seq"`
	DeadLetters []webhookDeadLetter `json:"dead_letters"`
	HeadSeq     uint64              `json:"head_seq"`
}

type webhookDeadLetter struct {
	ID         uint64 `json:"id"`
	AfterSeq   uint64 `json:"after_seq"`
	ThroughSeq uint64 `json:"through_seq"`
}

func hookStateOf(t *testing.T, base, id string) hookState {
	t.Helper()
	var s hookState
	if err := json.Unmarshal(get(t, base+"/v1/webhooks/"+id), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestWebhookExampleReceiverAcrossCrashes builds examples/webhook and runs
// it as the receiver of a real ledgerd while writers post transfers. In
// each of three rounds the receiver is killed with SIGKILL while
// deliveries are in flight and kept down until the server has dead-lettered
// a delivery, then restarted on its file; after it has applied more, the
// server is killed with SIGKILL too. Throughout, forged deliveries (wrong
// secret, stale timestamp, tampered body, no signature) are sent to the
// receiver and must be refused with 401. Then the dead letters are
// replayed and the position is moved back to 0, so the whole journal comes
// again. The receiver's file must hold every journal entry exactly once,
// identical to the feed, in sequence order apart from the replayed dead
// letters, and the full redelivery must apply nothing.
func TestWebhookExampleReceiverAcrossCrashes(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("go is needed to build examples/webhook: %v", err)
		}
		t.Skipf("go not installed: %v", err)
	}
	tmp := t.TempDir()
	r := &exampleReceiver{bin: filepath.Join(tmp, "webhook-example"), out: filepath.Join(tmp, "entries.ndjson"),
		secret: "whsec_example_e2e_0123456789abcdef"}
	build := exec.Command(goBin, "build", "-o", r.bin, "../../examples/webhook")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build examples/webhook: %v\n%s", err, out)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.addr = ln.Addr().String()
	ln.Close()
	r.start(t)
	t.Cleanup(func() { r.signal(t, syscall.SIGKILL) })

	// Forged deliveries: each must be refused, and none of its entries
	// (sequence 900000000 and up) may reach the file.
	var forged atomic.Int64
	forge := func(t *testing.T) {
		now := time.Now()
		for i, sign := range []func(body []byte) string{
			func(b []byte) string { return webhook.Sign("whsec_not_the_endpoint_secret_0123", now, b) },
			func(b []byte) string { return webhook.Sign(r.secret, now.Add(-10*time.Minute), b) },
			func(b []byte) string {
				return webhook.Sign(r.secret, now, bytes.Replace(b, []byte("900000"), []byte("900001"), 1))
			},
			func([]byte) string { return "" },
		} {
			seq := 900000000 + i
			id := fmt.Sprintf("example.%d.%d", seq, seq)
			body := fmt.Sprintf(`{"id":%q,"endpoint":"example","entries":[{"seq":%d,"kind":"posted","lines":[]}]}`, id, seq)
			req, _ := http.NewRequest("POST", "http://"+r.addr+"/hook", strings.NewReader(body))
			req.Header.Set(webhook.SignatureHeader, sign([]byte(body)))
			req.Header.Set(webhook.IDHeader, id)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("forged delivery %d: %v", i, err)
				continue
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("forged delivery %d: %d, want 401", i, resp.StatusCode)
			}
			forged.Add(1)
		}
	}

	env := []string{"LEDGERD_WEBHOOK_TIMEOUT=1s", "LEDGERD_WEBHOOK_BACKOFF=5ms", "LEDGERD_WEBHOOK_BACKOFF_MAX=40ms",
		"LEDGERD_WEBHOOK_MAX_ATTEMPTS=4", "LEDGERD_SNAPSHOT_EVERY=300"}
	accounts := []string{"cash", "bank", "fees", "alice", "bob"}
	dir := filepath.Join(tmp, "data")
	var p *proc
	var keyN atomic.Int64
	deadLettered := 0
	const rounds = 3
	for round := 0; round < rounds; round++ {
		p = spawn(t, dir, env...)
		if round == 0 {
			for i, a := range accounts {
				if _, err := send(p.base, "POST", "/v1/accounts", fmt.Sprintf(`{"id":%q,"currency":"EUR","normal_balance":%q}`, a, []string{"debit", "credit"}[i%2]), ""); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := send(p.base, "POST", "/v1/webhooks", fmt.Sprintf(`{"id":"example","url":%q,"secret":%q,"max_batch":25,"after_seq":0}`,
				"http://"+r.addr+"/hook", r.secret), "example-hook"); err != nil {
				t.Fatal(err)
			}
		}
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < 3; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					from, to := accounts[(w+i)%len(accounts)], accounts[(w+i+1+i%3)%len(accounts)]
					body := fmt.Sprintf(`{"currency":"EUR","postings":[{"account":%q,"direction":"debit","amount":%d},{"account":%q,"direction":"credit","amount":%d}]}`,
						from, 1+i%997, to, 1+i%997)
					if _, err := send(p.base, "POST", "/v1/transfers", body, fmt.Sprintf("example-%d", keyN.Add(1))); err != nil {
						return // the server was killed
					}
				}
			}()
		}
		waitApplied := func(what string, n int) {
			t.Helper()
			from, _ := r.sums("delivery", "applied", 0)
			deadline := time.Now().Add(time.Minute)
			for {
				if got, _ := r.sums("delivery", "applied", 0); got >= from+n {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("round %d: the receiver applied nothing %s", round, what)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		waitApplied("before the receiver was killed", 100)
		r.signal(t, syscall.SIGKILL)
		// Down until the server gives up on a delivery.
		deadline := time.Now().Add(time.Minute)
		for len(hookStateOf(t, p.base, "example").DeadLetters) <= deadLettered {
			if time.Now().After(deadline) {
				t.Fatalf("round %d: no delivery was dead-lettered while the receiver was down", round)
			}
			time.Sleep(10 * time.Millisecond)
		}
		deadLettered = len(hookStateOf(t, p.base, "example").DeadLetters)
		r.start(t)
		forge(t)
		waitApplied("after its restart", 100)
		p.kill(t)
		close(stop)
		wg.Wait()
		t.Logf("round %d: %d transfers sent, %d dead letters", round, keyN.Load(), deadLettered)
	}

	p = spawn(t, dir, env...)
	defer p.stop(t)
	forge(t)
	caughtUp := func(what string) hookState {
		t.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for {
			s := hookStateOf(t, p.base, "example")
			if s.Cursor == s.HeadSeq {
				return s
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the endpoint is at %d of %d", what, s.Cursor, s.HeadSeq)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	s := caughtUp("after the last restart")
	dls := s.DeadLetters
	if len(dls) == 0 {
		t.Fatal("no dead letters to replay")
	}
	appliedBefore, _ := r.sums("delivery", "applied", 0)
	if _, err := send(p.base, "POST", "/v1/webhooks/example/replay", `{"all_dead_letters":true}`, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for len(hookStateOf(t, p.base, "example").DeadLetters) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("dead letters left after the replay: %+v", hookStateOf(t, p.base, "example").DeadLetters)
		}
		time.Sleep(20 * time.Millisecond)
	}
	appliedAfter, _ := r.sums("delivery", "applied", 0)

	seqs, entries := feed(t, p.base, url.Values{})
	checkExampleFile(t, r.out, seqs, entries, dls)
	before, err := os.ReadFile(r.out)
	if err != nil {
		t.Fatal(err)
	}

	// The position moves back to 0: the whole journal comes again, as
	// ordinary deliveries, and the receiver must apply none of it.
	mark := r.logLen()
	if _, err := send(p.base, "POST", "/v1/webhooks/example/replay", `{"after_seq":0}`, ""); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Minute)
	for {
		dups, _ := r.sums("delivery", "duplicates", mark)
		if dups >= len(seqs) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the receiver saw %d of %d entries again", dups, len(seqs))
		}
		time.Sleep(20 * time.Millisecond)
	}
	caughtUp("after moving the position back")
	if applied, n := r.sums("delivery", "applied", mark); applied != 0 {
		t.Fatalf("the full redelivery applied %d entries in %d deliveries", applied, n)
	}
	after, _ := os.ReadFile(r.out)
	if !bytes.Equal(before, after) {
		t.Fatal("the full redelivery changed the receiver's file")
	}

	if err := r.signal(t, syscall.SIGTERM); err != nil {
		t.Fatalf("the receiver did not stop cleanly: %v", err)
	}
	stopped := r.find("stopped")
	if len(stopped) != 1 || int(stopped[0]["entries"].(float64)) != len(seqs) {
		t.Fatalf("stopped lines: %v, want one with %d entries", stopped, len(seqs))
	}
	refused, nRefused := r.sums("refused", "status", 0)
	if int64(nRefused) != forged.Load() || refused != 401*nRefused {
		t.Fatalf("the receiver refused %d deliveries (status sum %d); %d were forged", nRefused, refused, forged.Load())
	}
	var truncated int
	for _, l := range r.find("recovered") {
		truncated += int(l["truncated_bytes"].(float64))
	}
	dups, deliveries := r.sums("delivery", "duplicates", 0)
	t.Logf("%d entries in %d deliveries: %d applied from %d replayed dead letters, %d received again and skipped; %d forged deliveries refused; %d receiver starts cut %d torn bytes",
		len(seqs), deliveries, appliedAfter-appliedBefore, len(dls), dups, nRefused, len(r.find("recovered")), truncated)
}

// checkExampleFile holds the receiver's file against the journal: every
// entry exactly once, identical to the feed, and in sequence order except
// for entries inside a dead letter, which were replayed after later ones.
func checkExampleFile(t *testing.T, path string, seqs []uint64, entries []json.RawMessage, dls []webhookDeadLetter) {
	t.Helper()
	pos := make(map[uint64]int, len(seqs))
	for i, s := range seqs {
		pos[s] = i
	}
	inDeadLetter := func(s uint64) bool {
		for _, d := range dls {
			if s > d.AfterSeq && s <= d.ThroughSeq {
				return true
			}
		}
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	seen := make([]bool, len(seqs))
	var last uint64
	n := 0
	for sc.Scan() {
		n++
		var e struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %d: %v", n, err)
		}
		p, ok := pos[e.Seq]
		if !ok {
			t.Fatalf("line %d: entry %d is not in the journal", n, e.Seq)
		}
		if seen[p] {
			t.Fatalf("line %d: entry %d applied twice", n, e.Seq)
		}
		seen[p] = true
		if !sameJSON(sc.Bytes(), entries[p]) {
			t.Fatalf("line %d: entry %d is %s, the journal has %s", n, e.Seq, sc.Bytes(), entries[p])
		}
		if !inDeadLetter(e.Seq) {
			if e.Seq <= last {
				t.Fatalf("line %d: entry %d after %d", n, e.Seq, last)
			}
			last = e.Seq
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	for i, ok := range seen {
		if !ok {
			t.Fatalf("entry %d never reached the receiver's file (%d of %d did)", seqs[i], n, len(seqs))
		}
	}
}
