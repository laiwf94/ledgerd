package api

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

func TestCursorCodec(t *testing.T) {
	c := newCursorCodec("", []string{apiKey, otherKey})
	filter := canonicalFilter(map[string]string{"currency": "USD"})
	cur := c.encode(cursorAccounts, filter, []byte("acct-42"))
	pos, err := c.decode(cursorAccounts, filter, cur)
	if err != nil || string(pos) != "acct-42" {
		t.Fatalf("round trip: %q %v", pos, err)
	}
	if len(cur) > maxCursorLength {
		t.Fatalf("cursor is %d characters, more than %d", len(cur), maxCursorLength)
	}
	long := c.encode(cursorAccounts, filter, []byte(strings.Repeat("x", maxCursorPosition)))
	if len(long) > maxCursorLength {
		t.Fatalf("longest cursor is %d characters, more than %d", len(long), maxCursorLength)
	}
	if _, err := c.decode(cursorAccounts, filter, long); err != nil {
		t.Fatal(err)
	}

	// The same key set in another order signs the same way; another key set
	// or an explicit secret does not.
	if _, err := newCursorCodec("", []string{otherKey, apiKey}).decode(cursorAccounts, filter, cur); err != nil {
		t.Fatalf("reordered keys: %v", err)
	}
	for name, other := range map[string]cursorCodec{
		"other keys": newCursorCodec("", []string{apiKey}),
		"secret":     newCursorCodec("cursor-secret-0123", []string{apiKey, otherKey}),
	} {
		if _, err := other.decode(cursorAccounts, filter, cur); err == nil {
			t.Errorf("%s accepted the cursor", name)
		}
	}

	// Any change to any byte is refused.
	raw, _ := base64.RawURLEncoding.DecodeString(cur)
	for i := range raw {
		for _, bit := range []byte{1, 0x80} {
			b := slices.Clone(raw)
			b[i] ^= bit
			if _, err := c.decode(cursorAccounts, filter, base64.RawURLEncoding.EncodeToString(b)); err == nil {
				t.Fatalf("byte %d with bit %#x flipped was accepted", i, bit)
			}
		}
	}
	for i := range len(raw) {
		if _, err := c.decode(cursorAccounts, filter, base64.RawURLEncoding.EncodeToString(raw[:i])); err == nil {
			t.Fatalf("cursor cut to %d bytes was accepted", i)
		}
	}

	// A correctly signed cursor for another listing, filter or version is
	// refused too.
	if _, err := c.decode(cursorAccounts+1, filter, cur); err == nil {
		t.Error("another kind accepted")
	}
	if _, err := c.decode(cursorAccounts, canonicalFilter(map[string]string{"currency": "EUR"}), cur); err == nil {
		t.Error("another filter accepted")
	}
	resign := func(b []byte) string {
		b = slices.Clone(b[:len(b)-cursorMACBytes])
		return base64.RawURLEncoding.EncodeToString(append(b, c.mac(b)...))
	}
	v2 := slices.Clone(raw)
	v2[0] = cursorVersion + 1
	if _, err := c.decode(cursorAccounts, filter, resign(v2)); err == nil {
		t.Error("another version accepted")
	}
	tooLong := append(slices.Clone(raw[:len(raw)-cursorMACBytes]), make([]byte, maxCursorPosition)...)
	if _, err := c.decode(cursorAccounts, filter, resign(append(tooLong, make([]byte, cursorMACBytes)...))); err == nil {
		t.Error("oversized position accepted")
	}
	for _, bad := range []string{"", "!!!", cur + "=", strings.Repeat("A", maxCursorLength+1)} {
		if _, err := c.decode(cursorAccounts, filter, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("encoding an oversized position did not panic")
			}
		}()
		c.encode(cursorAccounts, filter, make([]byte, maxCursorPosition+1))
	}()
}

func TestCanonicalFilterIsUnambiguous(t *testing.T) {
	seen := map[string]string{}
	for _, m := range []map[string]string{
		{}, {"a": ""}, {"a": "b"}, {"ab": ""}, {"a": "", "b": ""}, {"a": "b", "c": "d"}, {"a": "bc", "": "d"},
		{"currency": "USD"}, {"metadata[currency]": "USD"}, {"a": strings.Repeat("x", 300)},
	} {
		k := canonicalFilter(m)
		if prev, ok := seen[k]; ok {
			t.Fatalf("%v and %s share %q", m, prev, k)
		}
		seen[k] = fmt.Sprint(m)
	}
	if canonicalFilter(map[string]string{"b": "1", "a": "2"}) != canonicalFilter(map[string]string{"a": "2", "b": "1"}) {
		t.Fatal("order matters")
	}
}

