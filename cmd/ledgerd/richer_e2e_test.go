package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

// This file is the end-to-end test of week 3's richer transfers against the
// real binary: a marketplace payout split in one atomic batch, a hotel hold
// that grows, is extended and is posted at checkout, and a USD to EUR
// conversion through exchange accounts. Each is first walked through step by
// step, then run under load from concurrent clients while the server is
// killed with SIGKILL, so that batches and conversions are in flight when it
// dies. The restored copy must hold every acknowledged request whole and no
// unacknowledged one in part.

const (
	fxRate     = "0.9234"
	fxQuotedAt = "2026-11-12T09:00:00+01:00"
	bigFunding = int64(1) << 40
)

// reply is one HTTP answer.
type reply struct {
	status   int
	body     []byte
	replayed bool
}

// httpCall sends a request and returns whatever the server answered. Only a
// request that got no answer is an error.
func httpCall(base, method, path, body, idemKey string) (reply, error) {
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		return reply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := e2eHTTP.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return reply{}, err
	}
	return reply{status: resp.StatusCode, body: data, replayed: resp.Header.Get("Idempotent-Replayed") == "true"}, nil
}

// must sends a request that has to get the given status and decodes the
// answer into out (when not nil).
func must(t *testing.T, base, method, path, body, idemKey string, status int, out any) reply {
	t.Helper()
	r, err := httpCall(base, method, path, body, idemKey)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if r.status != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, r.status, status, r.body)
	}
	if out != nil {
		if err := json.Unmarshal(r.body, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, r.body)
		}
	}
	return r
}

type batchReply struct {
	Error *struct {
		Code string `json:"code"`
		Item *int   `json:"item"`
	} `json:"error"`
	Results []struct {
		Status   string           `json:"status"`
		Transfer *ledger.Transfer `json:"transfer"`
	} `json:"results"`
}

type errReply struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// effect is what a request does to one account's four totals.
type effect struct {
	account string
	d       totals
}

// tp is a two-posting body fragment.
func tp(from, to string, amount int64) string {
	return fmt.Sprintf(`"postings":[{"account":%q,"direction":"debit","amount":%d},{"account":%q,"direction":"credit","amount":%d}]`,
		from, amount, to, amount)
}

// conversion is the body of a USD to EUR conversion of usd cents with a fee
// in USD, and its effects. The EUR amount is the USD amount at fxRate,
// rounded down to the cent.
func conversion(from, to string, usd, fee int64) (string, []effect) {
	eur := usd * 9234 / 10000
	body := fmt.Sprintf(`{"currency":"USD","postings":[`+
		`{"account":%q,"direction":"debit","amount":%d},`+
		`{"account":"fx_usd","direction":"credit","amount":%d},`+
		`{"account":"fees_usd","direction":"credit","amount":%d},`+
		`{"account":"fx_eur","direction":"debit","amount":%d,"currency":"EUR"},`+
		`{"account":%q,"direction":"credit","amount":%d,"currency":"EUR"}],`+
		`"fx":{"rate":%q,"source":"ecb","quoted_at":%q},"reference":"fx-%s-%d"}`,
		from, usd+fee, usd, fee, eur, to, eur, fxRate, fxQuotedAt, from, usd)
	return body, []effect{
		{from, totals{DebitsPosted: usd + fee}},
		{"fx_usd", totals{CreditsPosted: usd}},
		{"fees_usd", totals{CreditsPosted: fee}},
		{"fx_eur", totals{DebitsPosted: eur}},
		{to, totals{CreditsPosted: eur}},
	}
}

// rOp is one keyed request of the load phase. increase, extend and post act
// on the hold an earlier op of the same client created (ref).
type rOp struct {
	kind    string // fund, batch, hold, increase, extend, post, convert
	body    string
	ref     int
	effects []effect
	n       int // transfers the request creates
	key     string
}

