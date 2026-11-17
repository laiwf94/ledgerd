package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/webhook"
)

// hookFault is what a fault-injecting receiver does with one request.
type hookFault int

const (
	faultNone        hookFault = iota // verify, record, answer 200
	faultStatus                       // answer 503 without reading the body
	faultResetEarly                   // reset the connection before reading the body
	faultPartialRead                  // read part of the body, then reset
	faultDropReply                    // read and record the body, then close without answering
	faultSlow                         // read and record the body, answer 200 after the sender's timeout
	nFaults
)

var faultNames = [nFaults]string{"none", "503", "reset before reading", "reset mid-body", "accepted then dropped", "answered after the timeout"}

// hookArrival is one delivery a receiver read in full and whose signature
// checked out, in the order deliveries arrived.
type hookArrival struct {
	id      string
	seqs    []uint64
	entries []json.RawMessage
	fault   hookFault
}

// hookReceiver is a webhook endpoint behind a faulty network. It can be
// taken down (its listener closed and every open connection cut, so the
// sender meets connection refused) and brought back on the same address.
type hookReceiver struct {
	name    string
	secret  string
	filter  url.Values // the endpoint's filter, as a feed query
	addr    string
	mu      sync.Mutex
	rng     *rand.Rand
	srv     *http.Server
	faulty  bool // faults are injected while set
	arrived []hookArrival
	byID    map[string][]uint64
	faults  [nFaults]int
	errs    []string
}

