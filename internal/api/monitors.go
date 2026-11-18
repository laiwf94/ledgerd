package api

import (
	"net/http"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

// monitorBody is what POST /v1/balance-monitors takes.
type monitorBody struct {
	ID          string           `json:"id"`
	Account     string           `json:"account"`
	Condition   ledger.Condition `json:"condition"`
	Description string           `json:"description,omitempty"`
}

func (s *Server) createMonitor(w http.ResponseWriter, r *http.Request) error {
	var body monitorBody
	if err := decode(r, &body, false); err != nil {
		return err
	}
	spec := ledger.MonitorSpec{ID: body.ID, Account: body.Account, Condition: body.Condition, Description: body.Description}
	out, err := s.do(w, r, ledger.Command{CreateMonitor: &spec}, body)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, out.Monitor)
	return nil
}

func (s *Server) listMonitors(w http.ResponseWriter, r *http.Request) error {
	q, err := parseListQuery(r, func(name string) bool { return name == "account" })
	if err != nil {
		return err
	}
	if q.cursor != "" || r.URL.Query().Has("limit") {
		return badRequest("the monitor listing returns every monitor and takes no cursor or limit")
	}
	acct, filtered := q.filters["account"]
	if filtered && !ledger.ValidID(acct) {
		return badRequest("%q is not a valid account id", acct)
	}
	writeJSON(w, http.StatusOK, map[string]any{"monitors": s.l.Monitors(acct), "alert_head_seq": s.l.AlertHead()})
	return nil
}

func (s *Server) getMonitor(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	m, err := s.l.Monitor(id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, m)
	return nil
}

func (s *Server) deleteMonitor(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	out, err := s.do(w, r, ledger.Command{DeleteMonitor: &ledger.MonitorRef{ID: id}}, struct{}{})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": out.Monitor.ID, "deleted": true})
	return nil
}

// listAlerts is the balance alert feed: GET /v1/balance-alerts?after_seq=
// with optional monitor and account filters. It pages like the journal
// feed: a consumer keeps next_after_seq and asks for what follows.
func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) error {
	q, err := parseListQuery(r, func(name string) bool { return name == "after_seq" || name == "monitor" || name == "account" })
	if err != nil {
		return err
	}
	if q.cursor != "" {
		return badRequest("the alert feed resumes from after_seq, not a cursor")
	}
	var f ledger.AlertFilter
	var after uint64
	for name, v := range q.filters {
		if v == "" {
			return badRequest("%s must not be empty", name)
		}
		switch name {
		case "after_seq":
			if after, err = queryUint(r, "after_seq", 0, 1<<63); err != nil {
				return err
			}
		case "monitor":
			f.Monitor = v
		case "account":
			f.Account = v
		}
	}
	page, err := s.l.Alerts(f, after, q.limit)
	if err != nil {
		return err
	}
	resp := struct {
		Alerts       []ledger.BalanceAlert `json:"alerts"`
		HasMore      bool                  `json:"has_more"`
		NextAfterSeq uint64                `json:"next_after_seq"`
		HeadSeq      uint64                `json:"head_seq"`
	}{Alerts: page.Alerts, HasMore: page.HasMore(), NextAfterSeq: page.Next, HeadSeq: page.Head}
	if resp.Alerts == nil {
		resp.Alerts = []ledger.BalanceAlert{}
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}
