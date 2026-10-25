// Command ledgerd runs the double-entry ledger service.
//
// Configuration comes from the environment:
//
//	LEDGERD_ADDR             listen address (default ":8080")
//	LEDGERD_DATA_DIR         directory for the log; empty keeps data in memory only
//	LEDGERD_API_KEYS         comma-separated bearer keys, each at least 16 characters (required)
//	LEDGERD_ADMIN_TOKEN      bearer token for /v1/admin; when unset, admin endpoints answer loopback clients only
//	LEDGERD_IDEMPOTENCY_TTL  how long idempotency keys are kept, e.g. 24h (default 24h, at most 720h)
//	LEDGERD_MAX_BATCH        most commands sharing one fsync (default 1024; 1 disables group commit)
//	LEDGERD_LOG_LEVEL        debug, info, warn or error (default info)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/laiwf94/ledgerd/internal/api"
	"github.com/laiwf94/ledgerd/internal/ledger"
	"github.com/laiwf94/ledgerd/internal/metrics"
)

func main() {
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
	idemTTL    time.Duration
	maxBatch   int
	logLevel   slog.Level
}

const maxIdempotencyTTL = 30 * 24 * time.Hour

func loadConfig(getenv func(string) string) (config, error) {
	c := config{addr: ":8080", idemTTL: ledger.DefaultIdempotencyTTL, maxBatch: ledger.DefaultMaxBatch}
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
	if v := getenv("LEDGERD_LOG_LEVEL"); v != "" {
		if err := c.logLevel.UnmarshalText([]byte(v)); err != nil {
			return c, fmt.Errorf("LEDGERD_LOG_LEVEL: %v", err)
		}
	}
	return c, nil
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
	log := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: cfg.logLevel, ReplaceAttr: utcTime}))
	if cfg.dataDir == "" {
		log.Warn("LEDGERD_DATA_DIR not set; the ledger lives in memory and is lost on exit")
	}
	reg := metrics.New()
	start := time.Now()
	l, err := ledger.Open(ledger.Options{Dir: cfg.dataDir, IdempotencyTTL: cfg.idemTTL, MaxBatch: cfg.maxBatch,
		Logger: log, Metrics: reg})
	if err != nil {
		return fmt.Errorf("opening ledger: %w", err)
	}
	defer l.Close()
	stats := l.Stats()
	log.Info("recovered", "head", stats.Head, "accounts", stats.Accounts, "transfers", stats.Transfers,
		"open_pending", stats.OpenPending, "log_bytes", stats.LogBytes, "duration_ms", time.Since(start).Milliseconds())

	srv, err := api.New(api.Config{Ledger: l, APIKeys: cfg.apiKeys, AdminToken: cfg.adminToken, Logger: log, Metrics: reg})
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
