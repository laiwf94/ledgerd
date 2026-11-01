package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

// served writes a small ledger through the HTTP API and stops the server.
func served(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	addr, stop, _ := start(t, map[string]string{"LEDGERD_ADDR": "127.0.0.1:0", "LEDGERD_API_KEYS": key, "LEDGERD_DATA_DIR": dir,
		"LEDGERD_SNAPSHOT_EVERY": "3"})
	base := "http://" + addr
	call(t, "POST", base+"/v1/accounts", `{"id":"cash","currency":"EUR","normal_balance":"debit"}`)
	call(t, "POST", base+"/v1/accounts", `{"id":"owner","currency":"EUR","normal_balance":"credit"}`)
	for i := 0; i < 5; i++ {
		if code, body := call(t, "POST", base+"/v1/transfers", `{"currency":"EUR","postings":[{"account":"cash","direction":"debit","amount":250},{"account":"owner","direction":"credit","amount":250}]}`); code != 201 {
			t.Fatalf("transfer: %d %s", code, body)
		}
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runVerify(t *testing.T, e map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = verify(args, env(e), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVerifyCommand(t *testing.T) {
	dir := served(t)

	code, out, _ := runVerify(t, map[string]string{"LEDGERD_DATA_DIR": dir})
	if code != exitSound || !strings.Contains(out, "result: sound") || !strings.Contains(out, "journal head 5: 2 accounts, 5 transfers") ||
		!strings.Contains(out, "equals the log at record") {
		t.Fatalf("sound: %d\n%s", code, out)
	}

	code, out, _ = runVerify(t, nil, "-json", dir)
	var rep ledger.VerifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != exitSound || rep.Status != ledger.StatusSound || rep.Head != 5 {
		t.Fatalf("json: %d %v\n%s", code, err, out)
	}

	seg := filepath.Join(dir, "wal", "00000000000000000001.seg")
	f, _ := os.OpenFile(seg, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("0bad")
	f.Close()
	if code, out, _ = runVerify(t, nil, dir); code != exitRepairable || !strings.Contains(out, "repairable: segment") ||
		!strings.Contains(out, "torn tail of 4 bytes") {
		t.Fatalf("repairable: %d\n%s", code, out)
	}

	data, _ := os.ReadFile(seg)
	data[12] ^= 1
	os.WriteFile(seg, data, 0o600)
	if code, out, _ = runVerify(t, nil, dir); code != exitDamaged || !strings.Contains(out, "DAMAGED") || !strings.Contains(out, "result: damaged") {
		t.Fatalf("damaged: %d\n%s", code, out)
	}
}

func TestVerifyCommandErrors(t *testing.T) {
	dir := t.TempDir()
	_, stop, _ := start(t, map[string]string{"LEDGERD_ADDR": "127.0.0.1:0", "LEDGERD_API_KEYS": key, "LEDGERD_DATA_DIR": dir})
	if code, _, errOut := runVerify(t, nil, dir); code != exitError || !strings.Contains(errOut, "in use") {
		t.Fatalf("in use: %d %s", code, errOut)
	}
	stop()

	cases := map[string][]string{
		"no directory given":     nil,
		"usage: ledgerd verify":  {"a", "b"},
		"flag provided but not":  {"-bogus"},
		"no ledger found":        {t.TempDir()},
		"no such file or direct": {filepath.Join(t.TempDir(), "missing")},
	}
	for want, args := range cases {
		if code, _, errOut := runVerify(t, nil, args...); code != exitError || !strings.Contains(errOut, want) {
			t.Errorf("%v: %d %q, want %q", args, code, errOut, want)
		}
	}
	if code, _, errOut := runVerify(t, nil, "-h"); code != exitSound || !strings.Contains(errOut, "Exit status: 0 sound") {
		t.Fatalf("-h: %d %s", code, errOut)
	}
}