func (o rOp) path(ids [][]string) string {
	switch o.kind {
	case "batch":
		return "/v1/transfers/batch"
	case "increase", "extend", "post":
		return "/v1/transfers/" + ids[o.ref][0] + "/" + o.kind
	}
	return "/v1/transfers"
}

// rClient runs one stream of keyed requests on accounts of its own.
type rClient struct {
	n        int
	rng      *rand.Rand
	only     string // when set, the client only makes this kind of request
	ops      []rOp
	ids      [][]string // transfer IDs created by ops[i]
	replies  [][]byte   // the answer to ops[i]
	acked    int
	inflight bool
}

func (c *rClient) esc() string   { return fmt.Sprintf("esc_%d", c.n) }
func (c *rClient) guest() string { return fmt.Sprintf("guest_%d", c.n) }
func (c *rClient) usd() string   { return fmt.Sprintf("usd_%d", c.n) }
func (c *rClient) eur() string   { return fmt.Sprintf("eur_%d", c.n) }

func (c *rClient) add(o rOp) int {
	o.key = fmt.Sprintf("rich-c%d-op%d", c.n, len(c.ops))
	if o.n == 0 && (o.kind == "fund" || o.kind == "hold" || o.kind == "convert") {
		o.n = 1
	}
	c.ops = append(c.ops, o)
	c.ids = append(c.ids, nil)
	c.replies = append(c.replies, nil)
	return len(c.ops) - 1
}

// next appends the requests of one more payout, stay or conversion.
func (c *rClient) next() {
	if len(c.ops) == 0 {
		// Fund the client's guest card and USD wallet first.
		c.add(rOp{kind: "fund", body: `{"currency":"EUR",` + tp("card_in", c.guest(), bigFunding) + `}`,
			effects: []effect{{"card_in", totals{DebitsPosted: bigFunding}}, {c.guest(), totals{CreditsPosted: bigFunding}}}})
		c.add(rOp{kind: "fund", body: `{"currency":"USD",` + tp("usd_in", c.usd(), bigFunding) + `}`,
			effects: []effect{{"usd_in", totals{DebitsPosted: bigFunding}}, {c.usd(), totals{CreditsPosted: bigFunding}}}})
		return
	}
	kind := c.only
	if kind == "" {
		switch r := c.rng.IntN(5); {
		case r < 2:
			kind = "batch"
		case r < 4:
			kind = "stay"
		default:
			kind = "convert"
		}
	}
	switch kind {
	case "batch":
		// A payout: the order's money arrives in escrow and is split
		// between two sellers, VAT and the platform, which sweeps whatever
		// is left with a balancing transfer.
		a1, a2, rest := 1+c.rng.Int64N(50_000), 1+c.rng.Int64N(50_000), 1+c.rng.Int64N(5_000)
		vat := (a1 + a2) / 10
		total := a1 + a2 + vat + rest
		s1 := fmt.Sprintf("seller_%d", 1+c.rng.IntN(3))
		s2 := fmt.Sprintf("seller_%d", 1+c.rng.IntN(3))
		body := `{"transfers":[` +
			`{"currency":"EUR",` + tp("card_in", c.esc(), total) + `},` +
			`{"currency":"EUR",` + tp(c.esc(), s1, a1) + `},` +
			`{"currency":"EUR",` + tp(c.esc(), s2, a2) + `},` +
			`{"currency":"EUR",` + tp(c.esc(), "vat", vat) + `},` +
			`{"currency":"EUR","balance_debit":true,` + tp(c.esc(), "platform", bigFunding) + `}]}`
		c.add(rOp{kind: "batch", body: body, n: 5, effects: []effect{
			{"card_in", totals{DebitsPosted: total}},
			{c.esc(), totals{CreditsPosted: total, DebitsPosted: total}},
			{s1, totals{CreditsPosted: a1}},
			{s2, totals{CreditsPosted: a2}},
			{"vat", totals{CreditsPosted: vat}},
			{"platform", totals{CreditsPosted: rest}},
		}})
	case "stay":
		// A hotel stay: a hold at check-in, grown for extras, extended for
		// a late checkout and posted for less than it holds.
		h, b := 1+c.rng.Int64N(40_000), 1+c.rng.Int64N(10_000)
		g := c.guest()
		hold := c.add(rOp{kind: "hold", body: `{"currency":"EUR","pending":true,"timeout_seconds":86400,` + tp(g, "hotel", h) + `}`,
			effects: []effect{{g, totals{DebitsPending: h}}, {"hotel", totals{CreditsPending: h}}}})
		c.add(rOp{kind: "increase", ref: hold, body: fmt.Sprintf(`{"amount":%d}`, b),
			effects: []effect{{g, totals{DebitsPending: b}}, {"hotel", totals{CreditsPending: b}}}})
		c.add(rOp{kind: "extend", ref: hold, body: fmt.Sprintf(`{"timeout_seconds":%d}`, 2*86400+c.rng.IntN(86400))})
		held := h + b
		paid := held - c.rng.Int64N(held)
		c.add(rOp{kind: "post", ref: hold, body: fmt.Sprintf(`{"amount":%d}`, paid),
			effects: []effect{{g, totals{DebitsPending: -held, DebitsPosted: paid}}, {"hotel", totals{CreditsPending: -held, CreditsPosted: paid}}}})
	case "convert":
		usd := 100 + c.rng.Int64N(100_000)
		body, eff := conversion(c.usd(), c.eur(), usd, usd/100)
		c.add(rOp{kind: "convert", body: body, effects: eff})
	}
}

