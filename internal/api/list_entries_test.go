package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type entryPage struct {
	Entries []struct {
		Seq        uint64 `json:"seq"`
		Kind       string `json:"kind"`
		TransferID string `json:"transfer_id"`
		Currency   string `json:"currency"`
		Lines      []struct {
			Account   string `json:"account"`
			Direction string `json:"direction"`
			Currency  string `json:"currency"`
			Posted    int64  `json:"posted"`
			Pending   int64  `json:"pending"`
		} `json:"lines"`
	} `json:"entries"`
	HasMore      bool   `json:"has_more"`
	NextAfterSeq uint64 `json:"next_after_seq"`
	HeadSeq      uint64 `json:"head_seq"`
}

func (f *fixture) entries(query string) entryPage {
	f.t.Helper()
	r := f.call("GET", "/v1/entries?"+query, "")
	if r.status != 200 {
		f.t.Fatalf("GET /v1/entries?%s: %d %s", query, r.status, r.body)
	}
	var p entryPage
	if err := json.Unmarshal(r.body, &p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// follow walks the feed from after_seq to the head and returns the
// sequences it saw.
func (f *fixture) follow(filters string, after uint64, limit int) []uint64 {
	f.t.Helper()
	var out []uint64
	for pages := 0; pages < 10_000; pages++ {
		q := fmt.Sprintf("after_seq=%d&limit=%d", after, limit)
		if filters != "" {
			q += "&" + filters
		}
		p := f.entries(q)
		if p.NextAfterSeq < after || p.NextAfterSeq > p.HeadSeq || p.HasMore != (p.NextAfterSeq < p.HeadSeq) {
			f.t.Fatalf("%s: next %d, head %d, has_more %v", q, p.NextAfterSeq, p.HeadSeq, p.HasMore)
		}
		for _, e := range p.Entries {
			out = append(out, e.Seq)
		}
		if !p.HasMore {
			return out
		}
		after = p.NextAfterSeq
	}
	f.t.Fatalf("the feed %s never ended", filters)
	return nil
}

func TestEntriesOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.must(201, "POST", "/v1/accounts", `{"id":"bob","currency":"USD","normal_balance":"credit"}`)
	f.must(201, "POST", "/v1/accounts", `{"id":"eur-bank","currency":"EUR","normal_balance":"debit"}`)
	f.must(201, "POST", "/v1/accounts", `{"id":"eur-wallet","currency":"EUR","normal_balance":"credit"}`)
	pay := func(cur, from, to string, amount int, extra string) string {
		return f.must(201, "POST", "/v1/transfers", fmt.Sprintf(`{"currency":%q,"postings":[{"account":%q,"direction":"debit","amount":%d},`+
			`{"account":%q,"direction":"credit","amount":%d}]%s}`, cur, from, amount, to, amount, extra))["id"].(string)
	}
	pay("USD", "bank", "alice", 1000, "")                                              // 1
	h := pay("USD", "alice", "merchant", 300, `,"pending":true,"timeout_seconds":600`) // 2
	pay("EUR", "eur-bank", "eur-wallet", 500, "")                                      // 3
	f.must(200, "POST", "/v1/transfers/"+h+"/extend", `{"timeout_seconds":1200}`)      // 4
	f.must(200, "POST", "/v1/transfers/"+h+"/post", `{"amount":200}`)                  // 5
	p := pay("USD", "alice", "bob", 100, "")                                           // 6
	f.must(201, "POST", "/v1/transfers/"+p+"/reverse", `{"amount":40}`)                // 7

	page := f.entries("")
	if len(page.Entries) != 7 || page.HasMore || page.NextAfterSeq != 7 || page.HeadSeq != 7 {
		t.Fatalf("whole feed: %+v", page)
	}
	wantKinds := []string{"posted", "pending", "posted", "extend_pending", "post_pending", "posted", "reversal"}
	for i, e := range page.Entries {
		if e.Seq != uint64(i+1) || e.Kind != wantKinds[i] || e.TransferID == "" {
			t.Fatalf("entry %d: %+v", i+1, e)
		}
	}
	if e := page.Entries[4]; e.TransferID != h || len(e.Lines) != 2 || e.Lines[0].Account != "alice" ||
		e.Lines[0].Posted != 200 || e.Lines[0].Pending != -300 {
		t.Fatalf("the capture: %+v", e)
	}
	if e := page.Entries[3]; len(e.Lines) != 0 {
		t.Fatalf("the extension has lines: %+v", e)
	}

	for filters, want := range map[string][]uint64{
		"":                                 {1, 2, 3, 4, 5, 6, 7},
		"account=alice":                    {1, 2, 5, 6, 7},
		"account=bob":                      {6, 7},
		"account=nobody":                   nil,
		"currency=EUR":                     {3},
		"currency=USD":                     {1, 2, 4, 5, 6, 7},
		"kind=posted":                      {1, 3, 6},
		"kind=pending,post_pending":        {2, 5},
		"kind=extend_pending&currency=USD": {4},
		"account=alice&kind=reversal":      {7},
		"account=eur-wallet&currency=USD":  nil,
	} {
		for _, limit := range []int{1, 2, 100} {
			if got := f.follow(filters, 0, limit); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%q limit %d: %v, want %v", filters, limit, got, want)
			}
		}
	}
	// Resuming from a position returns what follows it, and a filtered
	// page that runs out of matches reports the head as its position.
	if got := f.follow("", 4, 2); fmt.Sprint(got) != "[5 6 7]" {
		t.Fatalf("after 4: %v", got)
	}
	if p := f.entries("account=bob&after_seq=6"); len(p.Entries) != 1 || p.NextAfterSeq != 7 || p.HasMore {
		t.Fatalf("bob after 6: %+v", p)
	}
	// A full page stops at its last entry when another match follows it,
	// and otherwise scans on to the head.
	if p := f.entries("currency=USD&limit=2"); len(p.Entries) != 2 || p.NextAfterSeq != 2 || !p.HasMore {
		t.Fatalf("first USD page: %+v", p)
	}
	if p := f.entries("currency=EUR&limit=1"); len(p.Entries) != 1 || p.NextAfterSeq != 7 || p.HasMore {
		t.Fatalf("the only EUR page: %+v", p)
	}
	if p := f.entries("after_seq=7"); len(p.Entries) != 0 || p.NextAfterSeq != 7 || p.HasMore {
		t.Fatalf("at the head: %+v", p)
	}
	// The next entry written is picked up from the stored position.
	pay("USD", "bank", "bob", 5, "")
	if got := f.follow("account=bob", 7, 10); fmt.Sprint(got) != "[8]" {
		t.Fatalf("after a new entry: %v", got)
	}

	for _, q := range []string{"after_seq=9", "after_seq=-1", "after_seq=x", "after_seq=", "limit=0", "limit=1001",
		"kind=refund", "kind=posted,posted", "kind=", "kind=posted&kind=pending", "currency=usd", "currency=XAU",
		"account=-x", "account=", "cursor=AAAA", "state=posted", "order=desc", "metadata%5Bk%5D=v", "%zz"} {
		r := f.call("GET", "/v1/entries?"+q, "")
		if r.status != 400 || r.code() != "invalid_argument" {
			t.Errorf("GET /v1/entries?%s: %d %s", q, r.status, r.body)
		}
	}
	if r := f.call("GET", "/v1/entries", "", "Authorization", ""); r.status != 401 {
		t.Fatalf("without a key: %d", r.status)
	}
}

