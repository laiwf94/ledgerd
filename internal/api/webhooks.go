package api

import (
	"net/http"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
	"github.com/laiwf94/ledgerd/internal/webhook"
)

// webhookBody is what POST /v1/webhooks takes. Secret is optional; one is
// generated when it is left out, and returned in the response.
type webhookBody struct {
	ID       string             `json:"id"`
	URL      string             `json:"url"`
	Secret   string             `json:"secret,omitempty"`
	Account  string             `json:"account,omitempty"`
	Kinds    []ledger.EntryKind `json:"kinds,omitempty"`
	Currency string             `json:"currency,omitempty"`
	MaxBatch int                `json:"max_batch,omitempty"`
	AfterSeq *uint64            `json:"after_seq,omitempty"`
}

// webhookSummary is an endpoint in a listing: no secret, and the number of
// dead letters rather than the letters.
type webhookSummary struct {
	ID              string             `json:"id"`
	URL             string             `json:"url"`
	Account         string             `json:"account,omitempty"`
	Kinds           []ledger.EntryKind `json:"kinds,omitempty"`
	Currency        string             `json:"currency,omitempty"`
	MaxBatch        int                `json:"max_batch"`
	CreatedAt       time.Time          `json:"created_at"`
	Version         uint64             `json:"version"`
	Cursor          uint64             `json:"delivered_through_seq"`
	DeadLetterCount int                `json:"dead_letter_count"`
}

// webhookDetail is one endpoint read by its ID, with the journal head so a
// client can see how far behind it is.
type webhookDetail struct {
	*ledger.Webhook
	HeadSeq uint64 `json:"head_seq"`
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) error {
	var body webhookBody
	if err := decode(r, &body, false); err != nil {
		return err
	}
	spec := ledger.WebhookSpec{ID: body.ID, URL: body.URL, Secret: body.Secret, Account: body.Account, Kinds: body.Kinds,
		Currency: body.Currency, MaxBatch: body.MaxBatch, AfterSeq: body.AfterSeq}
	if spec.Secret == "" {
		spec.Secret = webhook.NewSecret()
	}
	return s.executeWebhook(w, r, ledger.Command{CreateWebhook: &spec}, body, http.StatusCreated)
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) error {
	if len(r.URL.Query()) > 0 {
		return badRequest("the webhook listing takes no parameters")
	}
	hooks := s.l.Webhooks()
	out := make([]webhookSummary, len(hooks))
	for i, h := range hooks {
		out[i] = webhookSummary{ID: h.ID, URL: h.URL, Account: h.Account, Kinds: h.Kinds, Currency: h.Currency, MaxBatch: h.MaxBatch,
			CreatedAt: h.CreatedAt, Version: h.Version, Cursor: h.Cursor, DeadLetterCount: len(h.DeadLetters)}
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out, "head_seq": s.l.Head()})
	return nil
}

func (s *Server) getWebhook(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	h, err := s.l.Webhook(id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, webhookDetail{Webhook: h, HeadSeq: s.l.Head()})
	return nil
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	out, err := s.do(w, r, ledger.Command{DeleteWebhook: &ledger.WebhookRef{ID: id}}, struct{}{})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": out.Webhook.ID, "deleted": true})
	return nil
}

// replayWebhook sends entries to an endpoint again: everything after
// after_seq, or the dead letters named (or all of them).
func (s *Server) replayWebhook(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var body struct {
		AfterSeq       *uint64  `json:"after_seq,omitempty"`
		DeadLetters    []uint64 `json:"dead_letters,omitempty"`
		AllDeadLetters bool     `json:"all_dead_letters,omitempty"`
	}
	if err := decode(r, &body, false); err != nil {
		return err
	}
	cmd := ledger.Command{ReplayWebhook: &ledger.WebhookReplaySpec{ID: id, AfterSeq: body.AfterSeq, DeadLetters: body.DeadLetters,
		AllDeadLetters: body.AllDeadLetters}}
	return s.executeWebhook(w, r, cmd, body, http.StatusOK)
}

func (s *Server) executeWebhook(w http.ResponseWriter, r *http.Request, cmd ledger.Command, params any, status int) error {
	out, err := s.do(w, r, cmd, params)
	if err != nil {
		return err
	}
	writeJSON(w, status, webhookDetail{Webhook: out.Webhook, HeadSeq: s.l.Head()})
	return nil
}
