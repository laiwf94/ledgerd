package api

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

func TestListTransfersOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.must(201, "POST", "/v1/accounts", `{"id":"bob","currency":"USD","normal_balance":"credit"}`)
	f.must(201, "POST", "/v1/accounts", `{"id":"eur-bank","currency":"EUR","normal_balance":"debit"}`)
	f.must(201, "POST", "/v1/accounts", `{"id":"eur-wallet","currency":"EUR","normal_balance":"credit"}`)

	type xfer struct {
		id, state, cur, ref, payee, channel string
	}
	var all []xfer // in creation order
	create := func(i int, pending bool) xfer {
		x := xfer{state: "posted", cur: "USD", payee: []string{"alice", "bob", "merchant"}[i%3]}
		from := "bank"
		if i%5 == 0 {
			x.cur, from, x.payee = "EUR", "eur-bank", "eur-wallet"
		}
		body := fmt.Sprintf(`{"currency":%q,"postings":[{"account":%q,"direction":"debit","amount":%d},{"account":%q,"direction":"credit","amount":%d}]`, x.cur, from, 10+i, x.payee, 10+i)
		if i%4 == 0 {
			x.ref = fmt.Sprintf("payout %d/%d", i%3, 7) // a space and a slash survive the query string
			body += fmt.Sprintf(`,"reference":%q`, x.ref)
		}
		if i%3 == 0 {
			x.channel = []string{"web", "app&ios"}[i%2]
			body += fmt.Sprintf(`,"metadata":{"channel":%q}`, x.channel)
		}
		if pending {
			x.state = "pending"
			body += `,"pending":true,"timeout_seconds":3600`
		}
		x.id = f.must(201, "POST", "/v1/transfers", body+"}")["id"].(string)
		return x
	}
	for i := range 40 {
		all = append(all, create(i, i%4 == 1))
	}
	// Post some holds and void others.
	for i := range all {
		if all[i].state != "pending" {
			continue
		}
		switch i % 3 {
		case 0:
			f.must(200, "POST", "/v1/transfers/"+all[i].id+"/post", `{}`)
			all[i].state = "posted"
		case 1:
			f.must(200, "POST", "/v1/transfers/"+all[i].id+"/void", "")
			all[i].state = "voided"
		}
	}
	want := func(desc bool, keep func(xfer) bool) []string {
		var out []string
		for _, x := range all {
			if keep(x) {
				out = append(out, x.id)
			}
		}
		if desc {
			slices.Reverse(out)
		}
		return out
	}
	q := url.QueryEscape
	cases := map[string]func(xfer) bool{
		"":                              func(xfer) bool { return true },
		"?state=pending":                func(x xfer) bool { return x.state == "pending" },
		"?state=voided":                 func(x xfer) bool { return x.state == "voided" },
		"?state=expired":                func(xfer) bool { return false },
		"?account=bob":                  func(x xfer) bool { return x.payee == "bob" },
		"?account=bank&state=posted":    func(x xfer) bool { return x.cur == "USD" && x.state == "posted" },
		"?currency=EUR":                 func(x xfer) bool { return x.cur == "EUR" },
		"?reference=" + q("payout 2/7"): func(x xfer) bool { return x.ref == "payout 2/7" },
		"?reference=" + q("payout 0/7") + "&account=eur-wallet": func(x xfer) bool { return x.ref == "payout 0/7" && x.payee == "eur-wallet" },
		"?metadata[channel]=" + q("app&ios"):                    func(x xfer) bool { return x.channel == "app&ios" },
		"?metadata%5Bchannel%5D=web&currency=USD":               func(x xfer) bool { return x.channel == "web" && x.cur == "USD" },
		"?reference=unknown":                                    func(xfer) bool { return false },
		"?account=nobody":                                       func(xfer) bool { return false },
	}
	nonEmpty := 0
	for filter, keep := range cases {
		for _, order := range []string{"", "order=asc", "order=desc"} {
			path := "/v1/transfers" + filter
			if order != "" {
				if filter == "" {
					path += "?" + order
				} else {
					path += "&" + order
				}
			}
			w := want(order == "order=desc", keep)
			if len(w) > 0 {
				nonEmpty++
			}
			for _, limit := range []int{1, 3, 100} {
				got, pages := f.listItems(path, "transfers", limit)
				if !slices.Equal(got, w) {
					t.Fatalf("%s limit %d: got %v, want %v", path, limit, got, w)
				}
				if wantPages := max(1, (len(w)+limit-1)/limit); pages != wantPages {
					t.Fatalf("%s limit %d: %d pages, want %d", path, limit, pages, wantPages)
				}
			}
		}
	}
	if nonEmpty != 3*(len(cases)-3) {
		t.Fatalf("%d listings found something", nonEmpty)
	}

	// A page holds whole transfers as GET /v1/transfers/{id} returns them.
	m := f.must(200, "GET", "/v1/transfers?order=desc&limit=1&state=posted", "")
	tr := m["transfers"].([]any)[0].(map[string]any)
	one := f.must(200, "GET", "/v1/transfers/"+tr["id"].(string), "")
	if fmt.Sprint(tr) != fmt.Sprint(one) || num(m["as_of_seq"]) != int64(f.l.Head()) || m["has_more"] != true {
		t.Fatalf("page %v, transfer %v", m, one)
	}
	if m := f.must(200, "GET", "/v1/transfers?state=expired", ""); fmt.Sprint(m["transfers"]) != "[]" || m["has_more"] != false {
		t.Fatalf("empty listing: %v", m)
	}

	// Cursors are bound to the filters and the order; order=asc and no
	// order are the same listing.
	cur := f.must(200, "GET", "/v1/transfers?account=bob&limit=1", "")["next_cursor"].(string)
	f.must(200, "GET", "/v1/transfers?order=asc&account=bob&limit=5&cursor="+cur, "")
	desc := f.must(200, "GET", "/v1/transfers?account=bob&order=desc&limit=1", "")["next_cursor"].(string)
	acct := f.must(200, "GET", "/v1/accounts?limit=1", "")["next_cursor"].(string)
	bad := map[string]string{
		"/v1/transfers?account=bob&order=desc&cursor=" + cur:                    "cursor is invalid",
		"/v1/transfers?account=bob&cursor=" + desc:                              "cursor is invalid",
		"/v1/transfers?account=alice&cursor=" + cur:                             "cursor is invalid",
		"/v1/transfers?account=bob&state=posted&cursor=" + cur:                  "cursor is invalid",
		"/v1/transfers?cursor=" + acct:                                          "cursor is invalid",
		"/v1/accounts?cursor=" + cur:                                            "cursor is invalid",
		"/v1/transfers?account=bob&cursor=" + cur[:len(cur)-2]:                  "cursor is invalid",
		"/v1/transfers?order=up":                                                "order must be asc or desc",
		"/v1/transfers?order=":                                                  "order must be asc or desc",
		"/v1/transfers?state=open":                                              "state must be one of",
		"/v1/transfers?state=":                                                  "state must not be empty",
		"/v1/transfers?account=":                                                "account must not be empty",
		"/v1/transfers?reference=":                                              "reference must not be empty",
		"/v1/transfers?account=-x":                                              "not a valid account id",
		"/v1/transfers?currency=eur":                                            "ISO 4217",
		"/v1/transfers?reference=" + strings.Repeat("r", ledger.MaxReference+1): "reference must be at most",
		"/v1/transfers?reference=a%0Ab":                                         "reference must be at most",
		"/v1/transfers?account=bob&account=alice":                               "only once",
		"/v1/transfers?order=asc&order=desc":                                    "only once",
		"/v1/transfers?direction=debit":                                         "unknown query parameter",
		"/v1/transfers?limit=0":                                                 "limit must be",
		"/v1/transfers?metadata[]=x":                                            "metadata filter keys",
	}
	for path, msg := range bad {
		r := f.call("GET", path, "")
		if r.status != 400 || r.code() != "invalid_argument" || !strings.Contains(string(r.body), msg) {
			t.Errorf("%s: %d %s, want 400 with %q", path, r.status, r.body, msg)
		}
	}
	if r := f.call("GET", "/v1/transfers", "", "Authorization", ""); r.status != 401 {
		t.Errorf("no key: %d", r.status)
	}
}

