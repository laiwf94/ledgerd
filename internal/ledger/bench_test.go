package ledger

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
)

// benchTransfers runs b.N single-phase transfers from `clients` concurrent
// goroutines. With persist set, every acknowledged transfer is fsynced.
func benchTransfers(b *testing.B, persist bool, maxBatch, clients int) {
	opts := Options{MaxBatch: maxBatch, SweepInterval: -1}
	if persist {
		opts.Dir = b.TempDir()
	}
	l, err := Open(opts)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	if _, err := l.Do(ctx, openAcct("source", "USD", Debit), nil); err != nil {
		b.Fatal(err)
	}
	const sinks = 64
	for i := 0; i < sinks; i++ {
		if _, err := l.Do(ctx, openAcct(fmt.Sprintf("sink%d", i), "USD", Credit), nil); err != nil {
			b.Fatal(err)
		}
	}
	syncs := l.Stats().LogSyncs
	var next atomic.Int64
	var wg sync.WaitGroup
	b.ResetTimer()
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1)
				if i > int64(b.N) {
					return
				}
				if _, err := l.Do(ctx, move(dr("source", 1), cr(fmt.Sprintf("sink%d", i%sinks), 1)), nil); err != nil {
					b.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "transfers/s")
	if persist {
		if n := l.Stats().LogSyncs - syncs; n > 0 {
			b.ReportMetric(float64(b.N)/float64(n), "transfers/fsync")
		}
	}
}

// BenchmarkGroupCommit compares one fsync per transfer (MaxBatch=1) with
// group commit, at increasing numbers of concurrent clients.
func BenchmarkGroupCommit(b *testing.B) {
	for _, clients := range []int{1, 16, 64, 256} {
		for _, batch := range []int{1, DefaultMaxBatch} {
			name := "fsync-per-write"
			if batch > 1 {
				name = "group-commit"
			}
			b.Run(fmt.Sprintf("%s/clients=%d", name, clients), func(b *testing.B) { benchTransfers(b, true, batch, clients) })
		}
	}
}

// BenchmarkInMemory measures the state machine and writer without the disk.
func BenchmarkInMemory(b *testing.B) {
	b.Run("clients=1", func(b *testing.B) { benchTransfers(b, false, DefaultMaxBatch, 1) })
	b.Run("clients=64", func(b *testing.B) { benchTransfers(b, false, DefaultMaxBatch, 64) })
}

func BenchmarkAccountRead(b *testing.B) {
	l, err := Open(Options{SweepInterval: -1})
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	l.Do(ctx, openAcct("a", "USD", Debit), nil)
	l.Do(ctx, openAcct("b", "USD", Credit), nil)
	for i := 0; i < 1000; i++ {
		l.Do(ctx, move(dr("a", 1), cr("b", 1)), nil)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := l.Account("a", 500); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// writeBenchLog appends records for single-phase transfers from seq first
// to last (inclusive) between 1,000 accounts, creating the log with its
// header and the accounts when first is 1.
func writeBenchLog(b *testing.B, path string, first, last uint64) {
	b.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	at := time.Unix(1_700_000_000, 0).UTC()
	put := func(r record) {
		p, err := encodeRecord(r)
		if err != nil {
			b.Fatal(err)
		}
		w.Write(wal.Encode(nil, p))
	}
	const accounts = 1000
	if first == 1 {
		put(record{Type: recHeader, Version: logVersion})
		for i := 0; i < accounts; i++ {
			c := openAcct(fmt.Sprintf("acct%04d", i), "USD", Credit)
			put(record{Type: recCmd, Time: at, Cmd: &c})
		}
	}
	for seq := first; seq <= last; seq++ {
		from, to := seq%accounts, (seq*7+1)%accounts
		if from == to {
			to = (to + 1) % accounts
		}
		c := move(dr(fmt.Sprintf("acct%04d", from), 1+int64(seq%100)), cr(fmt.Sprintf("acct%04d", to), 1+int64(seq%100)))
		put(record{Type: recCmd, Time: at, Seq: seq, ID: fmt.Sprintf("tr_%012d", seq), Cmd: &c})
	}
	if err := w.Flush(); err != nil {
		b.Fatal(err)
	}
}

func copyFile(b *testing.B, src, dst string) {
	b.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkRecovery measures Open on a ledger of 1 million journal entries:
// replaying the whole log, loading a snapshot taken at the head, and loading
// that snapshot plus a tail of 10,000 entries written after it.
//
//	go test -run=^$ -bench=Recovery -benchtime=3x ./internal/ledger/
func BenchmarkRecovery(b *testing.B) {
	const entries, tail = 1_000_000, 10_000
	full := b.TempDir()
	writeBenchLog(b, filepath.Join(full, logName), 1, entries)

	snap := b.TempDir()
	copyFile(b, filepath.Join(full, logName), filepath.Join(snap, logName))
	l, err := Open(Options{Dir: snap, SweepInterval: -1})
	if err != nil {
		b.Fatal(err)
	}
	if err := l.Close(); err != nil { // a clean shutdown writes the snapshot
		b.Fatal(err)
	}
	withTail := b.TempDir()
	for _, name := range []string{logName, snapName(1)} {
		copyFile(b, filepath.Join(snap, name), filepath.Join(withTail, name))
	}
	writeBenchLog(b, filepath.Join(withTail, logName), entries+1, entries+tail)

	for _, c := range []struct {
		name string
		dir  string
		head uint64
	}{{"full-replay", full, entries}, {"snapshot", snap, entries}, {"snapshot+10k-tail", withTail, entries + tail}} {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				l, err := Open(Options{Dir: c.dir, SweepInterval: -1, SnapshotEvery: -1})
				if err != nil {
					b.Fatal(err)
				}
				if l.Head() != c.head {
					b.Fatalf("head %d, want %d", l.Head(), c.head)
				}
				l.Close()
			}
			b.ReportMetric(float64(c.head), "entries")
		})
	}
}
