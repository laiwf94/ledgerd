package ledger

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

const testSecret = "whsec_0123456789abcdefghijklmnop"

func hookCmd(id, url string, after *uint64) Command {
	return Command{CreateWebhook: &WebhookSpec{ID: id, URL: url, Secret: testSecret, AfterSeq: after}}
}

func progressCmd(id string, version, through uint64, dead *DeadLetterSpec) Command {
	return Command{WebhookProgress: &WebhookProgressSpec{ID: id, Version: version, Through: through, Dead: dead}}
}

func (e *env) hook(id string) *Webhook {
	e.t.Helper()
	h, err := e.l.Webhook(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return h
}

// writeSmallWebhooks adds every kind of webhook state to writeSmallSnapshot's
// ledger: filtered and unfiltered endpoints, a keyed create and a keyed
// refusal, positions moved by deliveries and by a replay, dead letters
// replaying, failed again and redelivered, and a deleted endpoint.
func writeSmallWebhooks(t *testing.T, e *env) {
	t.Helper()
	ctx := context.Background()
	spec := &WebhookSpec{ID: "orders", URL: "https://hooks.example.test/ledger?src=1", Secret: testSecret, Account: "bank",
		Kinds: []EntryKind{KindPosted, KindPending}, Currency: "USD", MaxBatch: 7, AfterSeq: ptr(uint64(2))}
	if _, err := e.l.Do(ctx, Command{CreateWebhook: spec}, key("k9", "hook", "hook")); err != nil {
		t.Fatal(err)
	}
	e.l.Do(ctx, hookCmd("orders", "https://other.example.test/", nil), key("k9", "hook-again", "hook-again"))
	e.do(hookCmd("all", "http://127.0.0.1:9/all", ptr(uint64(0))))
	e.do(hookCmd("gone", "https://gone.example.test/", nil))
	e.do(Command{DeleteWebhook: &WebhookRef{ID: "gone"}})
	h := e.hook("all")
	e.do(progressCmd("all", h.Version, 3, nil))
	for i := uint64(0); i < 3; i++ {
		h = e.hook("all")
		e.do(progressCmd("all", h.Version, h.Cursor+2, &DeadLetterSpec{AfterSeq: h.Cursor, ThroughSeq: h.Cursor + 1, Entries: 1,
			Attempts: 20, Error: fmt.Sprintf("the endpoint answered %d", 500+i)}))
	}
	e.do(Command{ReplayWebhook: &WebhookReplaySpec{ID: "all", DeadLetters: []uint64{1, 3}}})
	h = e.hook("all")
	e.do(Command{WebhookProgress: &WebhookProgressSpec{ID: "all", Version: h.Version, Failed: &RedeliveryFailed{DeadLetter: 1, Attempts: 4, Error: "timeout"}}})
	h = e.hook("all")
	e.do(Command{WebhookProgress: &WebhookProgressSpec{ID: "all", Version: h.Version, Redelivered: 3}})
	e.do(Command{ReplayWebhook: &WebhookReplaySpec{ID: "all", DeadLetters: []uint64{2}}})
	if _, err := e.l.Do(ctx, Command{ReplayWebhook: &WebhookReplaySpec{ID: "orders", AfterSeq: ptr(uint64(1))}}, key("k9", "replay", "replay")); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookSnapshotRoundTrip(t *testing.T) {
	dir, clock := writeSmallSnapshot(t, snapVersion)
	fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
	full := openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "small", fromSnap, full)
	ctx := context.Background()
	for _, l := range []*Ledger{fromSnap, full} {
		hooks := l.Webhooks()
		if len(hooks) != 3 || hooks[0].ID != "all" || hooks[1].ID != "orders" || hooks[2].ID != "reserve-alerts" {
			t.Fatalf("webhooks %+v", hooks)
		}
		all := hooks[0]
		if all.Cursor != 9 || len(all.DeadLetters) != 2 || all.NextDeadLetter != 4 {
			t.Fatalf("all: %+v", all)
		}
		d1, d2 := all.DeadLetters[0], all.DeadLetters[1]
		if d1.ID != 1 || d1.Replaying || d1.Attempts != 24 || d1.Error != "timeout" || d2.ID != 2 || !d2.Replaying || d2.Error != "the endpoint answered 501" ||
			d2.AfterSeq != 5 || d2.ThroughSeq != 6 {
			t.Fatalf("dead letters %+v", all.DeadLetters)
		}
		orders := hooks[1]
		if orders.Cursor != 1 || orders.MaxBatch != 7 || orders.Account != "bank" || orders.Currency != "USD" ||
			!reflect.DeepEqual(orders.Kinds, []EntryKind{KindPosted, KindPending}) || orders.Secret != testSecret {
			t.Fatalf("orders: %+v", orders)
		}
		if orders.Version <= all.Version {
			t.Fatalf("versions %d and %d: the replay came last", orders.Version, all.Version)
		}
		if _, err := l.Webhook("gone"); codeOf(err) != CodeNotFound {
			t.Fatalf("deleted webhook: %v", err)
		}
		// Keyed results replay from the snapshot exactly as from the log.
		spec := &WebhookSpec{ID: "orders", URL: "https://hooks.example.test/ledger?src=1", Secret: testSecret, Account: "bank",
			Kinds: []EntryKind{KindPosted, KindPending}, Currency: "USD", MaxBatch: 7, AfterSeq: ptr(uint64(2))}
		out, err := l.Do(ctx, Command{CreateWebhook: spec}, key("k9", "hook", "hook"))
		if err != nil || !out.Replayed || out.Webhook == nil || out.Webhook.Cursor != 2 || out.Webhook.Secret != testSecret {
			t.Fatalf("replayed create: %v %+v", err, out.Webhook)
		}
		if out, err := l.Do(ctx, hookCmd("orders", "https://other.example.test/", nil), key("k9", "hook-again", "hook-again")); codeOf(err) != CodeAlreadyExists || !out.Replayed {
			t.Fatalf("replayed refusal: %v", err)
		}
		out, err = l.Do(ctx, Command{ReplayWebhook: &WebhookReplaySpec{ID: "orders", AfterSeq: ptr(uint64(1))}}, key("k9", "replay", "replay"))
		if err != nil || !out.Replayed || out.Webhook.Cursor != 1 || out.Webhook.Version != orders.Version {
			t.Fatalf("replayed replay: %v %+v", err, out.Webhook)
		}
		if v := allChecks(l); len(v) > 0 {
			t.Fatal(v)
		}
	}
	// A new version continues from the counter the snapshot kept.
	h, _ := fromSnap.Webhook("orders")
	var newest uint64
	for _, w := range fromSnap.Webhooks() {
		newest = max(newest, w.Version)
	}
	out, err := fromSnap.Do(ctx, progressCmd("orders", h.Version, 4, nil), nil)
	if err != nil || out.Webhook.Version != newest+1 {
		t.Fatalf("progress after loading: %v %+v", err, out.Webhook)
	}
}

func TestWebhookRegistry(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	head := e.l.Head()
	h := e.do(hookCmd("a", "https://example.test/a", nil)).Webhook
	if h.Cursor != head || h.Version != 1 || h.MaxBatch != DefaultWebhookBatch || h.CreatedAt != e.clock.Now() || len(h.DeadLetters) != 0 {
		t.Fatalf("created %+v", h)
	}
	if h := e.do(hookCmd("b", "http://localhost:8081/x", ptr(uint64(0)))).Webhook; h.Cursor != 0 || h.Version != 2 {
		t.Fatalf("from the start %+v", h)
	}
	e.fail(hookCmd("c", "https://example.test/", ptr(head+1)), CodeInvalid)
	e.fail(hookCmd("a", "https://example.test/again", nil), CodeAlreadyExists)
	if hooks := e.l.Webhooks(); len(hooks) != 2 || hooks[0].ID != "a" || hooks[1].ID != "b" {
		t.Fatalf("listing %+v", hooks)
	}
	out := e.do(Command{DeleteWebhook: &WebhookRef{ID: "a"}})
	if out.Webhook.ID != "a" {
		t.Fatalf("deleted %+v", out.Webhook)
	}
	e.fail(Command{DeleteWebhook: &WebhookRef{ID: "a"}}, CodeNotFound)
	if _, err := e.l.Webhook("a"); codeOf(err) != CodeNotFound {
		t.Fatal(err)
	}
	// Created again under the same ID, it starts over with a version
	// never used before.
	if h := e.do(hookCmd("a", "https://example.test/a2", nil)).Webhook; h.Version != 3 || h.URL != "https://example.test/a2" {
		t.Fatalf("recreated %+v", h)
	}
	e.fail(progressCmd("a", 1, head+1, nil), CodePrecondition)
	e.restart()
	if h := e.hook("a"); h.Version != 3 || h.Cursor != head {
		t.Fatalf("after restart %+v", h)
	}
	e.invariants()
}

func TestWebhookLimit(t *testing.T) {
	e := newEnv(t, false)
	for i := 0; i < MaxWebhooks; i++ {
		e.do(hookCmd(fmt.Sprintf("h%d", i), "https://example.test/", nil))
	}
	e.fail(hookCmd("one-more", "https://example.test/", nil), CodeLimitExceeded)
	e.do(Command{DeleteWebhook: &WebhookRef{ID: "h0"}})
	e.do(hookCmd("one-more", "https://example.test/", nil))
}

func TestWebhookValidation(t *testing.T) {
	long := "https://example.test/" + strings.Repeat("a", MaxWebhookURL)
	bad := map[string]Command{
		"bad id":           hookCmd("-x", "https://example.test/", nil),
		"no url":           hookCmd("x", "", nil),
		"ftp":              hookCmd("x", "ftp://example.test/", nil),
		"relative":         hookCmd("x", "/hooks", nil),
		"no host":          hookCmd("x", "https:///x", nil),
		"credentials":      hookCmd("x", "https://user:pw@example.test/", nil),
		"fragment":         hookCmd("x", "https://example.test/#f", nil),
		"space":            hookCmd("x", "https://example.test/a b", nil),
		"control":          hookCmd("x", "https://example.test/\x01", nil),
		"long url":         hookCmd("x", long, nil),
		"short secret":     {CreateWebhook: &WebhookSpec{ID: "x", URL: "https://example.test/", Secret: "short"}},
		"secret space":     {CreateWebhook: &WebhookSpec{ID: "x", URL: "https://example.test/", Secret: testSecret + " x"}},
		"long secret":      {CreateWebhook: &WebhookSpec{ID: "x", URL: "https://example.test/", Secret: strings.Repeat("s", MaxWebhookSecret+1)}},
		"bad account":      {CreateWebhook: &WebhookSpec{ID: "x", URL: "https://example.test/", Secret: testSecret, Account: "-a"}},
		"bad kind":         {CreateWebhook: &WebhookSpec{ID: "x", URL: "https://example.test/", Secret: testSecret, Kinds: []EntryKind{"nope"}}},
		"kind twice":       {CreateWebhook: &WebhookSpec{ID: "x", URL: "https://example.test/", Secret: testSecret, Kinds: []EntryKind{KindPosted, KindPosted}}},
		"bad currency":     {CreateWebhook: &WebhookSpec{ID: "x", URL: "https://example.test/", Secret: testSecret, Currency: "XAU"}},
		"batch too big":    {CreateWebhook: &WebhookSpec{ID: "x", URL: "https://example.test/", Secret: testSecret, MaxBatch: MaxWebhookBatch + 1}},
		"negative batch":   {CreateWebhook: &WebhookSpec{ID: "x", URL: "https://example.test/", Secret: testSecret, MaxBatch: -1}},
		"delete bad id":    {DeleteWebhook: &WebhookRef{ID: ""}},
		"progress no id":   progressCmd("", 1, 1, nil),
		"progress no ver":  progressCmd("x", 0, 1, nil),
		"progress nothing": {WebhookProgress: &WebhookProgressSpec{ID: "x", Version: 1}},
		"progress two":     {WebhookProgress: &WebhookProgressSpec{ID: "x", Version: 1, Through: 2, Redelivered: 1}},
		"dead no through":  {WebhookProgress: &WebhookProgressSpec{ID: "x", Version: 1, Redelivered: 1, Dead: &DeadLetterSpec{AfterSeq: 0, ThroughSeq: 1, Entries: 1, Attempts: 1}}},
		"dead empty":       progressCmd("x", 1, 5, &DeadLetterSpec{AfterSeq: 3, ThroughSeq: 3, Entries: 1, Attempts: 1}),
		"dead past":        progressCmd("x", 1, 5, &DeadLetterSpec{AfterSeq: 3, ThroughSeq: 6, Entries: 1, Attempts: 1}),
		"dead no entries":  progressCmd("x", 1, 5, &DeadLetterSpec{AfterSeq: 3, ThroughSeq: 4, Entries: 0, Attempts: 1}),
		"dead no attempts": progressCmd("x", 1, 5, &DeadLetterSpec{AfterSeq: 3, ThroughSeq: 4, Entries: 1, Attempts: 0}),
		"dead long error":  progressCmd("x", 1, 5, &DeadLetterSpec{AfterSeq: 3, ThroughSeq: 4, Entries: 1, Attempts: 1, Error: strings.Repeat("e", MaxWebhookError+1)}),
		"failed no letter": {WebhookProgress: &WebhookProgressSpec{ID: "x", Version: 1, Failed: &RedeliveryFailed{Attempts: 1}}},
		"replay nothing":   {ReplayWebhook: &WebhookReplaySpec{ID: "x"}},
		"replay two":       {ReplayWebhook: &WebhookReplaySpec{ID: "x", AfterSeq: ptr(uint64(0)), AllDeadLetters: true}},
		"replay zero id":   {ReplayWebhook: &WebhookReplaySpec{ID: "x", DeadLetters: []uint64{0}}},
		"replay dup ids":   {ReplayWebhook: &WebhookReplaySpec{ID: "x", DeadLetters: []uint64{2, 2}}},
		"replay bad id":    {ReplayWebhook: &WebhookReplaySpec{ID: "", AllDeadLetters: true}},
		"two commands":     {CreateWebhook: &WebhookSpec{ID: "x"}, DeleteWebhook: &WebhookRef{ID: "x"}},
	}
	e := newEnv(t, false)
	for name, c := range bad {
		if _, err := e.l.Do(context.Background(), c, nil); codeOf(err) != CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, u := range []string{"https://example.test", "http://[::1]:8080/a?b=c", "https://example.test:8443/hooks/ledger"} {
		if _, err := e.l.Do(context.Background(), hookCmd("ok", u, nil), nil); err != nil {
			t.Errorf("%s: %v", u, err)
		}
		e.do(Command{DeleteWebhook: &WebhookRef{ID: "ok"}})
	}
}

func TestWebhookProgressAndDeadLetters(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	for i := 0; i < 10; i++ {
		e.do(move(dr("bank", 1), cr("alice", 1)))
	}
	head := e.l.Head()
	h := e.do(hookCmd("w", "https://example.test/", ptr(uint64(0)))).Webhook
	e.fail(progressCmd("w", h.Version, 0+head+1, nil), CodeInvalid) // past the head
	h = e.do(progressCmd("w", h.Version, 4, nil)).Webhook
	if h.Cursor != 4 {
		t.Fatalf("cursor %d", h.Cursor)
	}
	e.fail(progressCmd("w", h.Version-1, 6, nil), CodePrecondition) // stale version
	e.fail(progressCmd("w", h.Version, 4, nil), CodeInvalid)        // not forward
	e.fail(progressCmd("w", h.Version, 3, nil), CodeInvalid)
	e.fail(progressCmd("w", h.Version, 8, &DeadLetterSpec{AfterSeq: 3, ThroughSeq: 6, Entries: 2, Attempts: 1}), CodeInvalid) // before the position
	h = e.do(progressCmd("w", h.Version, 8, &DeadLetterSpec{AfterSeq: 4, ThroughSeq: 7, Entries: 3, Attempts: 20, Error: "the endpoint answered 503"})).Webhook
	if h.Cursor != 8 || len(h.DeadLetters) != 1 || h.DeadLetters[0] != (DeadLetter{ID: 1, AfterSeq: 4, ThroughSeq: 7, Entries: 3, Attempts: 20,
		Error: "the endpoint answered 503", FailedAt: e.clock.Now()}) {
		t.Fatalf("after dead letter %+v", h)
	}
	before := h
	// Redelivery reports only apply to a dead letter being replayed.
	e.fail(Command{WebhookProgress: &WebhookProgressSpec{ID: "w", Version: h.Version, Redelivered: 1}}, CodePrecondition)
	e.fail(Command{WebhookProgress: &WebhookProgressSpec{ID: "w", Version: h.Version, Failed: &RedeliveryFailed{DeadLetter: 1, Attempts: 1}}}, CodePrecondition)
	e.fail(Command{ReplayWebhook: &WebhookReplaySpec{ID: "w", DeadLetters: []uint64{9}}}, CodeNotFound)
	e.fail(Command{ReplayWebhook: &WebhookReplaySpec{ID: "nope", AllDeadLetters: true}}, CodeNotFound)
	if got := e.hook("w"); got != before {
		t.Fatal("refused commands changed the endpoint")
	}
	h = e.do(Command{ReplayWebhook: &WebhookReplaySpec{ID: "w", AllDeadLetters: true}}).Webhook
	if !h.DeadLetters[0].Replaying || before.DeadLetters[0].Replaying {
		t.Fatalf("replay marking %+v, earlier version %+v", h.DeadLetters, before.DeadLetters)
	}
	e.clock.Advance(time.Minute)
	h = e.do(Command{WebhookProgress: &WebhookProgressSpec{ID: "w", Version: h.Version, Failed: &RedeliveryFailed{DeadLetter: 1, Attempts: 5, Error: "again"}}}).Webhook
	if d := h.DeadLetters[0]; d.Replaying || d.Attempts != 25 || d.Error != "again" || d.FailedAt != e.clock.Now() {
		t.Fatalf("failed again %+v", d)
	}
	h = e.do(Command{ReplayWebhook: &WebhookReplaySpec{ID: "w", DeadLetters: []uint64{1}}}).Webhook
	h = e.do(Command{WebhookProgress: &WebhookProgressSpec{ID: "w", Version: h.Version, Redelivered: 1}}).Webhook
	if len(h.DeadLetters) != 0 || h.NextDeadLetter != 2 || h.Cursor != 8 {
		t.Fatalf("redelivered %+v", h)
	}
	// A replay moves the position back, never forward, and a delivery
	// computed before it is refused.
	stale := h.Version
	e.fail(Command{ReplayWebhook: &WebhookReplaySpec{ID: "w", AfterSeq: ptr(uint64(9))}}, CodeInvalid)
	h = e.do(Command{ReplayWebhook: &WebhookReplaySpec{ID: "w", AfterSeq: ptr(uint64(2))}}).Webhook
	if h.Cursor != 2 {
		t.Fatalf("replayed to %d", h.Cursor)
	}
	e.fail(progressCmd("w", stale, 10, nil), CodePrecondition)
	if e.hook("w").Cursor != 2 {
		t.Fatal("a stale delivery undid the replay")
	}
	want := e.hook("w")
	e.restart()
	if got := e.hook("w"); !reflect.DeepEqual(got, want) {
		t.Fatalf("after restart %+v, want %+v", got, want)
	}
	e.invariants()
}

func TestDeadLetterListIsBounded(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	for e.l.Head() < MaxDeadLetters+2 {
		e.do(move(dr("bank", 1), cr("alice", 1)))
	}
	h := e.do(hookCmd("w", "https://example.test/", ptr(uint64(0)))).Webhook
	for i := uint64(0); i < MaxDeadLetters; i++ {
		h = e.do(progressCmd("w", h.Version, i+1, &DeadLetterSpec{AfterSeq: i, ThroughSeq: i + 1, Entries: 1, Attempts: 1})).Webhook
	}
	e.fail(progressCmd("w", h.Version, MaxDeadLetters+1, &DeadLetterSpec{AfterSeq: MaxDeadLetters, ThroughSeq: MaxDeadLetters + 1, Entries: 1, Attempts: 1}), CodeLimitExceeded)
	// Deliveries still move the position.
	h = e.do(progressCmd("w", h.Version, MaxDeadLetters+1, nil)).Webhook
	if len(h.DeadLetters) != MaxDeadLetters || h.Cursor != MaxDeadLetters+1 {
		t.Fatalf("%d dead letters at %d", len(h.DeadLetters), h.Cursor)
	}
	e.invariants()
}

// Webhook commands change no balance, version or journal entry, and wake
// webhook waiters but not entry streams.
func TestWebhookCommandsTouchNothingElse(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	before := e.account("alice")
	head := e.l.Head()
	grown := e.l.EntriesGrown()
	changed := e.l.WebhooksChanged()
	e.do(hookCmd("w", "https://example.test/", nil))
	select {
	case <-changed:
	default:
		t.Fatal("creating a webhook did not signal WebhooksChanged")
	}
	select {
	case <-grown:
		t.Fatal("a webhook command woke entry streams")
	default:
	}
	changed = e.l.WebhooksChanged()
	e.do(move(dr("bank", 1), cr("alice", 1)))
	select {
	case <-changed:
		t.Fatal("a transfer signalled WebhooksChanged")
	default:
	}
	if e.l.Head() != head+1 || e.account("alice").Version != before.Version+1 {
		t.Fatal("webhook command wrote an entry")
	}
}

func TestHookProblems(t *testing.T) {
	good := &Webhook{ID: "w", Version: 2, Cursor: 5, NextDeadLetter: 3, DeadLetters: []DeadLetter{{ID: 1, AfterSeq: 1, ThroughSeq: 2}, {ID: 2, AfterSeq: 6, ThroughSeq: 9}}}
	cases := map[string]func(h *Webhook){
		"":              func(h *Webhook) {},
		"filed under":   func(h *Webhook) { h.ID = "v" },
		"past the head": func(h *Webhook) { h.Cursor = 11 },
		"version 0":     func(h *Webhook) { h.Version = 0 },
		"version 9":     func(h *Webhook) { h.Version = 9 },
		"malformed dead": func(h *Webhook) {
			h.DeadLetters = []DeadLetter{{ID: 2, AfterSeq: 1, ThroughSeq: 2}, {ID: 1, AfterSeq: 1, ThroughSeq: 2}}
		},
		"malformed dead lett": func(h *Webhook) { h.DeadLetters = []DeadLetter{{ID: 1, AfterSeq: 1, ThroughSeq: 11}} },
		"dead letter 3":       func(h *Webhook) { h.DeadLetters = []DeadLetter{{ID: 3, AfterSeq: 1, ThroughSeq: 2}} },
		"holds 1001":          func(h *Webhook) { h.DeadLetters = make([]DeadLetter, MaxDeadLetters+1) },
	}
	for want, mutate := range cases {
		h := *good
		h.DeadLetters = append([]DeadLetter(nil), good.DeadLetters...)
		mutate(&h)
		var got []string
		hookProblems(map[string]*Webhook{"w": &h}, 8, 10, func(f string, a ...any) { got = append(got, fmt.Sprintf(f, a...)) })
		switch {
		case want == "" && len(got) > 0:
			t.Errorf("sound endpoint: %v", got)
		case want != "" && (len(got) == 0 || !strings.Contains(got[0], strings.TrimSuffix(want, " lett"))):
			t.Errorf("%s: %v", want, got)
		}
	}
	many := map[string]*Webhook{}
	for i := 0; i <= MaxWebhooks; i++ {
		id := fmt.Sprint(i)
		many[id] = &Webhook{ID: id, Version: 1, NextDeadLetter: 1}
	}
	var got []string
	hookProblems(many, 1, 0, func(f string, a ...any) { got = append(got, fmt.Sprintf(f, a...)) })
	if len(got) != 1 || !strings.Contains(got[0], "more than") {
		t.Fatalf("too many endpoints: %v", got)
	}
}
