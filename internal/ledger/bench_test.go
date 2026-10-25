package ledger

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
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