func (c *rClient) do(base string, i int) (reply, error) {
	o := c.ops[i]
	return httpCall(base, "POST", o.path(c.ids), o.body, o.key)
}

// record keeps the transfer IDs an answer created.
func (c *rClient) record(i int, r reply) error {
	o := c.ops[i]
	var ids []string
	switch o.kind {
	case "batch":
		var v batchReply
		if err := json.Unmarshal(r.body, &v); err != nil {
			return err
		}
		for _, it := range v.Results {
			if it.Status != "created" || it.Transfer == nil {
				return fmt.Errorf("batch result %+v", it)
			}
			ids = append(ids, it.Transfer.ID)
		}
	default:
		var v ledger.Transfer
		if err := json.Unmarshal(r.body, &v); err != nil {
			return err
		}
		ids = []string{v.ID}
	}
	if o.n > 0 && len(ids) != o.n {
		return fmt.Errorf("%s created %d transfers, want %d", o.kind, len(ids), o.n)
	}
	c.ids[i], c.replies[i] = ids, r.body
	return nil
}

// richWorld holds every account of the test and the effects of every
// acknowledged request, deterministic and loaded.
type richWorld struct {
	currency map[string]string // account -> currency
	accounts []string
	fixed    [][]effect // the walk-through's acknowledged effects
	clients  []*rClient
}

func (w *richWorld) open(t *testing.T, base, id, currency, normal string, extra string) {
	t.Helper()
	must(t, base, "POST", "/v1/accounts", fmt.Sprintf(`{"id":%q,"currency":%q,"normal_balance":%q%s}`, id, currency, normal, extra), "", 201, nil)
	w.currency[id] = currency
	w.accounts = append(w.accounts, id)
}

func (w *richWorld) model() map[string]totals {
	m := map[string]totals{}
	apply := func(effs []effect) {
		for _, e := range effs {
			v := m[e.account]
			v.DebitsPosted += e.d.DebitsPosted
			v.CreditsPosted += e.d.CreditsPosted
			v.DebitsPending += e.d.DebitsPending
			v.CreditsPending += e.d.CreditsPending
			m[e.account] = v
		}
	}
	for _, effs := range w.fixed {
		apply(effs)
	}
	for _, c := range w.clients {
		for _, o := range c.ops[:c.acked] {
			apply(o.effects)
		}
	}
	return m
}

