package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

// sseEvent is one dispatched Server-Sent Event, or a retry field.
type sseEvent struct {
	id, event, data string
	hasID           bool
	retry           string
}

// sseStream reads events from a live entry stream.
type sseStream struct {
	t      testing.TB
	res    *http.Response
	cancel context.CancelFunc
	events chan sseEvent
	done   chan error // the read error that ended the stream
}

// openStream connects to /v1/entries/stream. lastID, when not empty, is
// sent as Last-Event-ID.
func (f *fixture) openStream(query, lastID string) *sseStream {
	f.t.Helper()
	s, status, body := openStreamAt(f.t, f.srv.URL, query, lastID)
	if s == nil {
		f.t.Fatalf("GET /v1/entries/stream?%s: %d %s", query, status, body)
	}
	return s
}

func openStreamAt(t testing.TB, base, query, lastID string) (*sseStream, int, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v1/entries/stream?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set(lastEventIDHeader, lastID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		cancel()
		return nil, res.StatusCode, string(b)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	s := &sseStream{t: t, res: res, cancel: cancel, events: make(chan sseEvent, 1<<16), done: make(chan error, 1)}
	go s.read()
	t.Cleanup(s.close)
	return s, 200, ""
}

// read parses the stream as the EventSource specification does.
func (s *sseStream) read() {
	r := bufio.NewReader(s.res.Body)
	var ev sseEvent
	var data []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			close(s.events)
			s.done <- err
			return
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			if ev.retry != "" {
				s.events <- sseEvent{retry: ev.retry}
			}
			if len(data) > 0 || ev.hasID {
				ev.data = strings.Join(data, "\n")
				ev.retry = ""
				s.events <- ev
			}
			ev, data = sseEvent{}, nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		name, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "id":
			ev.id, ev.hasID = value, true
		case "event":
			ev.event = value
		case "data":
			data = append(data, value)
		case "retry":
			ev.retry = value
		default:
			s.t.Errorf("unknown field in %q", line)
		}
	}
}

func (s *sseStream) close() {
	s.cancel()
	s.res.Body.Close()
}

// next returns the next event, failing after a timeout.
func (s *sseStream) next() sseEvent {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			s.t.Fatalf("the stream ended: %v", <-s.done)
		}
		return ev
	case <-time.After(20 * time.Second):
		s.t.Fatal("no event within 20s")
	}
	return sseEvent{}
}

// entries reads entry events until it has n, skipping heartbeats, and
// returns their sequences. Each event's id must be its entry's sequence.
func (s *sseStream) entries(n int) []uint64 {
	s.t.Helper()
	var out []uint64
	for len(out) < n {
		ev := s.next()
		if ev.event != "entry" {
			continue
		}
		var e ledger.Entry
		if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
			s.t.Fatalf("entry %q: %v", ev.data, err)
		}
		if ev.id != strconv.FormatUint(e.Seq, 10) {
			s.t.Fatalf("event id %q for entry %d", ev.id, e.Seq)
		}
		out = append(out, e.Seq)
	}
	return out
}

func seqRange(from, to uint64) []uint64 {
	var out []uint64
	for s := from; s <= to; s++ {
		out = append(out, s)
	}
	return out
}

func sameSeqs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (f *fixture) pay(from, to string, amount int) {
	f.t.Helper()
	f.must(201, "POST", "/v1/transfers", fmt.Sprintf(`{"currency":"USD","postings":[{"account":%q,"direction":"debit","amount":%d},`+
		`{"account":%q,"direction":"credit","amount":%d}]}`, from, amount, to, amount))
}

