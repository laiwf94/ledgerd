// Command ledgerd runs the double-entry ledger service.
//
//	ledgerd                  serve the HTTP API
//	ledgerd verify [dir]     check a data directory offline (see ledgerd verify -h)
//	ledgerd verify-export f  check an exported journal file (see ledgerd verify-export -h)
//
// Configuration comes from the environment:
//
//	LEDGERD_ADDR             listen address (default ":8080")
//	LEDGERD_DATA_DIR         directory for the log; empty keeps data in memory only
//	LEDGERD_API_KEYS         comma-separated bearer keys, each at least 16 characters (required)
//	LEDGERD_ADMIN_TOKEN      bearer token for /v1/admin; when unset, admin endpoints answer loopback clients only
//	LEDGERD_CURSOR_SECRET    signs listing cursors, at least 16 characters; when unset the key is derived from LEDGERD_API_KEYS
//	LEDGERD_IDEMPOTENCY_TTL  how long idempotency keys are kept, e.g. 24h (default 24h, at most 720h)
//	LEDGERD_MAX_BATCH        most commands sharing one fsync (default 1024; 1 disables group commit)
//	LEDGERD_SNAPSHOT_EVERY   log records between snapshots (default 100000; 0 disables snapshots)
//	LEDGERD_SEGMENT_BYTES    size at which a log segment is sealed (default 67108864; 4096 to 4294967296)
//	LEDGERD_WEBHOOK_TIMEOUT       time one webhook delivery attempt may take (default 10s, 1s to 5m)
//	LEDGERD_WEBHOOK_BACKOFF       delay after a delivery's first failed attempt, doubling after each (default 1s)
//	LEDGERD_WEBHOOK_BACKOFF_MAX   longest delay between attempts (default 1h)
//	LEDGERD_WEBHOOK_MAX_ATTEMPTS  attempts before a delivery becomes a dead letter (default 20, 1 to 1000)
//	LEDGERD_EXPORT_KEY       Ed25519 seed signing export manifests, 32 bytes of base64; when unset the key is
//	                         <data dir>/export.key, made on first start, or a key that lasts until exit without a data dir
//	LEDGERD_LOG_LEVEL        debug, info, warn or error (default info)
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/laiwf94/ledgerd/internal/api"
	"github.com/laiwf94/ledgerd/internal/export"
	"github.com/laiwf94/ledgerd/internal/ledger"
	"github.com/laiwf94/ledgerd/internal/metrics"
	"github.com/laiwf94/ledgerd/internal/webhook"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "verify" {
		os.Exit(verify(os.Args[2:], os.Getenv, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-export" {
		os.Exit(verifyExport(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv, os.Stdout, nil); err != nil {
		fmt.Fprintln(os.Stderr, "ledgerd:", err)
		os.Exit(1)
	}
}

type config struct {
	addr       string
	dataDir    string
	apiKeys    []string
	adminToken string
	cursorKey  string
	exportSeed []byte
	idemTTL    time.Duration
	maxBatch   int
	snapEvery  int
	segBytes   int64
	logLevel   slog.Level

	hookTimeout, hookBackoff, hookBackoffMax time.Duration
	hookAttempts                             int
}

const maxIdempotencyTTL = 30 * 24 * time.Hour

func loadConfig(getenv func(string) string) (config, error) {
	c := config{addr: ":8080", idemTTL: ledger.DefaultIdempotencyTTL, maxBatch: ledger.DefaultMaxBatch,
		snapEvery: ledger.DefaultSnapshotEvery, segBytes: ledger.DefaultSegmentBytes,
		hookTimeout: webhook.DefaultTimeout, hookBackoff: webhook.DefaultBackoffBase, hookBackoffMax: webhook.DefaultBackoffMax,
		hookAttempts: webhook.DefaultMaxAttempts}
	if v := getenv("LEDGERD_ADDR"); v != "" {
		c.addr = v
	}
	c.dataDir = getenv("LEDGERD_DATA_DIR")
	for _, k := range strings.Split(getenv("LEDGERD_API_KEYS"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			c.apiKeys = append(c.apiKeys, k)
		}
	}
	if len(c.apiKeys) == 0 {
		return c, errors.New("LEDGERD_API_KEYS is required (comma-separated, each at least 16 characters)")
	}
	c.adminToken = getenv("LEDGERD_ADMIN_TOKEN")
	c.cursorKey = getenv("LEDGERD_CURSOR_SECRET")
	if c.cursorKey != "" && len(c.cursorKey) < 16 {
		return c, errors.New("LEDGERD_CURSOR_SECRET must be at least 16 characters")
	}
	if v := getenv("LEDGERD_EXPORT_KEY"); v != "" {
		seed, err := export.ParseSeed(v)
		if err != nil {
			return c, errors.New("LEDGERD_EXPORT_KEY must be a 32-byte Ed25519 seed in base64")
		}
		c.exportSeed = seed
	}
	if v := getenv("LEDGERD_IDEMPOTENCY_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute || d > maxIdempotencyTTL {
			return c, errors.New("LEDGERD_IDEMPOTENCY_TTL must be a duration between 1m and 720h")
		}
		c.idemTTL = d
	}
	if v := getenv("LEDGERD_MAX_BATCH"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65536 {
			return c, errors.New("LEDGERD_MAX_BATCH must be between 1 and 65536")
		}
		c.maxBatch = n
	}
	if v := getenv("LEDGERD_SNAPSHOT_EVERY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100_000_000 {
			return c, errors.New("LEDGERD_SNAPSHOT_EVERY must be between 0 (off) and 100000000")
		}
		c.snapEvery = n
		if n == 0 {
			c.snapEvery = -1
		}
	}
	if v := getenv("LEDGERD_SEGMENT_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 4096 || n > 1<<32 {
			return c, errors.New("LEDGERD_SEGMENT_BYTES must be between 4096 and 4294967296")
		}
		c.segBytes = n
	}
	for _, d := range []struct {
		name     string
		dst      *time.Duration
		min, max time.Duration
	}{
		{"LEDGERD_WEBHOOK_TIMEOUT", &c.hookTimeout, time.Second, 5 * time.Minute},
		{"LEDGERD_WEBHOOK_BACKOFF", &c.hookBackoff, time.Millisecond, time.Hour},
		{"LEDGERD_WEBHOOK_BACKOFF_MAX", &c.hookBackoffMax, time.Millisecond, 24 * time.Hour},
	} {
		if v := getenv(d.name); v != "" {
			n, err := time.ParseDuration(v)
			if err != nil || n < d.min || n > d.max {
				return c, fmt.Errorf("%s must be a duration between %s and %s", d.name, d.min, d.max)
			}
			*d.dst = n
		}
	}
	if c.hookBackoffMax < c.hookBackoff {
		return c, errors.New("LEDGERD_WEBHOOK_BACKOFF_MAX must not be shorter than LEDGERD_WEBHOOK_BACKOFF")
	}
	if v := getenv("LEDGERD_WEBHOOK_MAX_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return c, errors.New("LEDGERD_WEBHOOK_MAX_ATTEMPTS must be between 1 and 1000")
		}
		c.hookAttempts = n
	}
	if v := getenv("LEDGERD_LOG_LEVEL"); v != "" {
		if err := c.logLevel.UnmarshalText([]byte(v)); err != nil {
			return c, fmt.Errorf("LEDGERD_LOG_LEVEL: %v", err)
		}
	}
	return c, nil
}

