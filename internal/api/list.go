package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

// listQuery reads a listing's query string. Every parameter may appear at
// most once and must be one the listing knows, so a misspelt filter is an
// error instead of a listing of everything. Filters are the parameters other
// than limit and cursor.
type listQuery struct {
	filters map[string]string // the filter parameters as given
	limit   int
	cursor  string
}

func parseListQuery(r *http.Request, known func(name string) bool) (listQuery, error) {
	q := listQuery{filters: map[string]string{}, limit: defaultLimit}
	vals, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return q, badRequest("invalid query string: %v", err)
	}
	for name, vs := range vals {
		if len(vs) != 1 {
			return q, badRequest("%s may be given only once", name)
		}
		v := vs[0]
		switch name {
		case "limit":
			n, err := queryUint(r, "limit", defaultLimit, ledger.MaxListLimit)
			if err != nil {
				return q, err
			}
			if n == 0 {
				return q, badRequest("limit must be between 1 and %d", ledger.MaxListLimit)
			}
			q.limit = int(n)
		case "cursor":
			if v == "" {
				return q, badRequest("cursor must not be empty")
			}
			q.cursor = v
		default:
			if !known(name) {
				return q, badRequest("unknown query parameter %q", name)
			}
			q.filters[name] = v
		}
	}
	return q, nil
}

// metadataParam returns the key of a metadata[key] parameter.
func metadataParam(name string) (string, bool) {
	k, ok := strings.CutPrefix(name, "metadata[")
	if !ok || !strings.HasSuffix(k, "]") {
		return "", false
	}
	return k[:len(k)-1], true
}

// position decodes the listing position from q's cursor, or returns "" for
// the first page.
func (s *Server) position(kind cursorKind, q listQuery) (string, error) {
	if q.cursor == "" {
		return "", nil
	}
	pos, err := s.cursors.decode(kind, canonicalFilter(q.filters), q.cursor)
	if err != nil {
		return "", badRequest("cursor is invalid, or was issued for a different listing or different filters")
	}
	return string(pos), nil
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) error {
	q, err := parseListQuery(r, func(name string) bool {
		_, meta := metadataParam(name)
		return name == "currency" || meta
	})
	if err != nil {
		return err
	}
	var f ledger.AccountFilter
	for name, v := range q.filters {
		if k, ok := metadataParam(name); ok {
			if f.Metadata == nil {
				f.Metadata = map[string]string{}
			}
			f.Metadata[k] = v
			continue
		}
		if v == "" {
			return badRequest("%s must not be empty", name)
		}
		f.Currency = v
	}
	after, err := s.position(cursorAccounts, q)
	if err != nil {
		return err
	}
	page, err := s.l.ListAccounts(f, after, q.limit)
	if err != nil {
		return err
	}
	resp := struct {
		Accounts   []ledger.AccountState `json:"accounts"`
		HasMore    bool                  `json:"has_more"`
		NextCursor string                `json:"next_cursor,omitempty"`
		AsOfSeq    uint64                `json:"as_of_seq"`
	}{Accounts: page.Accounts, AsOfSeq: page.AsOfSeq}
	if resp.Accounts == nil {
		resp.Accounts = []ledger.AccountState{}
	}
	if page.Next != "" {
		resp.HasMore = true
		resp.NextCursor = s.cursors.encode(cursorAccounts, canonicalFilter(q.filters), []byte(page.Next))
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}
