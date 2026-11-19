package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
	"github.com/laiwf94/ledgerd/internal/webhook"
)

// alertFixture opens alice, bank and merchant, watches alice for posted at
// least 100 and moves money so that entries 1 to 6 raise alerts 1 to 4:
//
//	1 bank -> alice 150      alert 1, triggered
//	2 bank -> merchant 5     none
//	3 alice -> merchant 100  alert 2, cleared
//	4 bank -> alice 60       alert 3, triggered
//	5 bank -> merchant 5     none
//	6 alice -> merchant 20   alert 4, cleared
func alertFixture(t *testing.T) *fixture {
	f := newFixture(t, nil)
	f.accounts()
	f.must(201, "POST", "/v1/balance-monitors", `{"id":"alice-100","account":"alice","condition":{"field":"posted","operator":"greater_than_or_equals","value":100}}`)
	f.pay("bank", "alice", 150)
	f.pay("bank", "merchant", 5)
	f.pay("alice", "merchant", 100)
	f.pay("bank", "alice", 60)
	f.pay("bank", "merchant", 5)
	f.pay("alice", "merchant", 20)
	return f
}

type alertedEntry struct {
	Seq    uint64 `json:"seq"`
	Alerts []struct {
		Seq      uint64 `json:"seq"`
		Event    string `json:"event"`
		Monitor  string `json:"monitor"`
		EntrySeq uint64 `json:"entry_seq"`
		Balance  struct {
			Posted int64 `json:"posted"`
		} `json:"balance"`
	} `json:"alerts"`
}

func TestEntriesFeedCarriesAlertsOverHTTP(t *testing.T) {
	f := alertFixture(t)
	page := func(q string) []alertedEntry {
		t.Helper()
		r := f.call("GET", "/v1/entries?"+q, "")
		if r.status != 200 {
			t.Fatalf("%s: %d %s", q, r.status, r.body)
		}
		var p struct {
			Entries []alertedEntry `json:"entries"`
		}
		if err := json.Unmarshal(r.body, &p); err != nil {
			t.Fatal(err)
		}
		return p.Entries
	}
	// Without alerts the entries carry no alerts field at all.
	if r := f.call("GET", "/v1/entries", ""); strings.Contains(string(r.body), `"alerts"`) {
		t.Fatalf("plain feed %s", r.body)
	}
	got := page("alerts=only")
	if len(got) != 4 || got[0].Seq != 1 || got[1].Seq != 3 || got[2].Seq != 4 || got[3].Seq != 6 {
		t.Fatalf("alerts=only %+v", got)
	}
	for i, e := range got {
		a := e.Alerts
		want := []string{"triggered", "cleared"}[i%2]
		if len(a) != 1 || a[0].Seq != uint64(i+1) || a[0].Event != want || a[0].EntrySeq != e.Seq || a[0].Monitor != "alice-100" {
			t.Fatalf("entry %d alerts %+v", e.Seq, a)
		}
	}
	if got[0].Alerts[0].Balance.Posted != 150 {
		t.Fatalf("balance %+v", got[0].Alerts[0])
	}
	// include: merchant's entries plus the entries that raised alerts.
	if got := page("account=merchant&alerts=include&after_seq=1"); len(got) != 5 || got[0].Seq != 2 || len(got[0].Alerts) != 0 ||
		got[2].Seq != 4 || len(got[2].Alerts) != 1 {
		t.Fatalf("merchant + alerts %+v", got)
	}
	if got := page("alerts=only&alert_monitor=other"); len(got) != 0 {
		t.Fatalf("other monitor %+v", got)
	}
	if got := page("alerts=only&alert_account=alice&limit=1&after_seq=3"); len(got) != 1 || got[0].Seq != 4 {
		t.Fatalf("paged %+v", got)
	}
	for _, q := range []string{"alerts=all", "alert_monitor=alice-100", "alert_account=alice", "alerts=", "alerts=only&alert_monitor=a%20b"} {
		if r := f.call("GET", "/v1/entries?"+q, ""); r.status != 400 || r.code() != "invalid_argument" {
			t.Errorf("%s: %d %s", q, r.status, r.body)
		}
	}
}