// exportSigner picks the key that signs export manifests: LEDGERD_EXPORT_KEY,
// else the data directory's export.key (made on first start), else a key
// that lasts until exit.
func exportSigner(cfg config, log *slog.Logger) (*export.Signer, error) {
	var s *export.Signer
	var err error
	source := "LEDGERD_EXPORT_KEY"
	switch {
	case cfg.exportSeed != nil:
		s, err = export.NewSigner(cfg.exportSeed)
	case cfg.dataDir != "":
		var created bool
		path := filepath.Join(cfg.dataDir, export.KeyFile)
		s, created, err = export.LoadOrCreateKey(path)
		source = path
		if created {
			source += " (new)"
		}
	default:
		seed := make([]byte, export.SeedSize)
		_, _ = rand.Read(seed)
		s, err = export.NewSigner(seed)
		source = "random until exit"
	}
	if err != nil {
		return nil, fmt.Errorf("export signing key: %w", err)
	}
	log.Info("export signing key", "key_id", s.KeyID(), "public_key", export.EncodeKey(s.PublicKey()), "source", source)
	return s, nil
}

// utcTime logs timestamps in UTC, like every time the ledger stores.
func utcTime(groups []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey && len(groups) == 0 {
		a.Value = slog.TimeValue(a.Value.Time().UTC())
	}
	return a
}

// run serves until ctx ends. ready, when set, receives the bound address.
func run(ctx context.Context, getenv func(string) string, logOut io.Writer, ready func(addr string)) error {
	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	// Reject bad secrets before opening storage, which can take seconds to recover.
	if err := api.CheckSecrets(cfg.apiKeys, cfg.adminToken); err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: cfg.logLevel, ReplaceAttr: utcTime}))
	if cfg.dataDir == "" {
		log.Warn("LEDGERD_DATA_DIR not set; the ledger lives in memory and is lost on exit")
	}
	reg := metrics.New()
	start := time.Now()
	l, err := ledger.Open(ledger.Options{Dir: cfg.dataDir, IdempotencyTTL: cfg.idemTTL, MaxBatch: cfg.maxBatch,
		SnapshotEvery: cfg.snapEvery, SegmentBytes: cfg.segBytes, Logger: log, Metrics: reg})
	if err != nil {
		return fmt.Errorf("opening ledger: %w", err)
	}
	defer l.Close()
	stats := l.Stats()
	log.Info("recovered", "head", stats.Head, "accounts", stats.Accounts, "transfers", stats.Transfers,
		"open_pending", stats.OpenPending, "log_bytes", stats.LogBytes, "log_segments", stats.LogSegments, "duration_ms", time.Since(start).Milliseconds())

	signer, err := exportSigner(cfg, log)
	if err != nil {
		return err
	}

	hooks, err := webhook.Start(webhook.Options{Ledger: l, Timeout: cfg.hookTimeout, BackoffBase: cfg.hookBackoff,
		BackoffMax: cfg.hookBackoffMax, MaxAttempts: cfg.hookAttempts, Logger: log, Metrics: reg})
	if err != nil {
		return err
	}
	defer hooks.Close() // before the ledger closes: a delivery in flight is sent again on the next start

	srv, err := api.New(api.Config{Ledger: l, APIKeys: cfg.apiKeys, AdminToken: cfg.adminToken, CursorSecret: cfg.cursorKey,
		ExportSigner: signer, Logger: log, Metrics: reg})
	if err != nil {
		return err
	}
	hs := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      api.DefaultTimeout + 15*time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	// Shutdown waits for handlers; live entry streams end so it can finish.
	hs.RegisterOnShutdown(srv.CloseStreams)
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return err
	}
	log.Info("listening", "addr", ln.Addr().String(), "persistent", cfg.dataDir != "", "admin_token", cfg.adminToken != "")
	if ready != nil {
		ready(ln.Addr().String())
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Stop taking requests first, then let the ledger finish queued commands.
	return hs.Shutdown(shutdownCtx)
}