// A consumer that follows the feed over HTTP while clients write sees
// every entry exactly once, in order.
func TestEntriesFollowOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":100000},`+
		`{"account":"alice","direction":"credit","amount":100000}]}`)
	const writers, each = 4, 40
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"alice","direction":"debit","amount":1},`+
					`{"account":"merchant","direction":"credit","amount":1}]}`)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	var seen []uint64
	var after uint64
	for finished := false; ; {
		select {
		case <-done:
			finished = true
		default:
		}
		p := f.entries(fmt.Sprintf("after_seq=%d&limit=7", after))
		for _, e := range p.Entries {
			seen = append(seen, e.Seq)
		}
		after = p.NextAfterSeq
		if finished && !p.HasMore {
			break
		}
	}
	if len(seen) != 1+writers*each {
		t.Fatalf("followed %d entries, want %d", len(seen), 1+writers*each)
	}
	for i, s := range seen {
		if s != uint64(i+1) {
			t.Fatalf("entry %d has sequence %d", i+1, s)
		}
	}
}

func fuzzEntrySeeds() []string {
	return []string{"after_seq=3", "account=a&kind=posted,pending", "kind=extend_pending", "currency=EUR&after_seq=" + strings.Repeat("9", 25),
		"kind=" + url.QueryEscape("posted,void_pending")}
}