// listAll pages through path (a listing with its filters) and returns every
// account ID with the number of pages.
func (f *fixture) listAll(path string, limit int) ([]string, int) {
	f.t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var out []string
	cursor := ""
	for pages := 1; ; pages++ {
		if pages > 10_000 {
			f.t.Fatalf("%s never ended", path)
		}
		u := fmt.Sprintf("%s%slimit=%d", path, sep, limit)
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		m := f.must(200, "GET", u, "")
		for _, a := range m["accounts"].([]any) {
			out = append(out, a.(map[string]any)["id"].(string))
		}
		next, _ := m["next_cursor"].(string)
		if m["has_more"].(bool) != (next != "") {
			f.t.Fatalf("%s: has_more %v with next_cursor %q", u, m["has_more"], next)
		}
		if next == "" {
			return out, pages
		}
		cursor = next
	}
}

func TestListAccountsOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	type acct struct{ id, cur, tier string }
	var all []acct
	for i := range 45 {
		a := acct{fmt.Sprintf("acct-%02d", (i*17)%45), []string{"USD", "EUR", "JPY"}[i%3], []string{"gold", "silver", ""}[i%2*(1+i%3%2)]}
		meta := ""
		if a.tier != "" {
			meta = fmt.Sprintf(`,"metadata":{"tier":%q,"note":"a b&c"}`, a.tier)
		}
		f.must(201, "POST", "/v1/accounts", fmt.Sprintf(`{"id":%q,"currency":%q,"normal_balance":"credit"%s}`, a.id, a.cur, meta))
		all = append(all, a)
	}
	want := func(keep func(acct) bool) []string {
		var out []string
		for _, a := range all {
			if keep(a) {
				out = append(out, a.id)
			}
		}
		sort.Strings(out)
		return out
	}
	cases := map[string][]string{
		"/v1/accounts":                                  want(func(acct) bool { return true }),
		"/v1/accounts?currency=EUR":                     want(func(a acct) bool { return a.cur == "EUR" }),
		"/v1/accounts?metadata[tier]=gold":              want(func(a acct) bool { return a.tier == "gold" }),
		"/v1/accounts?metadata%5Btier%5D=silver":        want(func(a acct) bool { return a.tier == "silver" }),
		"/v1/accounts?currency=USD&metadata[tier]=gold": want(func(a acct) bool { return a.cur == "USD" && a.tier == "gold" }),
		"/v1/accounts?metadata[note]=a+b%26c":           want(func(a acct) bool { return a.tier != "" }),
		"/v1/accounts?currency=GBP":                     nil,
		"/v1/accounts?metadata[tier]=gold&currency=JPY": want(func(a acct) bool { return a.cur == "JPY" && a.tier == "gold" }),
	}
	for path, w := range cases {
		for _, limit := range []int{1, 4, 100} {
			got, pages := f.listAll(path, limit)
			if !slices.Equal(got, w) {
				t.Fatalf("%s limit %d: got %v, want %v", path, limit, got, w)
			}
			if wantPages := max(1, (len(w)+limit-1)/limit); pages != wantPages {
				t.Fatalf("%s limit %d: %d pages, want %d", path, limit, pages, wantPages)
			}
		}
	}

	// A page carries balances and the sequence they are as of.
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"acct-00","direction":"debit","amount":5},{"account":"acct-03","direction":"credit","amount":5}]}`)
	m := f.must(200, "GET", "/v1/accounts?limit=2", "")
	first := m["accounts"].([]any)[0].(map[string]any)
	if first["id"] != "acct-00" || num(first["balance"].(map[string]any)["posted"]) != -5 || num(m["as_of_seq"]) != 1 || num(first["as_of_seq"]) != 1 {
		t.Fatalf("first page: %v", m)
	}
	if m := f.must(200, "GET", "/v1/accounts", ""); len(m["accounts"].([]any)) != 45 || m["has_more"] != false || m["next_cursor"] != nil {
		t.Fatalf("default limit: %d accounts, has_more %v", len(m["accounts"].([]any)), m["has_more"])
	}
	if m := f.must(200, "GET", "/v1/accounts?currency=GBP", ""); fmt.Sprint(m["accounts"]) != "[]" {
		t.Fatalf("empty listing: %v", m)
	}

	// Cursors are bound to their filters and cannot be edited.
	cur := f.must(200, "GET", "/v1/accounts?currency=USD&limit=1", "")["next_cursor"].(string)
	f.must(200, "GET", "/v1/accounts?limit=3&currency=USD&cursor="+cur, "") // the limit may change
	bad := map[string]string{
		"/v1/accounts?cursor=" + cur:                                   "cursor is invalid",
		"/v1/accounts?currency=EUR&cursor=" + cur:                      "cursor is invalid",
		"/v1/accounts?currency=USD&metadata[tier]=gold&cursor=" + cur:  "cursor is invalid",
		"/v1/accounts?currency=USD&cursor=" + cur[:len(cur)-1] + "A":   "cursor is invalid",
		"/v1/accounts?currency=USD&cursor=" + strings.Repeat("A", 200): "cursor is invalid",
		"/v1/accounts?cursor=":                                         "cursor must not be empty",
		"/v1/accounts?limit=0":                                         "limit must be",
		"/v1/accounts?limit=1001":                                      "limit must be",
		"/v1/accounts?limit=x":                                         "limit must be",
		"/v1/accounts?currency=usd":                                    "ISO 4217",
		"/v1/accounts?currency=":                                       "currency must not be empty",
		"/v1/accounts?currency=USD&currency=EUR":                       "only once",
		"/v1/accounts?state=open":                                      "unknown query parameter",
		"/v1/accounts?metadata[tier=gold":                              "unknown query parameter",
		"/v1/accounts?metadata[]=gold":                                 "metadata filter keys",
		"/v1/accounts?%zz":                                             "invalid query string",
	}
	for path, msg := range bad {
		r := f.call("GET", path, "")
		if r.status != 400 || r.code() != "invalid_argument" || !strings.Contains(string(r.body), msg) {
			t.Errorf("%s: %d %s, want 400 with %q", path, r.status, r.body, msg)
		}
	}
	var many strings.Builder
	for i := range 9 {
		fmt.Fprintf(&many, "&metadata[k%d]=v", i)
	}
	if r := f.call("GET", "/v1/accounts?limit=1"+many.String(), ""); r.status != 400 || !strings.Contains(string(r.body), "at most 8 metadata") {
		t.Errorf("9 metadata filters: %d %s", r.status, r.body)
	}

	// A cursor from a server with other keys is refused.
	g := newFixture(t, func(_ *ledger.Options, c *Config) { c.CursorSecret = "another-cursor-secret" })
	if r := g.call("GET", "/v1/accounts?currency=USD&cursor="+cur, ""); r.status != 400 {
		t.Errorf("foreign cursor: %d %s", r.status, r.body)
	}
	if r := f.call("GET", "/v1/accounts", "", "Authorization", ""); r.status != 401 {
		t.Errorf("no key: %d", r.status)
	}
}

// TestListAccountsPagingDuringWritesOverHTTP walks the listing over HTTP
// while other clients open accounts, and checks each walk is in order with
// no repeats and includes every account acknowledged before it began.
func TestListAccountsPagingDuringWritesOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	var mu sync.Mutex
	var known []string
	for i := range 60 {
		id := fmt.Sprintf("seed-%03d", i*7)
		f.must(201, "POST", "/v1/accounts", fmt.Sprintf(`{"id":%q,"currency":"USD","normal_balance":"credit"}`, id))
		known = append(known, id)
	}
	stop := make(chan struct{})
	var created atomic.Int64
	var wg sync.WaitGroup
	for w := range 3 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				// IDs interleave with the seeds, before and after any position.
				id := fmt.Sprintf("seed-%03d-%d-%d", (i*13)%420, w, i)
				r := f.call("POST", "/v1/accounts", fmt.Sprintf(`{"id":%q,"currency":"USD","normal_balance":"credit"}`, id))
				if r.status != 201 {
					t.Errorf("create %s: %d %s", id, r.status, r.body)
					return
				}
				mu.Lock()
				known = append(known, id)
				mu.Unlock()
				created.Add(1)
			}
		})
	}
	walks := 0
	for ; walks < 200 && (walks < 5 || created.Load() < 100); walks++ {
		mu.Lock()
		must := slices.Clone(known)
		mu.Unlock()
		got, _ := f.listAll("/v1/accounts?currency=USD", 5+walks%11)
		if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
			t.Fatalf("walk %d is out of order or repeats", walks)
		}
		for _, id := range must {
			if _, ok := slices.BinarySearch(got, id); !ok {
				t.Fatalf("walk %d is missing %s, created before it began", walks, id)
			}
		}
	}
	close(stop)
	wg.Wait()
	if created.Load() < 100 {
		t.Fatalf("only %d accounts created during %d walks", created.Load(), walks)
	}
	t.Logf("%d walks during %d new accounts", walks, created.Load())
	got, _ := f.listAll("/v1/accounts", 50)
	sort.Strings(known)
	if !slices.Equal(got, known) {
		t.Fatalf("final walk: %d accounts, want %d", len(got), len(known))
	}
}