// TestListTransfersPagingDuringWritesOverHTTP walks transfer listings over
// HTTP in both orders while other clients create transfers, and checks each
// walk is in creation order with no repeats and includes every transfer
// acknowledged before it began.
func TestListTransfersPagingDuringWritesOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	var mu sync.Mutex
	var known []string // in creation order
	pay := func(ref string) string {
		r := f.call("POST", "/v1/transfers", fmt.Sprintf(`{"currency":"USD","reference":%q,"postings":[{"account":"bank","direction":"debit","amount":3},{"account":"merchant","direction":"credit","amount":3}]}`, ref))
		if r.status != 201 {
			t.Errorf("transfer: %d %s", r.status, r.body)
			return ""
		}
		return r.json(t)["id"].(string)
	}
	for range 50 {
		known = append(known, pay("seed"))
	}
	stop := make(chan struct{})
	var created atomic.Int64
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Hold the lock across the call, so known stays in
				// creation order.
				mu.Lock()
				id := pay("seed")
				if id != "" {
					known = append(known, id)
				}
				mu.Unlock()
				if id == "" {
					return
				}
				created.Add(1)
			}
		})
	}
	walks := 0
	for ; walks < 200 && (walks < 6 || created.Load() < 100); walks++ {
		mu.Lock()
		must := slices.Clone(known)
		mu.Unlock()
		desc := walks%2 == 1
		path := "/v1/transfers?reference=seed"
		if desc {
			path += "&order=desc"
		}
		got, _ := f.listItems(path, "transfers", 4+walks%9)
		if desc {
			slices.Reverse(got)
		}
		// In creation order with no repeats: a subsequence of the final
		// order, so check positions grow.
		pos := map[string]int{}
		mu.Lock()
		for i, id := range known {
			pos[id] = i
		}
		mu.Unlock()
		last := -1
		for _, id := range got {
			p, ok := pos[id]
			if !ok || p <= last {
				t.Fatalf("walk %d (desc %v): %s is out of order or unknown", walks, desc, id)
			}
			last = p
		}
		// Either order lists everything acknowledged before the walk began,
		// then possibly some of what came after: an oldest-first walk reaches
		// new transfers as it goes, a newest-first walk only those created
		// before its first page.
		if len(got) < len(must) || !slices.Equal(got[:len(must)], must) {
			t.Fatalf("walk %d (desc %v) does not start with the transfers acknowledged before it began", walks, desc)
		}
	}
	close(stop)
	wg.Wait()
	if created.Load() < 100 {
		t.Fatalf("only %d transfers created during %d walks", created.Load(), walks)
	}
	t.Logf("%d walks during %d new transfers", walks, created.Load())
	if got, _ := f.listItems("/v1/transfers", "transfers", 37); !slices.Equal(got, known) {
		t.Fatalf("final walk: %d transfers, want %d", len(got), len(known))
	}
}