func TestEntryStreamCarriesAlertsAndResumes(t *testing.T) {
	f := alertFixture(t)
	read := func(s *sseStream, n int) []alertedEntry {
		t.Helper()
		var out []alertedEntry
		for len(out) < n {
			ev := s.next()
			if ev.event != "entry" {
				continue
			}
			var e alertedEntry
			if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
				t.Fatal(err)
			}
			if ev.id != strconv.FormatUint(e.Seq, 10) {
				t.Fatalf("event id %s for entry %d", ev.id, e.Seq)
			}
			out = append(out, e)
		}
		return out
	}
	s := f.openStream("alerts=only", "")
	got := read(s, 3)
	if got[0].Seq != 1 || got[1].Seq != 3 || got[2].Seq != 4 || got[2].Alerts[0].Seq != 3 {
		t.Fatalf("stream %+v", got)
	}
	s.close()
	// The consumer had processed entry 4 (alert 3); it resumes from there,
	// missing nothing committed while it was away.
	f.pay("bank", "alice", 200) // 7: alert 5, triggered
	f.pay("bank", "merchant", 1)
	s = f.openStream("alerts=only", "4")
	got = read(s, 2)
	if got[0].Seq != 6 || got[0].Alerts[0].Seq != 4 || got[1].Seq != 7 || got[1].Alerts[0].Seq != 5 || got[1].Alerts[0].Event != "triggered" {
		t.Fatalf("resumed %+v", got)
	}
	// Live.
	f.pay("alice", "merchant", 290) // 9: alert 6, cleared
	if got := read(s, 1); got[0].Seq != 9 || got[0].Alerts[0].Seq != 6 {
		t.Fatalf("live %+v", got)
	}
	s.close()
	// A plain stream carries no alerts.
	s = f.openStream("after_seq=6", "")
	for _, ev := range []sseEvent{s.next(), s.next()} {
		if strings.Contains(ev.data, "alerts") {
			t.Fatalf("plain stream event %+v", ev)
		}
	}
	if _, status, body := openStreamAt(t, f.srv.URL, "alerts=later", ""); status != 400 {
		t.Fatalf("bad mode: %d %s", status, body)
	}
	// 4 on the first stream's first page, then 2 and 1 on the second.
	if !strings.Contains(f.call("GET", "/metrics", "").String(), "ledgerd_entry_stream_alerts_total 7") {
		t.Fatalf("stream alert metric: %s", f.call("GET", "/metrics", "").String())
	}
}

func TestWebhookAlertsOverHTTP(t *testing.T) {
	var mu sync.Mutex
	var got []webhook.Delivery
	var secret string
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if err := webhook.Verify(secret, r.Header.Get(webhook.SignatureHeader), body, time.Now(), 0); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		var d webhook.Delivery
		if err := json.Unmarshal(body, &d); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got = append(got, d)
	}))
	defer recv.Close()
	f := alertFixture(t)
	d, err := webhook.Start(webhook.Options{Ledger: f.l, Metrics: f.api.m, BackoffBase: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	mu.Lock()
	created := f.must(201, "POST", "/v1/webhooks", `{"id":"alerts","url":"`+recv.URL+`","after_seq":0,"alerts":"only","alert_monitor":"alice-100"}`)
	secret = created["secret"].(string)
	mu.Unlock()
	if created["alerts"] != "only" || created["alert_monitor"] != "alice-100" {
		t.Fatalf("created %v", created)
	}
	list := f.must(200, "GET", "/v1/webhooks", "")["webhooks"].([]any)[0].(map[string]any)
	if list["alerts"] != "only" || list["alert_monitor"] != "alice-100" {
		t.Fatalf("listed %v", list)
	}
	deadline := time.Now().Add(10 * time.Second)
	var alerts []ledger.BalanceAlert
	for len(alerts) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("received %+v", alerts)
		}
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		alerts = alerts[:0]
		for _, d := range got {
			for _, e := range d.Entries {
				alerts = append(alerts, e.Alerts...)
			}
		}
		mu.Unlock()
	}
	for i, a := range alerts {
		if a.Seq != uint64(i+1) || a.Monitor != "alice-100" || a.Account != "alice" {
			t.Fatalf("alert %d: %+v", i, a)
		}
	}
	for _, body := range []string{
		`{"id":"x","url":"https://h.example.test/","alerts":"sometimes"}`,
		`{"id":"x","url":"https://h.example.test/","alert_account":"alice"}`,
	} {
		if r := f.call("POST", "/v1/webhooks", body); r.status != 400 || r.code() != "invalid_argument" {
			t.Errorf("%s: %d %s", body, r.status, r.body)
		}
	}
}