func TestEntryStreamDeliversAndResumes(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	for i := 0; i < 300; i++ {
		f.pay("bank", "alice", 10)
	}
	// History first, across several pages of the stream.
	s := f.openStream("", "")
	if ev := s.next(); ev.retry != strconv.FormatInt(streamRetry.Milliseconds(), 10) {
		t.Fatalf("first event %+v, want the retry delay", ev)
	}
	if got := s.entries(300); !sameSeqs(got, seqRange(1, 300)) {
		t.Fatalf("history %v", got)
	}
	// Then live entries as they commit.
	for i := 0; i < 5; i++ {
		f.pay("alice", "merchant", 1)
		if got := s.entries(1); got[0] != uint64(301+i) {
			t.Fatalf("live entry %v, want %d", got, 301+i)
		}
	}
	s.close()

	// Commits while disconnected arrive after a reconnect with
	// Last-Event-ID, which wins over the after_seq of the original URL.
	for i := 0; i < 20; i++ {
		f.pay("bank", "alice", 1)
	}
	s = f.openStream("after_seq=0", "305")
	if got := s.entries(20); !sameSeqs(got, seqRange(306, 325)) {
		t.Fatalf("after reconnecting %v", got)
	}
	s.close()

	// after_seq alone, and filters.
	s = f.openStream("after_seq=320", "")
	if got := s.entries(5); !sameSeqs(got, seqRange(321, 325)) {
		t.Fatalf("after_seq=320: %v", got)
	}
	s = f.openStream("account=merchant", "")
	if got := s.entries(5); !sameSeqs(got, seqRange(301, 305)) {
		t.Fatalf("account=merchant: %v", got)
	}
	f.pay("bank", "alice", 1)
	f.pay("alice", "merchant", 1)
	if got := s.entries(1); got[0] != 327 {
		t.Fatalf("live merchant entry %v", got)
	}
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","pending":true,"timeout_seconds":600,"postings":[`+
		`{"account":"alice","direction":"debit","amount":5},{"account":"merchant","direction":"credit","amount":5}]}`)
	s = f.openStream("kind=pending&currency=USD&after_seq=1", "")
	if got := s.entries(1); got[0] != 328 {
		t.Fatalf("kind=pending: %v", got)
	}
}

func TestEntryStreamHeartbeatCarriesPosition(t *testing.T) {
	f := newFixture(t, func(_ *ledger.Options, c *Config) { c.StreamHeartbeat = 20 * time.Millisecond })
	f.accounts()
	// A filter that matches nothing hears only heartbeats, and their ids
	// follow the head, so a resume does not rescan.
	s := f.openStream("account=merchant", "")
	if ev := s.next(); ev.retry == "" {
		t.Fatalf("first event %+v", ev)
	}
	for i := 0; i < 10; i++ {
		f.pay("bank", "alice", 1)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		ev := s.next()
		if ev.event != "heartbeat" {
			t.Fatalf("event %+v on a stream nothing matches", ev)
		}
		var hb struct {
			HeadSeq uint64 `json:"head_seq"`
		}
		if err := json.Unmarshal([]byte(ev.data), &hb); err != nil || !ev.hasID {
			t.Fatalf("heartbeat %+v: %v", ev, err)
		}
		if id, _ := strconv.ParseUint(ev.id, 10, 64); id > hb.HeadSeq {
			t.Fatalf("heartbeat id %s past head %d", ev.id, hb.HeadSeq)
		}
		if ev.id == "10" && hb.HeadSeq == 10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heartbeats never reached the head: %+v", ev)
		}
	}
	// Resuming from the heartbeat's id delivers the next match only.
	f.pay("alice", "merchant", 1)
	s = f.openStream("account=merchant", "10")
	if got := s.entries(1); got[0] != 11 {
		t.Fatalf("after the heartbeat %v", got)
	}
}

