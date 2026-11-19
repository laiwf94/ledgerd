package ledger

import (
	"net/url"
	"slices"
	"strings"
	"time"
)

// Webhook endpoints are part of the ledger's state, so that what has been
// delivered survives a restart exactly like a balance does. An endpoint, its
// delivery position and its dead letters change only through logged
// commands: create_webhook and delete_webhook from the API,
// webhook_progress from the dispatcher after each delivery, and
// replay_webhook from an operator. Snapshots carry them, replay rebuilds
// them, and ledgerd verify compares them.
//
// The position (Cursor) is the last journal sequence the endpoint has dealt
// with: delivered, dead-lettered, or examined and not matched by its filter.
// The dispatcher sends the entries after it and logs the new position only
// once the receiver has answered 2xx, so a crash between the two sends those
// entries again: delivery is at least once, in sequence order per endpoint.
//
// Every change to an endpoint gives it a new Version, drawn from one counter
// for the whole ledger, so a version is never reused even when an endpoint
// is deleted and created again under the same ID. A progress command names
// the version it was computed from and is refused once anything else has
// changed the endpoint (a replay that moved the position back, a deletion),
// so a delivery that was in flight can never undo an operator's replay.

// Webhook limits.
const (
	MaxWebhooks           = 4096
	MaxWebhookURL         = 2048
	MinWebhookSecret      = 24
	MaxWebhookSecret      = 256
	DefaultWebhookBatch   = 100
	MaxWebhookBatch       = 1000
	MaxDeadLetters        = 1000
	MaxWebhookError       = 256
	maxReplayDeadLetterID = MaxDeadLetters
)

// Webhook is a registered endpoint and how far delivery to it has got.
// A committed Webhook is never modified; every change makes a new one.
type Webhook struct {
	ID  string `json:"id"`
	URL string `json:"url"`
	// Secret signs every delivery with HMAC-SHA256. It is shown when the
	// endpoint is read by its ID and left out of listings.
	Secret string `json:"secret,omitempty"`
	// Account, Kinds and Currency filter the entries delivered, as in the
	// journal feed.
	Account  string      `json:"account,omitempty"`
	Kinds    []EntryKind `json:"kinds,omitempty"`
	Currency string      `json:"currency,omitempty"`
	// Alerts, AlertMonitor and AlertAccount opt the endpoint in to balance
	// alerts, as in the journal feed: each delivered entry carries the
	// alerts it raised, so the position, dead letters and replays cover
	// alerts exactly as they cover entries.
	Alerts       AlertMode `json:"alerts,omitempty"`
	AlertMonitor string    `json:"alert_monitor,omitempty"`
	AlertAccount string    `json:"alert_account,omitempty"`
	MaxBatch     int       `json:"max_batch"`
	CreatedAt    time.Time `json:"created_at"`
	Version      uint64    `json:"version"`
	// Cursor is the last journal sequence the endpoint has dealt with.
	Cursor      uint64       `json:"delivered_through_seq"`
	DeadLetters []DeadLetter `json:"dead_letters"`
	// NextDeadLetter is the ID the next dead letter will get.
	NextDeadLetter uint64 `json:"-"`
}

// Filter returns the endpoint's entry filter.
func (h *Webhook) Filter() EntryFilter {
	return EntryFilter{Account: h.Account, Kinds: h.Kinds, Currency: h.Currency, Alerts: h.Alerts, AlertMonitor: h.AlertMonitor,
		AlertAccount: h.AlertAccount}
}

// DeadLetter is a delivery the dispatcher gave up on: the entries matching
// the endpoint's filter in (AfterSeq, ThroughSeq]. The journal never
// changes, so a dead letter can be replayed at any time.
type DeadLetter struct {
	ID         uint64    `json:"id"`
	AfterSeq   uint64    `json:"after_seq"`
	ThroughSeq uint64    `json:"through_seq"`
	Entries    int       `json:"entries"`
	Attempts   int       `json:"attempts"`
	Error      string    `json:"error"`
	FailedAt   time.Time `json:"failed_at"`
	// Replaying is set by replay_webhook and cleared when the redelivery
	// succeeds (the dead letter is removed) or fails again.
	Replaying bool `json:"replaying"`
}