func newHookReceiver(t *testing.T, name string, seed uint64, filter url.Values) *hookReceiver {
	r := &hookReceiver{name: name, secret: "whsec_e2e_0123456789abcdef_" + name, filter: filter, rng: rand.New(rand.NewPCG(seed, 7)),
		faulty: true, byID: map[string][]uint64{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.addr = ln.Addr().String()
	r.serve(ln)
	t.Cleanup(r.down)
	return r
}

func (r *hookReceiver) url() string { return "http://" + r.addr + "/hook" }

func (r *hookReceiver) serve(ln net.Listener) {
	srv := &http.Server{Handler: http.HandlerFunc(r.handle), ReadHeaderTimeout: 10 * time.Second}
	r.mu.Lock()
	r.srv = srv
	r.mu.Unlock()
	go srv.Serve(ln)
}

// down closes the listener and cuts every open connection.
func (r *hookReceiver) down() {
	r.mu.Lock()
	srv := r.srv
	r.srv = nil
	r.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
}

// up listens on the same address again. It may run on any goroutine, so
// a failure is recorded rather than ending the test.
func (r *hookReceiver) up() {
	deadline := time.Now().Add(10 * time.Second)
	for {
		ln, err := net.Listen("tcp", r.addr)
		if err == nil {
			r.serve(ln)
			return
		}
		if time.Now().After(deadline) {
			r.fail("listening on %s again: %v", r.addr, err)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *hookReceiver) setFaulty(on bool) {
	r.mu.Lock()
	r.faulty = on
	r.mu.Unlock()
}

func (r *hookReceiver) pick() hookFault {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.faulty {
		return faultNone
	}
	switch n := r.rng.IntN(100); {
	case n < 14:
		return faultStatus
	case n < 20:
		return faultResetEarly
	case n < 25:
		return faultPartialRead
	case n < 33:
		return faultDropReply
	case n < 35:
		return faultSlow
	}
	return faultNone
}

func (r *hookReceiver) fail(format string, args ...any) {
	r.mu.Lock()
	r.errs = append(r.errs, fmt.Sprintf("receiver %s: ", r.name)+fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

// reset hijacks the connection and closes it with a TCP reset.
func reset(w http.ResponseWriter) {
	c, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetLinger(0)
	}
	c.Close()
}

func (r *hookReceiver) handle(w http.ResponseWriter, req *http.Request) {
	f := r.pick()
	r.mu.Lock()
	r.faults[f]++
	r.mu.Unlock()
	switch f {
	case faultStatus:
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	case faultResetEarly:
		reset(w)
		return
	case faultPartialRead:
		io.CopyN(io.Discard, req.Body, 64)
		reset(w)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		// The sender gave up mid-body (it was killed or timed out).
		return
	}
	if err := webhook.Verify(r.secret, req.Header.Get(webhook.SignatureHeader), body, time.Now(), 5*time.Minute); err != nil {
		r.fail("signature: %v", err)
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	var d struct {
		ID       string            `json:"id"`
		Endpoint string            `json:"endpoint"`
		Replay   bool              `json:"replay"`
		Entries  []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		r.fail("body: %v", err)
		return
	}
	a := hookArrival{id: d.ID, entries: d.Entries, fault: f}
	for _, e := range d.Entries {
		var v struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(e, &v); err != nil {
			r.fail("entry: %v", err)
			return
		}
		a.seqs = append(a.seqs, v.Seq)
	}
	switch {
	case d.Endpoint != r.name || d.Replay:
		r.fail("delivery %s is for endpoint %q (replay %v)", d.ID, d.Endpoint, d.Replay)
	case len(a.seqs) == 0:
		r.fail("delivery %s has no entries", d.ID)
	case req.Header.Get(webhook.IDHeader) != d.ID || d.ID != fmt.Sprintf("%s.%d.%d", r.name, a.seqs[0], a.seqs[len(a.seqs)-1]):
		r.fail("delivery %s arrived under ID header %q", d.ID, req.Header.Get(webhook.IDHeader))
	}
	r.mu.Lock()
	if prev, ok := r.byID[d.ID]; ok && !reflect.DeepEqual(prev, a.seqs) {
		r.errs = append(r.errs, fmt.Sprintf("receiver %s: delivery %s carried %v, and %v before", r.name, d.ID, a.seqs, prev))
	}
	r.byID[d.ID] = a.seqs
	r.arrived = append(r.arrived, a)
	r.mu.Unlock()
	switch f {
	case faultDropReply:
		reset(w)
	case faultSlow:
		select {
		case <-time.After(1300 * time.Millisecond): // past LEDGERD_WEBHOOK_TIMEOUT=1s
		case <-req.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// snapshot returns what has arrived so far.
func (r *hookReceiver) snapshot() ([]hookArrival, [nFaults]int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hookArrival(nil), r.arrived...), r.faults, append([]string(nil), r.errs...)
}

// distinct is the number of different entries received.
func (r *hookReceiver) distinct() int {
	arrived, _, _ := r.snapshot()
	seen := map[uint64]bool{}
	for _, a := range arrived {
		for _, s := range a.seqs {
			seen[s] = true
		}
	}
	return len(seen)
}

// feed reads every journal entry matching filter, in order.
func feed(t *testing.T, base string, filter url.Values) (seqs []uint64, entries []json.RawMessage) {
	t.Helper()
	after := uint64(0)
	for {
		q := url.Values{}
		for k, v := range filter {
			q[k] = v
		}
		q.Set("after_seq", fmt.Sprint(after))
		q.Set("limit", "1000")
		var page struct {
			Entries      []json.RawMessage `json:"entries"`
			NextAfterSeq uint64            `json:"next_after_seq"`
			HasMore      bool              `json:"has_more"`
		}
		if err := json.Unmarshal(get(t, base+"/v1/entries?"+q.Encode()), &page); err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Entries {
			var v struct {
				Seq uint64 `json:"seq"`
			}
			json.Unmarshal(e, &v)
			seqs = append(seqs, v.Seq)
			entries = append(entries, e)
		}
		after = page.NextAfterSeq
		if !page.HasMore {
			return seqs, entries
		}
	}
}

func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// checkDelivery holds a receiver's arrivals against the journal: every
// entry matching the endpoint's filter arrived at least once, nothing else
// arrived, each arrived entry equals the journal's, and the arrivals are in
// order: each delivery is a run of consecutive matching entries that starts
// no later than just after the furthest entry already received, so no entry
// is ever received before one that precedes it. It returns how many entries
// arrived more than once, and how many of those had already been answered
// 200 (sent again because a kill came before the position was logged). With complete unset, entries that have not
// arrived yet are not reported, only the order of those that have.
func checkDelivery(t *testing.T, r *hookReceiver, seqs []uint64, entries []json.RawMessage, complete bool) (dups, afterOK int) {
	t.Helper()
	pos := make(map[uint64]int, len(seqs))
	for i, s := range seqs {
		pos[s] = i
	}
	arrived, _, errs := r.snapshot()
	for _, e := range errs {
		t.Error(e)
	}
	frontier := -1
	seen := make([]bool, len(seqs))
	answered := make([]bool, len(seqs))
	for n, a := range arrived {
		for i, s := range a.seqs {
			p, ok := pos[s]
			if !ok {
				t.Fatalf("receiver %s: delivery %d (%s) carried entry %d, which is not in the journal or does not match the filter", r.name, n, a.id, s)
			}
			if i == 0 && p > frontier+1 {
				t.Fatalf("receiver %s: delivery %d (%s) starts at entry %d but the furthest entry received before it was %d: entries %v were skipped",
					r.name, n, a.id, s, frontierSeq(seqs, frontier), seqs[frontier+1:p])
			}
			if i > 0 && p != pos[a.seqs[i-1]]+1 {
				t.Fatalf("receiver %s: delivery %d (%s) carries %d after %d, not the next matching entry", r.name, n, a.id, s, a.seqs[i-1])
			}
			if !sameJSON(a.entries[i], entries[p]) {
				t.Fatalf("receiver %s: entry %d arrived as %s but the journal has %s", r.name, s, a.entries[i], entries[p])
			}
			if seen[p] {
				dups++
				if answered[p] {
					afterOK++
				}
			}
			seen[p] = true
			answered[p] = answered[p] || a.fault == faultNone
			frontier = max(frontier, p)
		}
	}
	for i, ok := range seen {
		if !ok && complete {
			t.Fatalf("receiver %s: entry %d never arrived (%d of %d did)", r.name, seqs[i], r.distinct(), len(seqs))
		}
	}
	return dups, afterOK
}

func frontierSeq(seqs []uint64, frontier int) uint64 {
	if frontier < 0 {
		return 0
	}
	return seqs[frontier]
}

// TestWebhookDeliveryUnderKillNineAndNetworkFaults runs a real ledgerd
// process with four webhook endpoints (everything, one account's entries,
// hold entries only, and everything in batches of one) behind receivers
// that inject network faults: 503s, connection resets before and during
// the request body, deliveries accepted and then left unanswered, answers
// that come after the sender's timeout, and whole outages where the
// receiver stops listening. Concurrent clients write transfers, holds,
// posts, voids and reversals, and the server is killed with SIGKILL in
// each round while deliveries are in flight, once while one receiver is
// down. After the last restart the faults stop. Every entry must then have
// reached every endpoint whose filter it matches, at least once and in
// order per endpoint, with valid signatures, nothing that is not in the
// journal, no dead letters, and every acknowledged request still held.
func TestWebhookDeliveryUnderKillNineAndNetworkFaults(t *testing.T) {
	const rounds, nClients, perRound = 3, 4, 400
	accounts := []string{"cash", "bank", "clearing", "fees", "alice", "bob"}
	receivers := []*hookReceiver{
		newHookReceiver(t, "all", 1, url.Values{}),
		newHookReceiver(t, "alice", 2, url.Values{"account": {"alice"}}),
		newHookReceiver(t, "holds", 3, url.Values{"kind": {"pending,post_pending,void_pending"}}),
		newHookReceiver(t, "single", 4, url.Values{}),
	}
	create := []string{
		`{"id":"all","url":%q,"secret":%q,"max_batch":40}`,
		`{"id":"alice","url":%q,"secret":%q,"account":"alice","max_batch":10}`,
		`{"id":"holds","url":%q,"secret":%q,"kinds":["pending","post_pending","void_pending"],"max_batch":7}`,
		`{"id":"single","url":%q,"secret":%q,"max_batch":1}`,
	}
	env := []string{"LEDGERD_WEBHOOK_TIMEOUT=1s", "LEDGERD_WEBHOOK_BACKOFF=5ms", "LEDGERD_WEBHOOK_BACKOFF_MAX=40ms",
		"LEDGERD_WEBHOOK_MAX_ATTEMPTS=1000", "LEDGERD_SNAPSHOT_EVERY=300"}
	clients := make([]*e2eClient, nClients)
	for i := range clients {
		clients[i] = &e2eClient{n: i, rng: rand.New(rand.NewPCG(uint64(i), 27)), accounts: accounts, reversed: map[int]bool{}}
	}
	dir := t.TempDir()

	var p *proc
	for round := 0; round <= rounds; round++ {
		p = spawn(t, dir, env...)
		t.Logf("round %d: recovered head %v", round, p.recovered["head"])
		if round == 0 {
			for i, a := range accounts {
				normal := []string{"debit", "credit"}[i%2]
				if _, err := send(p.base, "POST", "/v1/accounts", fmt.Sprintf(`{"id":%q,"currency":"EUR","normal_balance":%q}`, a, normal), ""); err != nil {
					t.Fatal(err)
				}
			}
			for i, r := range receivers {
				if _, err := send(p.base, "POST", "/v1/webhooks", fmt.Sprintf(create[i], r.url(), r.secret), "hook-"+r.name); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			replayAcknowledged(t, p.base, clients)
			compareBalances(t, p.base, accounts, clients)
		}
		if round == 2 {
			// The "alice" receiver was taken down before the last kill;
			// the restarted server must keep retrying until it is back.
			time.Sleep(300 * time.Millisecond)
			receivers[1].up()
		}
		if round == rounds {
			break
		}

		before := make([]int, len(receivers))
		for i, r := range receivers {
			arrived, _, _ := r.snapshot()
			before[i] = len(arrived)
		}
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
		// Outages: one receiver at a time stops listening for a while.
		stopOutages := make(chan struct{})
		outages := make(chan struct{})
		go func() {
			defer close(outages)
			rng := rand.New(rand.NewPCG(uint64(round), 99))
			for {
				r := receivers[rng.IntN(len(receivers))]
				if round == 1 && r == receivers[1] {
					r = receivers[0] // kept for the outage across the kill
				}
				select {
				case <-stopOutages:
					return
				case <-time.After(time.Duration(50+rng.IntN(150)) * time.Millisecond):
				}
				r.down()
				time.Sleep(time.Duration(50+rng.IntN(200)) * time.Millisecond)
				r.up()
			}
		}()
		// Kill once enough has been written and every receiver has had new
		// deliveries this round, so the kill lands with deliveries going.
		deadline := time.Now().Add(3 * time.Minute)
		for {
			ready := acked.Load() >= perRound
			for i, r := range receivers {
				arrived, _, _ := r.snapshot()
				if len(arrived) < before[i]+5 {
					ready = false
				}
			}
			if ready {
				break
			}
			if time.Now().After(deadline) {
				p.cmd.Process.Kill()
				t.Fatalf("round %d: deliveries did not move after %d requests", round, acked.Load())
			}
			time.Sleep(10 * time.Millisecond)
		}
		close(stopOutages)
		<-outages
		if round == 1 {
			receivers[1].down()
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
		t.Logf("round %d: killed after %d acknowledged requests", round, acked.Load())
	}

	// The network heals. Every endpoint's position must reach the last
	// entry its filter matches; a position moves only after the receiver
	// answered 2xx, and a receiver records a delivery before answering.
	for _, r := range receivers {
		r.setFaulty(false)
	}
	want := make([][]uint64, len(receivers))
	wantEntries := make([][]json.RawMessage, len(receivers))
	last := map[string]uint64{}
	for i, r := range receivers {
		want[i], wantEntries[i] = feed(t, p.base, r.filter)
		if len(want[i]) == 0 {
			t.Fatalf("receiver %s: no entries match its filter", r.name)
		}
		last[r.name] = want[i][len(want[i])-1]
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var list struct {
			Webhooks []struct {
				ID          string `json:"id"`
				Cursor      uint64 `json:"delivered_through_seq"`
				DeadLetters int    `json:"dead_letter_count"`
			} `json:"webhooks"`
			HeadSeq uint64 `json:"head_seq"`
		}
		if err := json.Unmarshal(get(t, p.base+"/v1/webhooks"), &list); err != nil {
			t.Fatal(err)
		}
		if len(list.Webhooks) != len(receivers) {
			t.Fatalf("%d endpoints registered", len(list.Webhooks))
		}
		caught := true
		for _, h := range list.Webhooks {
			if h.DeadLetters != 0 {
				t.Fatalf("endpoint %s has %d dead letters", h.ID, h.DeadLetters)
			}
			if h.Cursor > list.HeadSeq {
				t.Fatalf("endpoint %s is at %d, past the head %d", h.ID, h.Cursor, list.HeadSeq)
			}
			if h.Cursor < last[h.ID] {
				caught = false
			}
		}
		if caught {
			break
		}
		if time.Now().After(deadline) {
			for i, r := range receivers {
				t.Logf("receiver %s: %d of %d entries", r.name, r.distinct(), len(want[i]))
				checkDelivery(t, r, want[i], wantEntries[i], false)
			}
			t.Fatalf("positions did not catch up: %+v", list)
		}
		time.Sleep(20 * time.Millisecond)
	}

	var total [nFaults]int
	for i, r := range receivers {
		dups, afterOK := checkDelivery(t, r, want[i], wantEntries[i], true)
		arrived, faults, _ := r.snapshot()
		for f, n := range faults {
			total[f] += n
		}
		t.Logf("receiver %s: %d entries in %d deliveries read, %d entries received more than once, %d of them after a 200",
			r.name, len(want[i]), len(arrived), dups, afterOK)
	}
	var parts []string
	for f := faultStatus; f < nFaults; f++ {
		if total[f] == 0 {
			t.Errorf("no request met the %q fault", faultNames[f])
		}
		parts = append(parts, fmt.Sprintf("%s %d", faultNames[f], total[f]))
	}
	t.Logf("requests by fault: %s, none %d", strings.Join(parts, ", "), total[faultNone])
	checkInvariants(t, p.base)
	p.stop(t)
	if code, out := verifyDir(t, dir); code != exitSound {
		t.Fatalf("verify after clean shutdown: exit %d\n%s", code, out)
	}
}