// Several streams follow concurrent writers and each sees every entry
// exactly once and in order, including one that reconnects midway.
func TestEntryStreamFollowsConcurrentWrites(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.pay("bank", "alice", 1_000_000)
	const writers, each = 4, 100
	all := f.openStream("", "")
	bob := f.openStream("account=merchant", "")
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				to := "merchant"
				if (w+i)%2 == 0 {
					to = "alice"
				}
				body := fmt.Sprintf(`{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":1},`+
					`{"account":%q,"direction":"credit","amount":1}]}`, to)
				if r := f.call("POST", "/v1/transfers", body); r.status != 201 {
					t.Errorf("transfer: %d %s", r.status, r.body)
					return
				}
			}
		}(w)
	}
	// A third consumer drops after 50 entries and resumes.
	part := f.openStream("", "")
	first := part.entries(50)
	part.close()
	wg.Wait()
	total := 1 + writers*each
	rest := f.openStream("", strconv.FormatUint(first[len(first)-1], 10)).entries(total - 50)
	if got := append(first, rest...); !sameSeqs(got, seqRange(1, uint64(total))) {
		t.Fatalf("resumed consumer saw %d entries, not 1..%d once each", len(got), total)
	}
	if got := all.entries(total); !sameSeqs(got, seqRange(1, uint64(total))) {
		t.Fatalf("live consumer saw %d entries, not 1..%d once each", len(got), total)
	}
	want := f.follow("account=merchant", 0, 1000)
	if got := bob.entries(len(want)); !sameSeqs(got, want) {
		t.Fatalf("filtered consumer saw %v, feed has %v", got, want)
	}
}

// smallBuffers makes accepted connections send through a small socket
// buffer, so a consumer that stops reading fills it quickly.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4096)
	}
	return c, err
}

// stalledStream opens a stream on a raw connection whose client never
// reads.
func stalledStream(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.(*net.TCPConn).SetReadBuffer(4096)
	fmt.Fprintf(c, "GET /v1/entries/stream HTTP/1.1\r\nHost: ledgerd\r\nAuthorization: Bearer %s\r\n\r\n", apiKey)
	return c
}

func TestEntryStreamDisconnectsSlowConsumers(t *testing.T) {
	f := newFixture(t, func(_ *ledger.Options, c *Config) {
		c.StreamWriteTimeout = 200 * time.Millisecond
		c.MaxStreams = 1
	})
	f.accounts()
	// About 1.5 MB of events, far more than both socket buffers hold.
	var b strings.Builder
	b.WriteString(`{"transfers":[`)
	for i := 0; i < 4000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":1},{"account":"alice","direction":"credit","amount":1}]}`)
	}
	b.WriteString(`]}`)
	f.must(201, "POST", "/v1/transfers/batch", b.String())

	srv := httptest.NewUnstartedServer(f.api)
	srv.Listener = smallBuffers{srv.Listener}
	srv.Start()
	t.Cleanup(srv.Close)
	c := stalledStream(t, srv.Listener.Addr().String())

	slow := f.api.streams.mSlow
	deadline := time.Now().Add(20 * time.Second)
	for slow.Value() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a consumer that never reads was not disconnected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The server closed the connection: once the client drains what was
	// buffered, it reads the end.
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.Copy(io.Discard, c); err != nil {
		t.Fatalf("draining the closed stream: %v", err)
	}
	// Its slot is free again, and a reader that keeps up gets everything.
	for f.api.streams.open.Load() != 0 {
		time.Sleep(time.Millisecond)
	}
	s := f.openStream("", "")
	if got := s.entries(4000); !sameSeqs(got, seqRange(1, 4000)) {
		t.Fatalf("a reader that keeps up got %d entries", len(got))
	}
	if f.api.streams.mSlow.Value() != 1 {
		t.Fatalf("%d slow disconnects", f.api.streams.mSlow.Value())
	}
}