// check compares every balance with the model, checks debits equal credits
// in each currency and runs the invariant checker.
func (w *richWorld) check(t *testing.T, base string) {
	t.Helper()
	want, got := w.model(), balances(t, base, w.accounts)
	sums := map[string]totals{}
	for _, a := range w.accounts {
		if got[a] != want[a] {
			t.Errorf("account %s: server has %+v, the acknowledged requests give %+v", a, got[a], want[a])
		}
		s := sums[w.currency[a]]
		s.DebitsPosted += got[a].DebitsPosted
		s.CreditsPosted += got[a].CreditsPosted
		s.DebitsPending += got[a].DebitsPending
		s.CreditsPending += got[a].CreditsPending
		sums[w.currency[a]] = s
	}
	for cur, s := range sums {
		if s.DebitsPosted != s.CreditsPosted || s.DebitsPending != s.CreditsPending {
			t.Errorf("%s: debits and credits differ: %+v", cur, s)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	checkInvariants(t, base)
}

// conversions lists every transfer through fx_usd and checks each is one
// whole conversion: posted, both currencies, the rate, and balanced in each
// currency. It returns how many there are.
func conversions(t *testing.T, base string) int {
	t.Helper()
	n, cursor := 0, ""
	for {
		q := url.Values{"account": {"fx_usd"}, "limit": {"1000"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Transfers  []ledger.Transfer `json:"transfers"`
			HasMore    bool              `json:"has_more"`
			NextCursor string            `json:"next_cursor"`
		}
		must(t, base, "GET", "/v1/transfers?"+q.Encode(), "", "", 200, &page)
		for _, tr := range page.Transfers {
			if tr.State != ledger.StatePosted || tr.FX == nil || tr.FX.Base != "USD" || tr.FX.Quote != "EUR" ||
				tr.FX.Rate != fxRate || len(tr.Entries) != 1 || len(tr.Postings) != 5 {
				t.Fatalf("conversion %s is not whole: %+v", tr.ID, tr)
			}
			net := map[string]int64{}
			for _, p := range tr.Postings {
				cur := p.Currency
				if cur == "" {
					cur = tr.Currency
				}
				if p.Direction == ledger.Debit {
					net[cur] += p.Posted
				} else {
					net[cur] -= p.Posted
				}
			}
			if len(net) != 2 || net["USD"] != 0 || net["EUR"] != 0 {
				t.Fatalf("conversion %s does not balance in each currency: %v", tr.ID, net)
			}
			n++
		}
		if !page.HasMore {
			return n
		}
		cursor = page.NextCursor
	}
}

// keyed is one request of the walk-through and the answer it got.
type keyed struct {
	method, path, body, key string
	status                  int
	answer                  []byte
}

// walkThrough runs the three scenarios one step at a time, checking every
// balance after each, and returns its keyed requests with their answers for
// replay after restarts.
func (w *richWorld) walkThrough(t *testing.T, base string) []keyed {
	var out []keyed
	do := func(method, path, body, k string, status int, v any) reply {
		t.Helper()
		r := must(t, base, method, path, body, k, status, v)
		out = append(out, keyed{method, path, body, k, status, r.body})
		return r
	}

	// 1. A split payout. A 100.00 order is captured into escrow and paid
	// out to two sellers and VAT, and the platform sweeps the rest with a
	// balancing transfer, all in one batch.
	var payout batchReply
	do("POST", "/v1/transfers/batch", `{"transfers":[`+
		`{"currency":"EUR","reference":"order-1",`+tp("card_in", "escrow", 10000)+`},`+
		`{"currency":"EUR",`+tp("escrow", "seller_1", 6000)+`},`+
		`{"currency":"EUR",`+tp("escrow", "seller_2", 2500)+`},`+
		`{"currency":"EUR",`+tp("escrow", "vat", 500)+`},`+
		`{"currency":"EUR","balance_debit":true,`+tp("escrow", "platform", 1_000_000)+`}]}`, "walk-payout-1", 201, &payout)
	if len(payout.Results) != 5 {
		t.Fatalf("payout results: %+v", payout)
	}
	var seqs []uint64
	for _, it := range payout.Results {
		if it.Status != "created" || it.Transfer.State != ledger.StatePosted {
			t.Fatalf("payout result %+v", it)
		}
		seqs = append(seqs, it.Transfer.Entries[0].Seq)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[0]+uint64(i) {
			t.Fatalf("payout entries are not consecutive: %v", seqs)
		}
	}
	if b := payout.Results[4].Transfer.Balancing; b == nil || b.Requested != 1_000_000 || b.Amount != 1000 {
		t.Fatalf("platform sweep: %+v", b)
	}
	w.fixed = append(w.fixed, []effect{
		{"card_in", totals{DebitsPosted: 10000}}, {"escrow", totals{CreditsPosted: 10000, DebitsPosted: 10000}},
		{"seller_1", totals{CreditsPosted: 6000}}, {"seller_2", totals{CreditsPosted: 2500}},
		{"vat", totals{CreditsPosted: 500}}, {"platform", totals{CreditsPosted: 1000}},
	})
	w.check(t, base)

	// A payout promising more than the order brought in is refused at the
	// third transfer, and the capture and the first payment are undone.
	var refused batchReply
	do("POST", "/v1/transfers/batch", `{"transfers":[`+
		`{"currency":"EUR",`+tp("card_in", "escrow", 5000)+`},`+
		`{"currency":"EUR",`+tp("escrow", "seller_1", 3000)+`},`+
		`{"currency":"EUR",`+tp("escrow", "seller_3", 2500)+`}]}`, "walk-payout-2", 422, &refused)
	if refused.Error == nil || refused.Error.Code != "limit_exceeded" || refused.Error.Item == nil || *refused.Error.Item != 2 ||
		len(refused.Results) != 3 || refused.Results[0].Status != "not_applied" || refused.Results[1].Status != "not_applied" ||
		refused.Results[2].Status != "failed" {
		t.Fatalf("over-promised payout: %s", out[len(out)-1].answer)
	}
	w.check(t, base)

	// 2. A hotel hold that grows. The guest's card holds 500.00; check-in
	// holds 200.00, the minibar adds 60.00, a 300.00 upgrade is more than
	// the card has left, a late checkout extends the hold and checkout
	// posts 245.00 of the 260.00 held.
	do("POST", "/v1/transfers", `{"currency":"EUR",`+tp("card_in", "guest_card", 50000)+`}`, "walk-guest-fund", 201, nil)
	w.fixed = append(w.fixed, []effect{{"card_in", totals{DebitsPosted: 50000}}, {"guest_card", totals{CreditsPosted: 50000}}})
	var hold ledger.Transfer
	do("POST", "/v1/transfers", `{"currency":"EUR","pending":true,"timeout_seconds":86400,"reference":"stay-1",`+tp("guest_card", "hotel", 20000)+`}`,
		"walk-hold", 201, &hold)
	w.fixed = append(w.fixed, []effect{{"guest_card", totals{DebitsPending: 20000}}, {"hotel", totals{CreditsPending: 20000}}})
	var grown ledger.Transfer
	do("POST", "/v1/transfers/"+hold.ID+"/increase", `{"amount":6000}`, "walk-minibar", 200, &grown)
	if grown.State != ledger.StatePending || grown.Postings[0].Amount != 26000 {
		t.Fatalf("grown hold: %+v", grown)
	}
	w.fixed = append(w.fixed, []effect{{"guest_card", totals{DebitsPending: 6000}}, {"hotel", totals{CreditsPending: 6000}}})
	var tooMuch errReply
	do("POST", "/v1/transfers/"+hold.ID+"/increase", `{"amount":30000}`, "walk-upgrade", 422, &tooMuch)
	if tooMuch.Error.Code != "limit_exceeded" {
		t.Fatalf("upgrade past the card: %+v", tooMuch)
	}
	var extended ledger.Transfer
	do("POST", "/v1/transfers/"+hold.ID+"/extend", `{"timeout_seconds":259200}`, "walk-late-checkout", 200, &extended)
	if !extended.ExpiresAt.After(hold.ExpiresAt.Add(47 * time.Hour)) {
		t.Fatalf("extended hold expires at %v, created to expire at %v", extended.ExpiresAt, hold.ExpiresAt)
	}
	var posted ledger.Transfer
	do("POST", "/v1/transfers/"+hold.ID+"/post", `{"amount":24500}`, "walk-checkout", 200, &posted)
	var kinds []string
	for _, e := range posted.Entries {
		kinds = append(kinds, string(e.Kind))
	}
	if posted.State != ledger.StatePosted || posted.Postings[0].Posted != 24500 ||
		strings.Join(kinds, ",") != "pending,increase_pending,extend_pending,post_pending" {
		t.Fatalf("checkout: %+v (entries %v)", posted, kinds)
	}
	w.fixed = append(w.fixed, []effect{
		{"guest_card", totals{DebitsPending: -26000, DebitsPosted: 24500}},
		{"hotel", totals{CreditsPending: -26000, CreditsPosted: 24500}},
	})
	w.check(t, base)

	// 3. A conversion. Alice sells 100.00 USD for EUR at 0.9234 and pays a
	// 1.00 USD fee: one transfer, one entry, one log record.
	do("POST", "/v1/transfers", `{"currency":"USD",`+tp("usd_in", "alice_usd", 30000)+`}`, "walk-alice-fund", 201, nil)
	w.fixed = append(w.fixed, []effect{{"usd_in", totals{DebitsPosted: 30000}}, {"alice_usd", totals{CreditsPosted: 30000}}})
	body, eff := conversion("alice_usd", "alice_eur", 10000, 100)
	var conv ledger.Transfer
	do("POST", "/v1/transfers", body, "walk-convert", 201, &conv)
	if conv.FX == nil || conv.FX.Rate != fxRate || conv.FX.Source != "ecb" || conv.FX.QuotedAt.Location() != time.UTC ||
		!conv.FX.QuotedAt.Equal(time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)) || len(conv.Entries) != 1 {
		t.Fatalf("conversion: %+v", conv)
	}
	w.fixed = append(w.fixed, eff)
	// The same conversion two cents short of the rate is refused whole.
	bad := strings.Replace(body, `"amount":9234,"currency":"EUR"},{"account":"alice_eur","direction":"credit","amount":9234`,
		`"amount":9232,"currency":"EUR"},{"account":"alice_eur","direction":"credit","amount":9232`, 1)
	if bad == body {
		t.Fatal("could not build the off-rate conversion")
	}
	var offRate errReply
	do("POST", "/v1/transfers", bad, "walk-convert-off-rate", 400, &offRate)
	if offRate.Error.Code != "invalid_argument" || !strings.Contains(offRate.Error.Message, "converts to 92.3400 EUR, not 92.32 EUR") {
		t.Fatalf("off-rate conversion: %+v", offRate)
	}
	w.check(t, base)
	return out
}

// replayWalkThrough resends every keyed request of the walk-through: each
// must replay its original answer byte for byte.
func replayWalkThrough(t *testing.T, base string, walk []keyed) {
	t.Helper()
	for _, k := range walk {
		r, err := httpCall(base, k.method, k.path, k.body, k.key)
		if err != nil {
			t.Fatal(err)
		}
		if r.status != k.status || !r.replayed {
			t.Fatalf("%s: status %d replayed %v, want a replay of %d", k.key, r.status, r.replayed, k.status)
		}
		if k.status < 300 && !bytes.Equal(r.body, k.answer) {
			t.Fatalf("%s: replay differs from the original answer:\n%s\n%s", k.key, r.body, k.answer)
		}
	}
}

// replayLoad resends every acknowledged request of the load phase with its
// key; each must replay its original answer byte for byte. The request each
// client had in flight is resent too: a replay means it was written before
// the kill, anything else that it runs now. Either way it is acknowledged
// afterwards.
func replayLoad(t *testing.T, base string, clients []*rClient) (kept, lost map[string]int) {
	t.Helper()
	kept, lost = map[string]int{}, map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	errc := make(chan error, len(clients))
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < c.acked; i++ {
				r, err := c.do(base, i)
				if err != nil {
					errc <- err
					return
				}
				if !r.replayed || !bytes.Equal(r.body, c.replies[i]) {
					errc <- fmt.Errorf("client %d op %d (%s, key %s) was acknowledged with\n%s\nbut the restored server answered replayed=%v\n%s",
						c.n, i, c.ops[i].kind, c.ops[i].key, c.replies[i], r.replayed, r.body)
					return
				}
			}
			if !c.inflight {
				return
			}
			r, err := c.do(base, c.acked)
			if err != nil {
				errc <- err
				return
			}
			if r.status/100 != 2 {
				errc <- fmt.Errorf("client %d in-flight op %d (%s): %d %s", c.n, c.acked, c.ops[c.acked].kind, r.status, r.body)
				return
			}
			if err := c.record(c.acked, r); err != nil {
				errc <- err
				return
			}
			mu.Lock()
			if r.replayed {
				kept[c.ops[c.acked].kind]++
			} else {
				lost[c.ops[c.acked].kind]++
			}
			mu.Unlock()
			c.acked++
			c.inflight = false
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
	return kept, lost
}

// load runs the clients against the server until at least perRound more
// requests were acknowledged and a new snapshot was written, then kills it
// with SIGKILL. It returns the kind of request each client had unanswered.
func load(t *testing.T, p *proc, dir string, clients []*rClient, perRound int64) []string {
	t.Helper()
	snaps0, _ := files(dir)
	var acked atomic.Int64
	var killed atomic.Bool
	var wg sync.WaitGroup
	errc := make(chan error, len(clients))
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if c.acked == len(c.ops) {
					c.next()
				}
				r, err := c.do(p.base, c.acked)
				if err != nil {
					if !killed.Load() {
						errc <- fmt.Errorf("client %d op %d (%s): %v", c.n, c.acked, c.ops[c.acked].kind, err)
					}
					c.inflight = true
					return
				}
				if r.status/100 != 2 {
					errc <- fmt.Errorf("client %d op %d (%s): %d %s", c.n, c.acked, c.ops[c.acked].kind, r.status, r.body)
					return
				}
				if err := c.record(c.acked, r); err != nil {
					errc <- fmt.Errorf("client %d op %d: %v", c.n, c.acked, err)
					return
				}
				c.acked++
				acked.Add(1)
			}
		}()
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		snaps, _ := files(dir)
		newSnap := len(snaps) > 0 && (len(snaps0) == 0 || snaps[len(snaps)-1] > snaps0[len(snaps0)-1])
		if acked.Load() >= perRound && newSnap {
			break
		}
		if time.Now().After(deadline) {
			p.cmd.Process.Kill()
			t.Fatalf("no new snapshot after %d requests (snapshots %v)", acked.Load(), snaps)
		}
		select {
		case err := <-errc:
			p.cmd.Process.Kill()
			t.Fatal(err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	killed.Store(true)
	p.kill(t)
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
	var inflight []string
	for _, c := range clients {
		if c.inflight {
			inflight = append(inflight, c.ops[c.acked].kind)
		}
	}
	t.Logf("killed after %d acknowledged requests; unanswered: %v", acked.Load(), inflight)
	return inflight
}

// TestRicherTransfersSurviveKillNine walks a split payout, a growing hotel
// hold and a USD to EUR conversion through the real binary, then runs them
// under load from concurrent clients (two of which only convert) and kills
// the server with SIGKILL twice. After each kill the data directory is
// copied, ledgerd verify must pass on the copy and the next server runs on
// it: every acknowledged request must replay its original answer byte for
// byte, every balance must equal the acknowledged requests, debits must
// equal credits in each currency, and every conversion through the exchange
// accounts must be whole. A batch or conversion half written before the
// kill would break the balances, the per-currency sums or the conversion
// listing. After a clean stop the copy verifies as sound and restarts to
// the same balances.
func TestRicherTransfersSurviveKillNine(t *testing.T) {
	const rounds, nClients, perRound = 2, 6, 600
	root := t.TempDir()
	dir := filepath.Join(root, "data-0")
	p := spawn(t, dir)

	w := &richWorld{currency: map[string]string{}}
	w.open(t, p.base, "card_in", "EUR", "debit", "")
	w.open(t, p.base, "escrow", "EUR", "credit", `,"non_negative":true`)
	for _, a := range []string{"seller_1", "seller_2", "seller_3", "vat", "platform", "hotel"} {
		w.open(t, p.base, a, "EUR", "credit", "")
	}
	w.open(t, p.base, "guest_card", "EUR", "credit", `,"non_negative":true`)
	w.open(t, p.base, "usd_in", "USD", "debit", "")
	w.open(t, p.base, "alice_usd", "USD", "credit", `,"non_negative":true`)
	w.open(t, p.base, "alice_eur", "EUR", "credit", "")
	w.open(t, p.base, "fees_usd", "USD", "credit", "")
	w.open(t, p.base, "fx_usd", "USD", "credit", `,"exchange":true`)
	w.open(t, p.base, "fx_eur", "EUR", "credit", `,"exchange":true`)
	for i := range nClients {
		c := &rClient{n: i, rng: rand.New(rand.NewPCG(uint64(i), 3))}
		if i < 2 {
			c.only = "convert"
		}
		w.open(t, p.base, c.esc(), "EUR", "credit", `,"non_negative":true`)
		w.open(t, p.base, c.guest(), "EUR", "credit", `,"non_negative":true`)
		w.open(t, p.base, c.usd(), "USD", "credit", `,"non_negative":true`)
		w.open(t, p.base, c.eur(), "EUR", "credit", "")
		w.clients = append(w.clients, c)
	}

	walk := w.walkThrough(t, p.base)
	unansweredConversions := 0
	for round := 1; round <= rounds; round++ {
		inflight := load(t, p, dir, w.clients, perRound)
		for _, k := range inflight {
			if k == "convert" {
				unansweredConversions++
			}
		}
		next := filepath.Join(root, fmt.Sprintf("data-%d", round))
		copyDir(t, dir, next)
		if code, out := verifyDir(t, next); code != exitSound && code != exitRepairable {
			t.Fatalf("round %d: verify exit %d on the copied directory:\n%s", round, code, out)
		}
		dir = next
		p = spawn(t, dir)
		t.Logf("round %d: recovered head %v in %v ms", round, p.recovered["head"], p.recovered["duration_ms"])

		replayWalkThrough(t, p.base, walk)
		kept, lost := replayLoad(t, p.base, w.clients)
		t.Logf("round %d: unanswered requests found written %v, not written %v", round, kept, lost)
		w.check(t, p.base)
		want := 1
		for _, c := range w.clients {
			for _, o := range c.ops[:c.acked] {
				if o.kind == "convert" {
					want++
				}
			}
		}
		if got := conversions(t, p.base); got != want {
			t.Fatalf("round %d: %d conversions through fx_usd, the acknowledged requests made %d", round, got, want)
		}
	}
	if unansweredConversions < rounds*2 {
		t.Fatalf("only %d conversions were unanswered at the kills", unansweredConversions)
	}
	counts := map[string]int{}
	for _, c := range w.clients {
		for _, o := range c.ops[:c.acked] {
			counts[o.kind]++
		}
	}
	t.Logf("acknowledged load requests: %v", counts)
	for _, k := range []string{"batch", "hold", "increase", "extend", "post", "convert"} {
		if counts[k] == 0 {
			t.Fatalf("the load never made a %s request: %v", k, counts)
		}
	}

	p.stop(t)
	if code, out := verifyDir(t, dir); code != exitSound {
		t.Fatalf("verify after clean shutdown: exit %d\n%s", code, out)
	}
	p = spawn(t, dir)
	w.check(t, p.base)
	replayWalkThrough(t, p.base, walk)
	p.stop(t)
}
