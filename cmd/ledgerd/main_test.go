package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfig(t *testing.T) {
	c, err := loadConfig(env(map[string]string{"LEDGERD_API_KEYS": " k1-aaaaaaaaaaaaaaaa , ,k2-bbbbbbbbbbbbbbbb"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.addr != ":8080" || len(c.apiKeys) != 2 || c.idemTTL != ledger.DefaultIdempotencyTTL || c.maxBatch != ledger.DefaultMaxBatch || c.adminToken != "" {
		t.Fatalf("defaults wrong: %+v", c)
	}
	c, err = loadConfig(env(map[string]string{"LEDGERD_API_KEYS": "k", "LEDGERD_ADDR": ":9", "LEDGERD_DATA_DIR": "/d",
		"LEDGERD_ADMIN_TOKEN": "t", "LEDGERD_IDEMPOTENCY_TTL": "1h", "LEDGERD_MAX_BATCH": "1", "LEDGERD_LOG_LEVEL": "debug"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.addr != ":9" || c.dataDir != "/d" || c.adminToken != "t" || c.idemTTL != time.Hour || c.maxBatch != 1 || c.logLevel.String() != "DEBUG" {
		t.Fatalf("overrides wrong: %+v", c)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"LEDGERD_API_KEYS is required":     {},
		"LEDGERD_IDEMPOTENCY_TTL must be":  {"LEDGERD_API_KEYS": "k", "LEDGERD_IDEMPOTENCY_TTL": "5s"},
		"LEDGERD_IDEMPOTENCY_TTL must be ": {"LEDGERD_API_KEYS": "k", "LEDGERD_IDEMPOTENCY_TTL": "soon"},
		"LEDGERD_MAX_BATCH must be":        {"LEDGERD_API_KEYS": "k", "LEDGERD_MAX_BATCH": "0"},
		"LEDGERD_LOG_LEVEL":                {"LEDGERD_API_KEYS": "k", "LEDGERD_LOG_LEVEL": "loud"},
	}
	for want, e := range cases {
		if _, err := loadConfig(env(e)); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("env %v: err = %v, want %q", e, err, want)
		}
	}
}

const key = "integration-key-0123"

func start(t *testing.T, e map[string]string) (addr string, stop func() error, logs *bytes.Buffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan string, 1)
	done := make(chan error, 1)
	logs = &bytes.Buffer{}
	go func() { done <- run(ctx, env(e), logs, func(a string) { addrc <- a }) }()
	select {
	case addr = <-addrc:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	return addr, func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(25 * time.Second):
			t.Fatal("server did not stop")
			return nil
		}
	}, logs
}

func call(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRunServesPersistsAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	e := map[string]string{"LEDGERD_ADDR": "127.0.0.1:0", "LEDGERD_API_KEYS": key, "LEDGERD_DATA_DIR": dir}
	addr, stop, logs := start(t, e)
	base := "http://" + addr
	if code, body := call(t, "POST", base+"/v1/accounts", `{"id":"cash","currency":"EUR","normal_balance":"debit"}`); code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, _ := call(t, "POST", base+"/v1/accounts", `{"id":"owner","currency":"EUR","normal_balance":"credit"}`); code != 201 {
		t.Fatal(code)
	}
	if code, body := call(t, "POST", base+"/v1/transfers", `{"currency":"EUR","postings":[{"account":"cash","direction":"debit","amount":250},{"account":"owner","direction":"credit","amount":250}]}`); code != 201 {
		t.Fatalf("transfer: %d %s", code, body)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"msg":"shutting down"`) {
		t.Fatalf("logs: %s", logs.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "ledger.wal")); err != nil {
		t.Fatal(err)
	}

	// A restart recovers the balance from the log.
	addr, stop, logs = start(t, e)
	code, body := call(t, "GET", "http://"+addr+"/v1/accounts/cash", "")
	if code != 200 || !strings.Contains(body, `"posted":250`) {
		t.Fatalf("after restart: %d %s", code, body)
	}
	if !strings.Contains(logs.String(), `"head":1`) || !strings.Contains(logs.String(), `Z","level"`) {
		t.Fatalf("recovery not logged: %s", logs.String())
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestRunErrors(t *testing.T) {
	if err := run(context.Background(), env(map[string]string{}), io.Discard, nil); err == nil {
		t.Fatal("missing keys accepted")
	}
	if err := run(context.Background(), env(map[string]string{"LEDGERD_API_KEYS": "short"}), io.Discard, nil); err == nil {
		t.Fatal("short key accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	if err := run(context.Background(), env(map[string]string{"LEDGERD_API_KEYS": key, "LEDGERD_DATA_DIR": file}), io.Discard, nil); err == nil {
		t.Fatal("bad data dir accepted")
	}
	if err := run(context.Background(), env(map[string]string{"LEDGERD_API_KEYS": key, "LEDGERD_ADDR": "256.0.0.1:x"}), io.Discard, nil); err == nil {
		t.Fatal("bad address accepted")
	}
	addr, stop, _ := start(t, map[string]string{"LEDGERD_ADDR": "127.0.0.1:0", "LEDGERD_API_KEYS": key})
	if code, _ := call(t, "GET", "http://"+addr+"/healthz", ""); code != 200 {
		t.Fatal(code)
	}
	stop()
}