func TestEntryStreamLimitAndErrors(t *testing.T) {
	f := newFixture(t, func(_ *ledger.Options, c *Config) { c.MaxStreams = 2 })
	f.accounts()
	f.pay("bank", "alice", 1)
	a := f.openStream("", "")
	f.openStream("account=alice", "")
	r := f.call("GET", "/v1/entries/stream", "")
	if r.status != 503 || r.code() != "unavailable" {
		t.Fatalf("third stream: %d %s", r.status, r.body)
	}
	if f.api.streams.mRefused.Value() != 1 {
		t.Fatal("the refusal was not counted")
	}
	a.close()
	for f.api.streams.open.Load() != 1 {
		time.Sleep(time.Millisecond)
	}
	f.openStream("", "")

	for _, c := range []struct{ query, lastID string }{
		{"after_seq=2", ""},
		{"", "2"},
		{"", "x"},
		{"", "-1"},
		{"", "18446744073709551615"},
		{"limit=10", ""},
		{"cursor=abc", ""},
		{"after=1", ""},
		{"account=", ""},
		{"account=no%20such", ""},
		{"kind=posted,posted", ""},
		{"kind=open", ""},
		{"currency=usd", ""},
		{"after_seq=1&after_seq=1", ""},
	} {
		r := f.call("GET", "/v1/entries/stream?"+c.query, "", lastEventIDHeader, c.lastID)
		if r.status != 400 || r.code() != "invalid_argument" {
			t.Errorf("?%s with Last-Event-ID %q: %d %s", c.query, c.lastID, r.status, r.body)
		}
	}
	if r := f.call("GET", "/v1/entries/stream", "", "Authorization", ""); r.status != 401 {
		t.Fatalf("no key: %d", r.status)
	}
	var metrics strings.Builder
	if err := f.api.m.Write(&metrics); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ledgerd_entry_streams 2", "ledgerd_entry_stream_events_total", "ledgerd_entry_stream_refused_total 1",
		"ledgerd_entry_stream_slow_disconnects_total 0"} {
		if !strings.Contains(metrics.String(), name) {
			t.Errorf("metrics lack %q:\n%s", name, metrics.String())
		}
	}
}

// A stream outlives the server's read and write timeouts, and Shutdown
// ends it instead of waiting for it.
func TestEntryStreamUnderServerTimeoutsAndShutdown(t *testing.T) {
	f := newFixture(t, func(_ *ledger.Options, c *Config) { c.StreamHeartbeat = 50 * time.Millisecond })
	f.accounts()
	hs := &http.Server{Handler: f.api, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond}
	hs.RegisterOnShutdown(f.api.CloseStreams)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })
	base := "http://" + ln.Addr().String()
	s, status, body := openStreamAt(t, base, "", "")
	if s == nil {
		t.Fatalf("stream: %d %s", status, body)
	}
	start := time.Now()
	for time.Since(start) < time.Second {
		s.next()
	}
	f.pay("bank", "alice", 1)
	if got := s.entries(1); got[0] != 1 {
		t.Fatalf("entry after the timeouts: %v", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hs.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with an open stream: %v", err)
	}
	for range s.events {
	}
	if err := <-s.done; err != io.EOF {
		t.Fatalf("stream ended with %v", err)
	}
	// A closed server's handler refuses new streams.
	if r := f.call("GET", "/v1/entries/stream", ""); r.status != 503 {
		t.Fatalf("stream after CloseStreams: %d", r.status)
	}
}

// BenchmarkEntryStream measures a consumer catching up on 49,140 entries
// over HTTP, and a transfer's round trip from its request to its event
// arriving on a stream waiting at the head.
func BenchmarkEntryStream(b *testing.B) {
	f := newFixture(b, nil)
	f.accounts()
	var sb strings.Builder
	sb.WriteString(`{"transfers":[`)
	for i := 0; i < ledger.MaxBatchTransfers; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":1},{"account":"alice","direction":"credit","amount":1}]}`)
	}
	sb.WriteString(`]}`)
	const batches = 6
	for i := 0; i < batches; i++ {
		f.must(201, "POST", "/v1/transfers/batch", sb.String())
	}
	total := batches * ledger.MaxBatchTransfers
	b.Run("catch_up", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			s := f.openStream("", "")
			if got := s.entries(total); got[len(got)-1] != uint64(total) {
				b.Fatalf("last entry %d", got[len(got)-1])
			}
			s.close()
		}
		b.ReportMetric(float64(total)*float64(b.N)/b.Elapsed().Seconds(), "entries/s")
	})
	b.Run("live", func(b *testing.B) {
		s := f.openStream("", strconv.Itoa(total))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			f.pay("bank", "alice", 1)
			s.entries(1)
		}
	})
}
