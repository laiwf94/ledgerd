package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laiwf94/ledgerd/internal/export"
)

func get(t *testing.T, url string) []byte {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, b)
	}
	return b
}

func publicKey(t *testing.T, base string) string {
	t.Helper()
	var k struct {
		PublicKey string `json:"public_key"`
		KeyID     string `json:"key_id"`
	}
	if err := json.Unmarshal(get(t, base+"/v1/export/key"), &k); err != nil {
		t.Fatal(err)
	}
	return k.PublicKey
}

func runVerifyExport(t *testing.T, stdin []byte, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := verifyExport(args, bytes.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

// The server signs exports with a key kept in the data directory, so files
// exported before a restart still verify against the key it publishes
// after; ledgerd verify-export checks them.
func TestVerifyExportCommand(t *testing.T) {
	dir := t.TempDir()
	e := map[string]string{"LEDGERD_ADDR": "127.0.0.1:0", "LEDGERD_API_KEYS": key, "LEDGERD_DATA_DIR": dir}
	addr, stop, logs := start(t, e)
	base := "http://" + addr
	call(t, "POST", base+"/v1/accounts", `{"id":"cash","currency":"EUR","normal_balance":"debit"}`)
	call(t, "POST", base+"/v1/accounts", `{"id":"owner","currency":"EUR","normal_balance":"credit"}`)
	for i := 0; i < 3; i++ {
		if code, body := call(t, "POST", base+"/v1/transfers", `{"currency":"EUR","postings":[{"account":"cash","direction":"debit","amount":250},{"account":"owner","direction":"credit","amount":250}]}`); code != 201 {
			t.Fatalf("transfer: %d %s", code, body)
		}
	}
	nd := get(t, base+"/v1/export")
	cs := get(t, base+"/v1/export?format=csv")
	pub := publicKey(t, base)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"source":"`+filepath.Join(dir, export.KeyFile)+` (new)"`) {
		t.Fatalf("the new key was not logged:\n%s", logs)
	}
	if fi, err := os.Stat(filepath.Join(dir, export.KeyFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	// The data directory with its key is still sound.
	if code, out, _ := runVerify(t, nil, dir); code != exitSound {
		t.Fatalf("verify: %d\n%s", code, out)
	}
	addr, stop, _ = start(t, e)
	if got := publicKey(t, "http://"+addr); got != pub {
		t.Fatalf("the key changed across a restart: %s, was %s", got, pub)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	files := t.TempDir()
	ndPath, csPath, keyPath := filepath.Join(files, "x.ndjson"), filepath.Join(files, "x.csv"), filepath.Join(files, "key.pub")
	os.WriteFile(ndPath, nd, 0o600)
	os.WriteFile(csPath, cs, 0o600)
	os.WriteFile(keyPath, []byte(pub+"\n"), 0o600)

	if code, out, _ := runVerifyExport(t, nil, "-key", pub, ndPath); code != exportVerified ||
		!strings.Contains(out, "verified") || !strings.Contains(out, "3 entries in 3 rows") || !strings.Contains(out, "all time, head 3") {
		t.Fatalf("ndjson: %d\n%s", code, out)
	}
	if code, out, _ := runVerifyExport(t, nil, "-key-file", keyPath, csPath); code != exportVerified || !strings.Contains(out, "3 entries in 6 rows") {
		t.Fatalf("csv: %d\n%s", code, out)
	}
	if code, out, _ := runVerifyExport(t, cs, "-key", pub, "-"); code != exportVerified {
		t.Fatalf("stdin: %d\n%s", code, out)
	}
	if code, out, _ := runVerifyExport(t, nil, ndPath); code != exportIntact || !strings.Contains(out, "signature unchecked") {
		t.Fatalf("no key: %d\n%s", code, out)
	}
	code, out, _ := runVerifyExport(t, nil, "-json", "-key", pub, ndPath)
	var rep export.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != exportVerified || rep.Status != export.StatusVerified || rep.Manifest.HeadSeq != 3 {
		t.Fatalf("json: %d %v\n%s", code, err, out)
	}
	tampered := bytes.Replace(cs, []byte(",250,"), []byte(",205,"), 1)
	if code, out, _ := runVerifyExport(t, tampered, "-key", pub, "-"); code != exportDamaged || !strings.Contains(out, "problem: the file holds") ||
		!strings.Contains(out, "damaged") {
		t.Fatalf("tampered: %d\n%s", code, out)
	}
	if code, out, _ := runVerifyExport(t, nd[:len(nd)/2], "-key", pub, "-"); code != exportDamaged || !strings.Contains(out, "no manifest") {
		t.Fatalf("cut short: %d\n%s", code, out)
	}
	other, _ := export.NewSigner(bytes.Repeat([]byte{1}, export.SeedSize))
	if code, out, _ := runVerifyExport(t, nil, "-key", export.EncodeKey(other.PublicKey()), ndPath); code != exportDamaged ||
		!strings.Contains(out, "signature invalid") {
		t.Fatalf("other key: %d\n%s", code, out)
	}

	for _, args := range [][]string{{}, {ndPath, csPath}, {"-key", pub, "-key-file", keyPath, ndPath}, {"-key", "nope", ndPath},
		{"-key-file", filepath.Join(files, "missing"), ndPath}, {filepath.Join(files, "missing")}, {"-bogus"}} {
		if code, _, _ := runVerifyExport(t, nil, args...); code != exportError {
			t.Errorf("%q: exit %d, want %d", args, code, exportError)
		}
	}
	if code, _, errOut := runVerifyExport(t, nil, "-h"); code != exportVerified || !strings.Contains(errOut, "usage: ledgerd verify-export") {
		t.Fatalf("-h: %d %s", code, errOut)
	}
	// A directory reads as an error, not as a damaged export.
	if code, _, errOut := runVerifyExport(t, nil, files); code != exportError || errOut == "" {
		t.Fatalf("directory: %d %s", code, errOut)
	}
}

func TestExportKeyConfiguration(t *testing.T) {
	seed := bytes.Repeat([]byte{5}, export.SeedSize)
	want, _ := export.NewSigner(seed)
	dir := t.TempDir()
	addr, stop, _ := start(t, map[string]string{"LEDGERD_ADDR": "127.0.0.1:0", "LEDGERD_API_KEYS": key, "LEDGERD_DATA_DIR": dir,
		"LEDGERD_EXPORT_KEY": export.EncodeKey(seed)})
	if got := publicKey(t, "http://"+addr); got != export.EncodeKey(want.PublicKey()) {
		t.Fatalf("LEDGERD_EXPORT_KEY was not used: %s", got)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, export.KeyFile)); !os.IsNotExist(err) {
		t.Fatal("a key file was made although LEDGERD_EXPORT_KEY is set")
	}

	_, stop, logs := start(t, map[string]string{"LEDGERD_ADDR": "127.0.0.1:0", "LEDGERD_API_KEYS": key})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"source":"random until exit"`) {
		t.Fatalf("in memory:\n%s", logs)
	}

	if _, err := loadConfig(env(map[string]string{"LEDGERD_API_KEYS": "k", "LEDGERD_EXPORT_KEY": "short"})); err == nil ||
		!strings.Contains(err.Error(), "LEDGERD_EXPORT_KEY must be") {
		t.Fatalf("bad key: %v", err)
	}
	bad := t.TempDir()
	os.WriteFile(filepath.Join(bad, export.KeyFile), []byte("garbage"), 0o600)
	err := run(t.Context(), env(map[string]string{"LEDGERD_ADDR": "127.0.0.1:0", "LEDGERD_API_KEYS": key, "LEDGERD_DATA_DIR": bad}), io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "export signing key") {
		t.Fatalf("damaged key file: %v", err)
	}
}
