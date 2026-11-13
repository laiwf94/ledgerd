package ledger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/laiwf94/ledgerd/internal/wal"
)

// batchBenchSinks is how many accounts a benchmark batch pays into, so a
// large batch touches many accounts the way a payout run does.
const batchBenchSinks = 64

// batchBenchCmds builds n commands, each a batch of size two-posting
// transfers from "source" to the sinks, or a plain create_transfer when
// plain is set (size must then be 1). They are built before the timer runs
// so the benchmark measures the ledger, not the benchmark.
func batchBenchCmds(n, size int, plain bool) []Command {
	cmds := make([]Command, n)
	k := 0
	for i := range cmds {
		if plain {
			cmds[i] = move(dr("source", 1), cr(fmt.Sprintf("sink%d", k%batchBenchSinks), 1))
			k++
			continue
		}
		b := &BatchSpec{Transfers: make([]TransferSpec, size)}
		for j := range b.Transfers {
			b.Transfers[j] = TransferSpec{Currency: "USD", Postings: []PostingSpec{
				dr("source", 1), cr(fmt.Sprintf("sink%d", k%batchBenchSinks), 1)}}
			k++
		}
		cmds[i] = Command{CreateBatch: b}
	}
	return cmds
}

// logSize sums the size of every segment in a ledger's log.
func logSize(b *testing.B, dir string) int64 {
	b.Helper()
	paths, err := wal.SegmentPaths(filepath.Join(dir, walDir))
	if err != nil {
		b.Fatal(err)
	}
	var n int64
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			b.Fatal(err)
		}
		n += fi.Size()
	}
	return n
}

// benchBatches submits b.N commands of size transfers each, one after
// another from one client, so every command is its own group commit: with
// persist set, one log write and one fsync per batch. It reports transfers
// per second and, per transfer, the time, heap allocations and bytes the
// whole ledger spent (writer and background goroutines included), plus log
// bytes per transfer on disk.
func benchBatches(b *testing.B, persist bool, size int, plain bool) {
	opts := Options{SweepInterval: -1, SnapshotEvery: -1}
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
	for i := 0; i < batchBenchSinks; i++ {
		if _, err := l.Do(ctx, openAcct(fmt.Sprintf("sink%d", i), "USD", Credit), nil); err != nil {
			b.Fatal(err)
		}
	}
	cmds := batchBenchCmds(b.N, size, plain)
	var logBefore int64
	if persist {
		logBefore = logSize(b, opts.Dir)
	}
	syncs, head := l.Stats().LogSyncs, l.Head()
	var before, after runtime.MemStats
	runtime.GC()
	b.ReportAllocs()
	b.ResetTimer()
	runtime.ReadMemStats(&before)
	for i := 0; i < b.N; i++ {
		out, err := l.Do(ctx, cmds[i], nil)
		if err != nil {
			b.Fatal(err)
		}
		if got := len(out.Transfers); !plain && got != size {
			b.Fatalf("batch committed %d transfers, want %d", got, size)
		}
		cmds[i] = Command{} // let the spec go once the ledger has it
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
	if persist {
		if n := l.Stats().LogSyncs - syncs; n != uint64(b.N) {
			b.Fatalf("%d fsyncs for %d commands, want one each", n, b.N)
		}
		b.ReportMetric(float64(logSize(b, opts.Dir)-logBefore)/transfers, "log-B/transfer")
	}
}

// BenchmarkBatch measures atomic batches of 1, 100 and 8,190 transfers
// against a plain create_transfer, in memory and on disk with an fsync per
// batch. One client submits back to back, so the numbers are what one
// caller gets from batching, not what group commit gets from many callers
// (BenchmarkGroupCommit). Snapshots are off, so the numbers are the commit
// path alone.
//
//	go test -run=^$ -bench=Batch -benchtime=3s ./internal/ledger/
func BenchmarkBatch(b *testing.B) {
	for _, persist := range []bool{false, true} {
		where := "memory"
		if persist {
			where = "disk"
		}
		b.Run(where+"/transfer", func(b *testing.B) { benchBatches(b, persist, 1, true) })
		for _, size := range []int{1, 100, MaxBatchTransfers} {
			b.Run(fmt.Sprintf("%s/batch=%d", where, size), func(b *testing.B) { benchBatches(b, persist, size, false) })
		}
	}
}
