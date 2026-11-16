package api

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/laiwf94/ledgerd/internal/export"
	"github.com/laiwf94/ledgerd/internal/ledger"
)

// The journal export streams every entry of a period as NDJSON or CSV,
// ending in a manifest signed with the server's Ed25519 export key (see
// package export). It reads a ledger.Journal, so it holds no lock while it
// runs and never delays a commit, and it encodes one entry at a time into a
// fixed buffer, so its memory does not grow with the period.
//
// The server's write timeout covers a whole response, which a large export
// would exceed, so every write of the buffer gets its own deadline instead,
// like the live stream: a client that stops reading is disconnected and
// its file has no manifest.

const (
	// MaxConcurrentExports bounds the CPU exports can use: each one walks
	// the whole journal.
	MaxConcurrentExports = 4
	exportBufferBytes    = 64 << 10
	exportHeadHeader     = "Ledgerd-Export-Head-Seq"
)

// deadlineWriter gives every write its own deadline.
type deadlineWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

func (d deadlineWriter) Write(b []byte) (int, error) {
	// A writer without deadlines (a test recorder, some middleware) still
	// gets the export; only the slow-client cut-off is lost.
	if err := d.rc.SetWriteDeadline(time.Now().Add(d.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, err
	}
	return d.w.Write(b)
}

// parsePeriodEnd reads from or to: a date (midnight UTC) or an RFC 3339 time.
func parsePeriodEnd(name, v string) (time.Time, error) {
	if len(v) > maxQueryValueBytes {
		return time.Time{}, badRequest("%s is too long", name)
	}
	if t, err := time.Parse(dateLayout, v); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, badRequest("%s must be a date YYYY-MM-DD or an RFC 3339 time", name)
	}
	return t.UTC(), nil
}

type exportQuery struct {
	format   export.Format
	from, to time.Time
}

func parseExportQuery(r *http.Request) (exportQuery, error) {
	q := exportQuery{format: export.NDJSON}
	vals, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return q, badRequest("invalid query string: %v", err)
	}
	for name, vs := range vals {
		if len(vs) != 1 {
			return q, badRequest("%s may be given only once", name)
		}
		v := vs[0]
		if v == "" {
			return q, badRequest("%s must not be empty", name)
		}
		switch name {
		case "format":
			if q.format, err = export.ParseFormat(v); err != nil {
				return q, badRequest("%v", err)
			}
		case "from":
			if q.from, err = parsePeriodEnd(name, v); err != nil {
				return q, err
			}
		case "to":
			if q.to, err = parsePeriodEnd(name, v); err != nil {
				return q, err
			}
		default:
			return q, badRequest("unknown query parameter %q", name)
		}
	}
	if !q.from.IsZero() && !q.to.IsZero() && !q.from.Before(q.to) {
		return q, badRequest("from must be before to")
	}
	return q, nil
}

// exportJournal serves GET /v1/export.
func (s *Server) exportJournal(w http.ResponseWriter, r *http.Request) error {
	q, err := parseExportQuery(r)
	if err != nil {
		return err
	}
	select {
	case s.exports <- struct{}{}:
		defer func() { <-s.exports }()
	default:
		w.Header().Set("Retry-After", "1")
		return &apiError{http.StatusTooManyRequests, "resource_exhausted", fmt.Sprintf("at most %d exports run at once; retry shortly", MaxConcurrentExports)}
	}
	j := s.l.Journal()
	h := w.Header()
	h.Set("Content-Type", q.format.ContentType())
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ledgerd-export-%d.%s"`, j.Head(), q.format))
	h.Set(exportHeadHeader, strconv.FormatUint(j.Head(), 10))
	w.WriteHeader(http.StatusOK)

	bw := bufio.NewWriterSize(deadlineWriter{w: w, rc: http.NewResponseController(w), timeout: s.streams.timeout}, exportBufferBytes)
	x, err := export.NewWriter(bw, q.format, q.from, q.to, j.Head())
	var m export.Manifest
	if err == nil {
		ctx := r.Context()
		err = j.Each(q.from, q.to, func(e *ledger.Entry) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return x.Write(e)
		})
	}
	if err == nil {
		m, err = x.Close(s.signer, time.Now())
	}
	if err == nil {
		err = bw.Flush()
	}
	if err != nil {
		// The status is sent; the client's file has no manifest, which is
		// how it learns the export failed.
		s.mExportFailed.Inc()
		s.log.Warn("export ended early", "request_id", r.Context().Value(requestIDKey), "error", err.Error())
		return nil
	}
	s.mExports.Inc()
	s.mExportEntries.Add(m.Entries)
	s.mExportBytes.Add(m.Bytes)
	return nil
}

// exportKey serves GET /v1/export/key: the public key that verifies export
// manifests.
func (s *Server) exportKey(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, map[string]string{"algorithm": "ed25519", "key_id": s.signer.KeyID(),
		"public_key": export.EncodeKey(s.signer.PublicKey())})
	return nil
}