// WebhookSpec registers an endpoint. Secret must be given; the API
// generates one when the client does not. AfterSeq is where delivery
// starts: the journal head when nil, so only new entries are sent, or 0 to
// send the whole journal.
type WebhookSpec struct {
	ID       string      `json:"id"`
	URL      string      `json:"url"`
	Secret   string      `json:"secret"`
	Account  string      `json:"account,omitempty"`
	Kinds    []EntryKind `json:"kinds,omitempty"`
	Currency string      `json:"currency,omitempty"`
	// Alerts, AlertMonitor and AlertAccount opt in to balance alerts.
	Alerts       AlertMode `json:"alerts,omitempty"`
	AlertMonitor string    `json:"alert_monitor,omitempty"`
	AlertAccount string    `json:"alert_account,omitempty"`
	MaxBatch     int       `json:"max_batch,omitempty"`
	AfterSeq     *uint64   `json:"after_seq,omitempty"`
}

// WebhookRef names an endpoint, to delete it.
type WebhookRef struct {
	ID string `json:"id"`
}

// WebhookProgressSpec records what a delivery attempt achieved. Version is
// the endpoint version the dispatcher read; the command is refused with
// failed_precondition if the endpoint has changed since. Exactly one of
// these is given:
//
//   - Through: the receiver accepted everything up to Through (or the filter
//     matched nothing up to it), which becomes the new position. With Dead
//     set, the delivery failed every attempt instead and becomes a dead
//     letter, and the position still moves on.
//   - Redelivered: a dead letter being replayed was accepted and is removed.
//   - Failed: a dead letter being replayed failed again and stops replaying.
type WebhookProgressSpec struct {
	ID          string            `json:"id"`
	Version     uint64            `json:"version"`
	Through     uint64            `json:"through,omitempty"`
	Dead        *DeadLetterSpec   `json:"dead,omitempty"`
	Redelivered uint64            `json:"redelivered,omitempty"`
	Failed      *RedeliveryFailed `json:"failed,omitempty"`
}

// DeadLetterSpec is a delivery given up on.
type DeadLetterSpec struct {
	AfterSeq   uint64 `json:"after_seq"`
	ThroughSeq uint64 `json:"through_seq"`
	Entries    int    `json:"entries"`
	Attempts   int    `json:"attempts"`
	Error      string `json:"error"`
}

// RedeliveryFailed is a dead letter whose replay failed again.
type RedeliveryFailed struct {
	DeadLetter uint64 `json:"dead_letter"`
	Attempts   int    `json:"attempts"`
	Error      string `json:"error"`
}

// WebhookReplaySpec sends entries to an endpoint again. Exactly one of
// these is given: AfterSeq moves the position back so everything after it is
// delivered again; DeadLetters or AllDeadLetters mark dead letters for
// redelivery, which the dispatcher does before it carries on from the
// position.
type WebhookReplaySpec struct {
	ID             string   `json:"id"`
	AfterSeq       *uint64  `json:"after_seq,omitempty"`
	DeadLetters    []uint64 `json:"dead_letters,omitempty"`
	AllDeadLetters bool     `json:"all_dead_letters,omitempty"`
}

// validWebhookURL accepts an absolute http or https URL with a host and no
// credentials or fragment.
func validWebhookURL(s string) bool {
	if s == "" || len(s) > MaxWebhookURL || !validText(s, MaxWebhookURL) || strings.ContainsAny(s, " \t") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	return true
}

