package api

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

// BenchmarkBatchHTTP measures POST /v1/transfers/batch with batches of 1,
// 100 and 8,190 transfers, and POST /v1/transfers for comparison, through
// the handler (authentication, JSON decoding, the ledger, the response)
// without a network, against an in-memory ledger. One client submits back
// to back. It reports transfers per second and, per transfer, the time,
// heap allocations and bytes, with request and response bytes.
//
//	go test -run=^$ -bench=BatchHTTP -benchtime=3s ./internal/api/
func BenchmarkBatchHTTP(b *testing.B) {
	b.Run("transfer", func(b *testing.B) { benchBatchHTTP(b, 1, true) })
	for _, size := range []int{1, 100, ledger.MaxBatchTransfers} {
		b.Run(fmt.Sprintf("batch=%d", size), func(b *testing.B) { benchBatchHTTP(b, size, false) })
	}
}

func benchBatchHTTP(b *testing.B, size int, plain bool) {
	const sinks = 64
	l, err := ledger.Open(ledger.Options{SweepInterval: -1, SnapshotEvery: -1})
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	s, err := New(Config{Ledger: l, APIKeys: []string{apiKey}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		b.Fatal(err)
	}
	send := func(path string, body []byte, want int) int {
		req := httptest.NewRequest("POST", path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != want {
			b.Fatalf("%s: %d %s", path, w.Code, w.Body.Bytes())
		}
		return w.Body.Len()
	}
	send("/v1/accounts", []byte(`{"id":"source","currency":"USD","normal_balance":"debit"}`), http.StatusCreated)
	for i := 0; i < sinks; i++ {
		send("/v1/accounts", []byte(fmt.Sprintf(`{"id":"sink%d","currency":"USD","normal_balance":"credit"}`, i)), http.StatusCreated)
	}
	// Bodies are built before the timer, one per command.
	path := "/v1/transfers/batch"
	if plain {
		path = "/v1/transfers"
	}
	bodies := make([][]byte, b.N)
	k := 0
	var sent int64
	for i := range bodies {
		items := make([]string, size)
		for j := range items {
			items[j] = xfer("source", fmt.Sprintf("sink%d", k%sinks), 1, "")
			k++
		}
		if plain {
			bodies[i] = []byte(items[0])
		} else {
			bodies[i] = []byte(batchBody(items...))
		}
		sent += int64(len(bodies[i]))
	}
	head := l.Head()
	var received int64
	var before, after runtime.MemStats
	runtime.GC()
	b.ReportAllocs()
	b.ResetTimer()
	runtime.ReadMemStats(&before)
	for i := 0; i < b.N; i++ {
		received += int64(send(path, bodies[i], http.StatusCreated))
		bodies[i] = nil
	}
	runtime.ReadMemStats(&after)
	b.StopTimer()
	transfers := float64(b.N) * float64(size)
	if got := l.Head() - head; got != uint64(transfers) {
		b.Fatalf("head moved by %d entries, want %v", got, transfers)
	}
	b.ReportMetric(transfers/b.Elapsed().Seconds(), "transfers/s")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/transfers, "ns/transfer")
	b.ReportMetric(float64(after.Mallocs-before.Mallocs)/transfers, "allocs/transfer")
	b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc)/transfers, "B/transfer")
	b.ReportMetric(float64(sent)/transfers, "req-B/transfer")
	b.ReportMetric(float64(received)/transfers, "resp-B/transfer")
}