func validSecret(s string) bool {
	if len(s) < MinWebhookSecret || len(s) > MaxWebhookSecret {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func (s *WebhookSpec) validate() *Error {
	if !ValidID(s.ID) {
		return errf(CodeInvalid, "webhook id must be 1 to %d characters of letters, digits and _ . : -", MaxIDLength)
	}
	if !validWebhookURL(s.URL) {
		return errf(CodeInvalid, "url must be an absolute http or https URL of at most %d characters, without credentials or a fragment", MaxWebhookURL)
	}
	if !validSecret(s.Secret) {
		return errf(CodeInvalid, "secret must be %d to %d printable ASCII characters without spaces", MinWebhookSecret, MaxWebhookSecret)
	}
	f := EntryFilter{Account: s.Account, Kinds: s.Kinds, Currency: s.Currency, Alerts: s.Alerts, AlertMonitor: s.AlertMonitor,
		AlertAccount: s.AlertAccount}
	if err := f.validate(); err != nil {
		return err
	}
	for i, k := range s.Kinds {
		if slices.Contains(s.Kinds[:i], k) {
			return errf(CodeInvalid, "kind %s is given twice", k)
		}
	}
	if s.MaxBatch == 0 {
		s.MaxBatch = DefaultWebhookBatch
	}
	if s.MaxBatch < 1 || s.MaxBatch > MaxWebhookBatch {
		return errf(CodeInvalid, "max_batch must be between 1 and %d", MaxWebhookBatch)
	}
	return nil
}

func (s *WebhookRef) validate() *Error {
	if !ValidID(s.ID) {
		return errf(CodeInvalid, "webhook id is not valid")
	}
	return nil
}

func validWebhookError(s string) bool { return validText(s, MaxWebhookError) }

func (s *WebhookProgressSpec) validate() *Error {
	if !ValidID(s.ID) {
		return errf(CodeInvalid, "webhook id is not valid")
	}
	if s.Version == 0 {
		return errf(CodeInvalid, "version must be the endpoint version the delivery was made from")
	}
	n := 0
	for _, set := range []bool{s.Through != 0, s.Redelivered != 0, s.Failed != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errf(CodeInvalid, "give exactly one of through, redelivered and failed")
	}
	if d := s.Dead; d != nil {
		if s.Through == 0 {
			return errf(CodeInvalid, "a dead letter needs through, the position after it")
		}
		if d.ThroughSeq <= d.AfterSeq || d.ThroughSeq > s.Through || d.Entries < 1 || d.Entries > MaxWebhookBatch || d.Attempts < 1 {
			return errf(CodeInvalid, "the dead letter must cover at least one entry up to through, after at least one attempt")
		}
		if !validWebhookError(d.Error) {
			return errf(CodeInvalid, "the dead letter's error must be at most %d characters of printable text", MaxWebhookError)
		}
	}
	if f := s.Failed; f != nil {
		if f.DeadLetter == 0 || f.Attempts < 1 || !validWebhookError(f.Error) {
			return errf(CodeInvalid, "failed must name a dead letter, its attempts and an error of at most %d characters", MaxWebhookError)
		}
	}
	return nil
}

func (s *WebhookReplaySpec) validate() *Error {
	if !ValidID(s.ID) {
		return errf(CodeInvalid, "webhook id is not valid")
	}
	n := 0
	for _, set := range []bool{s.AfterSeq != nil, len(s.DeadLetters) > 0, s.AllDeadLetters} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errf(CodeInvalid, "give exactly one of after_seq, dead_letters and all_dead_letters")
	}
	if len(s.DeadLetters) > maxReplayDeadLetterID {
		return errf(CodeInvalid, "at most %d dead letters", maxReplayDeadLetterID)
	}
	for i, id := range s.DeadLetters {
		if id == 0 || slices.Contains(s.DeadLetters[:i], id) {
			return errf(CodeInvalid, "dead_letters must be distinct dead letter ids")
		}
	}
	return nil
}

// hook returns the newest version of an endpoint for reading, or nil.
func (t *txn) hook(id string) *Webhook {
	if h, ok := t.hooks[id]; ok {
		return h // nil when the txn deleted it
	}
	return t.st.hooks[id]
}

// putHook records a new version of an endpoint; nil deletes it.
func (t *txn) putHook(id string, h *Webhook) {
	if t.hooks == nil {
		t.hooks = map[string]*Webhook{}
	}
	if h != nil {
		t.hookGen++
		h.Version = t.st.hookGen + t.hookGen
	}
	t.hooks[id] = h
}

func (t *txn) hookCount() int {
	n := len(t.st.hooks)
	for id, h := range t.hooks {
		_, had := t.st.hooks[id]
		switch {
		case h != nil && !had:
			n++
		case h == nil && had:
			n--
		}
	}
	return n
}

func (t *txn) createWebhook(s *WebhookSpec) (Outcome, *Error) {
	if t.hook(s.ID) != nil {
		return Outcome{}, errf(CodeAlreadyExists, "webhook %s already exists", s.ID)
	}
	if t.hookCount() >= MaxWebhooks {
		return Outcome{}, errf(CodeLimitExceeded, "at most %d webhook endpoints", MaxWebhooks)
	}
	head := t.nextSeq() - 1
	cursor := head
	if s.AfterSeq != nil {
		if *s.AfterSeq > head {
			return Outcome{}, errf(CodeInvalid, "after_seq %d is past the journal head %d", *s.AfterSeq, head)
		}
		cursor = *s.AfterSeq
	}
	h := &Webhook{ID: s.ID, URL: s.URL, Secret: s.Secret, Account: s.Account, Kinds: slices.Clone(s.Kinds), Currency: s.Currency,
		Alerts: s.Alerts, AlertMonitor: s.AlertMonitor, AlertAccount: s.AlertAccount, MaxBatch: s.MaxBatch, CreatedAt: t.now, Cursor: cursor, DeadLetters: []DeadLetter{}, NextDeadLetter: 1}
	t.putHook(s.ID, h)
	return Outcome{Webhook: h}, nil
}

func (t *txn) deleteWebhook(s *WebhookRef) (Outcome, *Error) {
	h := t.hook(s.ID)
	if h == nil {
		return Outcome{}, errf(CodeNotFound, "webhook %s does not exist", s.ID)
	}
	t.putHook(s.ID, nil)
	return Outcome{Webhook: h}, nil
}

// changed returns a copy of h to make a new version from. Dead letters are
// copied too, since the new version may change them.
func changedHook(h *Webhook) *Webhook {
	c := *h
	c.DeadLetters = slices.Clone(h.DeadLetters)
	if c.DeadLetters == nil {
		c.DeadLetters = []DeadLetter{}
	}
	return &c
}

func deadIndex(h *Webhook, id uint64) int {
	for i := range h.DeadLetters {
		if h.DeadLetters[i].ID == id {
			return i
		}
	}
	return -1
}

func (t *txn) webhookProgress(s *WebhookProgressSpec) (Outcome, *Error) {
	h := t.hook(s.ID)
	if h == nil {
		return Outcome{}, errf(CodeNotFound, "webhook %s does not exist", s.ID)
	}
	if h.Version != s.Version {
		return Outcome{}, errf(CodePrecondition, "webhook %s is at version %d, not %d", s.ID, h.Version, s.Version)
	}
	c := changedHook(h)
	switch {
	case s.Through != 0:
		head := t.nextSeq() - 1
		if s.Through <= h.Cursor || s.Through > head {
			return Outcome{}, errf(CodeInvalid, "through %d must be after the position %d and no later than the head %d", s.Through, h.Cursor, head)
		}
		if d := s.Dead; d != nil {
			if d.AfterSeq < h.Cursor {
				return Outcome{}, errf(CodeInvalid, "the dead letter starts before the position %d", h.Cursor)
			}
			if len(h.DeadLetters) >= MaxDeadLetters {
				return Outcome{}, errf(CodeLimitExceeded, "webhook %s already holds %d dead letters; replay or delete them", s.ID, MaxDeadLetters)
			}
			c.DeadLetters = append(c.DeadLetters, DeadLetter{ID: c.NextDeadLetter, AfterSeq: d.AfterSeq, ThroughSeq: d.ThroughSeq,
				Entries: d.Entries, Attempts: d.Attempts, Error: d.Error, FailedAt: t.now})
			c.NextDeadLetter++
		}
		c.Cursor = s.Through
	case s.Redelivered != 0:
		i := deadIndex(h, s.Redelivered)
		if i < 0 || !h.DeadLetters[i].Replaying {
			return Outcome{}, errf(CodePrecondition, "webhook %s has no dead letter %d being replayed", s.ID, s.Redelivered)
		}
		c.DeadLetters = slices.Delete(c.DeadLetters, i, i+1)
	default:
		f := s.Failed
		i := deadIndex(h, f.DeadLetter)
		if i < 0 || !h.DeadLetters[i].Replaying {
			return Outcome{}, errf(CodePrecondition, "webhook %s has no dead letter %d being replayed", s.ID, f.DeadLetter)
		}
		d := &c.DeadLetters[i]
		d.Replaying, d.Attempts, d.Error, d.FailedAt = false, d.Attempts+f.Attempts, f.Error, t.now
	}
	t.putHook(s.ID, c)
	return Outcome{Webhook: c}, nil
}

func (t *txn) replayWebhook(s *WebhookReplaySpec) (Outcome, *Error) {
	h := t.hook(s.ID)
	if h == nil {
		return Outcome{}, errf(CodeNotFound, "webhook %s does not exist", s.ID)
	}
	c := changedHook(h)
	switch {
	case s.AfterSeq != nil:
		if *s.AfterSeq > h.Cursor {
			return Outcome{}, errf(CodeInvalid, "after_seq %d is past the endpoint's position %d; a replay only goes back", *s.AfterSeq, h.Cursor)
		}
		c.Cursor = *s.AfterSeq
	case s.AllDeadLetters:
		for i := range c.DeadLetters {
			c.DeadLetters[i].Replaying = true
		}
	default:
		for _, id := range s.DeadLetters {
			i := deadIndex(c, id)
			if i < 0 {
				return Outcome{}, errf(CodeNotFound, "webhook %s has no dead letter %d", s.ID, id)
			}
			c.DeadLetters[i].Replaying = true
		}
	}
	t.putHook(s.ID, c)
	return Outcome{Webhook: c}, nil
}

// commitHooks makes the txn's endpoint changes the committed ones. It runs
// inside txn.commit, under the write lock.
func (t *txn) commitHooks() bool {
	if t.hooks == nil {
		return false
	}
	for id, h := range t.hooks {
		if h == nil {
			delete(t.st.hooks, id)
		} else {
			t.st.hooks[id] = h
		}
	}
	t.st.hookGen += t.hookGen
	return true
}

// Webhooks returns every registered endpoint, sorted by ID. The values are
// shared with the ledger and must not be modified.
func (l *Ledger) Webhooks() []*Webhook {
	l.mu.RLock()
	out := make([]*Webhook, 0, len(l.st.hooks))
	for _, h := range l.st.hooks {
		out = append(out, h)
	}
	l.mu.RUnlock()
	slices.SortFunc(out, func(a, b *Webhook) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Webhook returns one endpoint. The value is shared with the ledger and
// must not be modified.
func (l *Ledger) Webhook(id string) (*Webhook, error) {
	l.mu.RLock()
	h := l.st.hooks[id]
	l.mu.RUnlock()
	if h == nil {
		return nil, errf(CodeNotFound, "webhook %s does not exist", id)
	}
	return h, nil
}

// WebhooksChanged returns a channel that is closed after the next commit
// that changes any webhook endpoint. Take it before reading the endpoints,
// so a change made in between still closes it.
func (l *Ledger) WebhooksChanged() <-chan struct{} {
	l.hookMu.Lock()
	defer l.hookMu.Unlock()
	if l.hookCh == nil {
		l.hookCh = make(chan struct{})
	}
	return l.hookCh
}

func (l *Ledger) wakeWebhookWaiters() {
	l.hookMu.Lock()
	if l.hookCh != nil {
		close(l.hookCh)
		l.hookCh = nil
	}
	l.hookMu.Unlock()
}

// hookProblems checks the endpoints against a journal head: positions and
// dead letters within the journal, versions within the counter, dead letter
// IDs increasing and below the next one.
func hookProblems(hooks map[string]*Webhook, gen, head uint64, bad func(string, ...any)) {
	ids := make([]string, 0, len(hooks))
	for id := range hooks {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if len(ids) > MaxWebhooks {
		bad("%d webhook endpoints, more than %d", len(ids), MaxWebhooks)
	}
	for _, id := range ids {
		h := hooks[id]
		switch {
		case h.ID != id:
			bad("webhook %s is filed under %s", h.ID, id)
		case h.Cursor > head:
			bad("webhook %s is at position %d, past the head %d", id, h.Cursor, head)
		case h.Version == 0 || h.Version > gen:
			bad("webhook %s has version %d, outside 1 to %d", id, h.Version, gen)
		case len(h.DeadLetters) > MaxDeadLetters:
			bad("webhook %s holds %d dead letters", id, len(h.DeadLetters))
		}
		var last uint64
		for _, d := range h.DeadLetters {
			if d.ID <= last || d.ID >= h.NextDeadLetter || d.ThroughSeq <= d.AfterSeq || d.ThroughSeq > head {
				bad("webhook %s has a malformed dead letter %d", id, d.ID)
				break
			}
			last = d.ID
		}
	}
}
