// Package api serves the ledger over HTTP with JSON bodies.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
	"github.com/laiwf94/ledgerd/internal/metrics"
	"github.com/laiwf94/ledgerd/internal/recon"
)

// Request limits.
const (
	MaxBodyBytes       = 1 << 20
	DefaultTimeout     = 5 * time.Second
	DefaultWindowDays  = 3
	MaxReconcileDays   = 366
	defaultLimit       = 100
	requestIDHeader    = "X-Request-Id"
	idempotencyHeader  = "Idempotency-Key"
	replayedHeader     = "Idempotent-Replayed"
	minSecretLength    = 16
	dateLayout         = "2006-01-02"
	maxQueryValueBytes = 64
	// MaxConcurrentReconciles bounds the memory reconciliation can use: each
	// one holds a statement of up to 4 MiB and up to 100,000 movements.
	MaxConcurrentReconciles = 4
)

// Config configures a Server.
type Config struct {
	Ledger  *ledger.Ledger
	APIKeys []string // bearer tokens accepted by /v1; must not be empty
	// AdminToken guards /v1/admin. When empty, admin endpoints answer only
	// requests from a loopback address that carry a valid API key.
	AdminToken string
	Logger     *slog.Logger
	Metrics    *metrics.Registry
	Timeout    time.Duration
}

// Server is the HTTP API.
type Server struct {
	l       *ledger.Ledger
	keys    [][sha256.Size]byte
	admin   *[sha256.Size]byte
	log     *slog.Logger
	m       *metrics.Registry
	timeout time.Duration
	mux     *http.ServeMux
	recon   chan struct{}
}

// New builds a server. It returns an error for unsafe configuration.
func New(cfg Config) (*Server, error) {
	if cfg.Ledger == nil {
		return nil, errors.New("api: a ledger is required")
	}
	if len(cfg.APIKeys) == 0 {
		return nil, errors.New("api: at least one API key is required")
	}
	s := &Server{l: cfg.Ledger, log: cfg.Logger, m: cfg.Metrics, timeout: cfg.Timeout, recon: make(chan struct{}, MaxConcurrentReconciles)}
	for _, k := range cfg.APIKeys {
		if len(k) < minSecretLength {
			return nil, fmt.Errorf("api: API keys must be at least %d characters", minSecretLength)
		}
		s.keys = append(s.keys, sha256.Sum256([]byte(k)))
	}
	if cfg.AdminToken != "" {
		if len(cfg.AdminToken) < minSecretLength {
			return nil, fmt.Errorf("api: the admin token must be at least %d characters", minSecretLength)
		}
		sum := sha256.Sum256([]byte(cfg.AdminToken))
		for _, k := range s.keys {
			if subtle.ConstantTimeCompare(sum[:], k[:]) == 1 {
				return nil, errors.New("api: the admin token must differ from every API key")
			}
		}
		s.admin = &sum
	}
	if s.log == nil {
		s.log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if s.m == nil {
		s.m = metrics.New()
	}
	if s.timeout <= 0 {
		s.timeout = DefaultTimeout
	}

	s.mux = http.NewServeMux()
	s.route("GET /healthz", s.healthz, authNone)
	s.route("GET /readyz", s.readyz, authNone)
	s.route("GET /metrics", s.metricsHandler, authKey)
	s.route("POST /v1/accounts", s.createAccount, authKey)
	s.route("GET /v1/accounts/{id}", s.getAccount, authKey)
	s.route("GET /v1/accounts/{id}/statement", s.statement, authKey)
	s.route("POST /v1/accounts/{id}/reconcile", s.reconcile, authKey, recon.MaxBytes)
	s.route("POST /v1/transfers", s.createTransfer, authKey)
	s.route("GET /v1/transfers/{id}", s.getTransfer, authKey)
	s.route("POST /v1/transfers/{id}/post", s.postTransfer, authKey)
	s.route("POST /v1/transfers/{id}/void", s.voidTransfer, authKey)
	s.route("POST /v1/transfers/{id}/reverse", s.reverseTransfer, authKey)
	s.route("GET /v1/admin/invariants", s.invariants, authAdmin)
	s.route("/", func(http.ResponseWriter, *http.Request) error {
		return &apiError{http.StatusNotFound, "not_found", "no such endpoint"}
	}, authNone)
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

type handler func(w http.ResponseWriter, r *http.Request) error

type authMode int

const (
	authNone authMode = iota
	authKey
	authAdmin
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	scopeKey
)

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func requestID(r *http.Request) string {
	if id := r.Header.Get(requestIDHeader); requestIDRe.MatchString(id) {
		return id
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// statusWriter remembers the response code for logs and metrics.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// route registers a handler. POST bodies are capped at bodyLimit bytes
// (MaxBodyBytes when zero) before anything reads them.
func (s *Server) route(pattern string, h handler, mode authMode, bodyLimit ...int64) {
	limit := int64(MaxBodyBytes)
	if len(bodyLimit) > 0 {
		limit = bodyLimit[0]
	}
	path := pattern[strings.IndexByte(pattern, ' ')+1:]
	latency := s.m.Histogram("ledgerd_http_request_duration_seconds", `route="`+path+`"`, "HTTP request latency.")
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := requestID(r)
		sw := &statusWriter{ResponseWriter: w}
		sw.Header().Set(requestIDHeader, id)
		sw.Header().Set("Cache-Control", "no-store")
		sw.Header().Set("X-Content-Type-Options", "nosniff")
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		r = r.WithContext(ctx)
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic", "request_id", id, "route", path, "panic", fmt.Sprint(p))
				if sw.code == 0 {
					writeError(sw, r, http.StatusInternalServerError, "internal", "internal error")
				}
			}
			code := sw.code
			if code == 0 {
				code = http.StatusOK
			}
			latency.Observe(time.Since(start))
			s.m.Counter("ledgerd_http_requests_total", fmt.Sprintf(`route=%q,code="%d"`, path, code), "HTTP requests.").Inc()
			lvl := slog.LevelInfo
			if code >= 500 {
				lvl = slog.LevelError
			}
			s.log.Log(ctx, lvl, "request", "request_id", id, "method", r.Method, "route", path,
				"status", code, "replayed", sw.Header().Get(replayedHeader) == "true",
				"duration_ms", float64(time.Since(start).Microseconds())/1000)
		}()
		switch mode {
		case authKey:
			scope, ok := s.authorized(r)
			if !ok {
				sw.Header().Set("WWW-Authenticate", `Bearer realm="ledgerd"`)
				writeError(sw, r, http.StatusUnauthorized, "unauthenticated", "missing or invalid API key")
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), scopeKey, scope))
		case authAdmin:
			if err := s.adminAllowed(r); err != nil {
				s.fail(sw, r, err)
				return
			}
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(sw, r.Body, limit)
		}
		if err := h(sw, r); err != nil {
			s.fail(sw, r, err)
		}
	})
}

// authorized compares the bearer token against every key in constant time
// and returns the caller's idempotency scope: a fingerprint of its key, so
// two clients can never see each other's idempotent results.
func (s *Server) authorized(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(tok))
	match := 0
	for _, k := range s.keys {
		match |= subtle.ConstantTimeCompare(sum[:], k[:])
	}
	return hex.EncodeToString(sum[:8]), match == 1
}

func (s *Server) adminAllowed(r *http.Request) error {
	if s.admin != nil {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(tok))
		if subtle.ConstantTimeCompare(sum[:], s.admin[:]) != 1 {
			return &apiError{http.StatusUnauthorized, "unauthenticated", "missing or invalid admin token"}
		}
		return nil
	}
	if _, ok := s.authorized(r); !ok {
		return &apiError{http.StatusUnauthorized, "unauthenticated", "missing or invalid API key"}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return &apiError{http.StatusForbidden, "permission_denied", "admin endpoints are served only on loopback unless LEDGERD_ADMIN_TOKEN is set"}
	}
	return nil
}

// apiError carries an HTTP status and stable code.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &apiError{http.StatusBadRequest, "invalid_argument", fmt.Sprintf(format, args...)}
}

// statusOf maps ledger error codes to HTTP statuses. A replayed error gets
// the same status as the original, because the mapping is fixed.
func statusOf(c ledger.Code) int {
	switch c {
	case ledger.CodeInvalid:
		return http.StatusBadRequest
	case ledger.CodeNotFound:
		return http.StatusNotFound
	case ledger.CodeAlreadyExists, ledger.CodeInvalidState, ledger.CodeExpired, ledger.CodeInProgress:
		return http.StatusConflict
	case ledger.CodePrecondition:
		return http.StatusPreconditionFailed
	case ledger.CodeCurrencyMismatch, ledger.CodeLimitExceeded, ledger.CodeOverflow, ledger.CodeKeyReused:
		return http.StatusUnprocessableEntity
	case ledger.CodeUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	var le *ledger.Error
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &ae):
		writeError(w, r, ae.status, ae.code, ae.msg)
	case errors.As(err, &mbe):
		writeError(w, r, http.StatusRequestEntityTooLarge, "invalid_argument", fmt.Sprintf("request body is larger than %d bytes", mbe.Limit))
	case errors.As(err, &le):
		status := statusOf(le.Code)
		if status >= 500 {
			s.log.Error("ledger error", "request_id", r.Context().Value(requestIDKey), "code", string(le.Code), "error", le.Message)
		}
		writeError(w, r, status, string(le.Code), le.Message)
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, r, http.StatusGatewayTimeout, "deadline_exceeded", "the request took too long; retry with the same Idempotency-Key to learn its outcome")
	case errors.Is(err, context.Canceled):
		writeError(w, r, 499, "canceled", "the client went away")
	default:
		s.log.Error("internal error", "request_id", r.Context().Value(requestIDKey), "error", err.Error())
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error")
	}
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	id, _ := r.Context().Value(requestIDKey).(string)
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg, "request_id": id}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func requireType(r *http.Request, want string) error {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if !strings.EqualFold(strings.TrimSpace(ct), want) {
		return &apiError{http.StatusUnsupportedMediaType, "invalid_argument", "Content-Type must be " + want}
	}
	return nil
}

// decode reads a strict JSON body: one object, no unknown fields, nothing
// after it. With optional set, an empty body counts as {}.
func decode(r *http.Request, v any, optional bool) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return badRequest("reading the request body: %v", err)
	}
	if optional && len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := requireType(r, "application/json"); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badRequest("request body must be a single JSON object")
	}
	return nil
}

// idem builds the idempotency identity for a request: the caller's scope,
// the key, and a digest of the operation and its decoded parameters, so
// whitespace or field order don't matter but any real difference does.
func idem(r *http.Request, params any) (*ledger.Idem, error) {
	k := r.Header.Get(idempotencyHeader)
	if k == "" {
		return nil, nil
	}
	canon, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s %s\n", r.Method, r.URL.Path)
	h.Write(canon)
	i := &ledger.Idem{Key: k}
	i.Scope, _ = r.Context().Value(scopeKey).(string)
	copy(i.Hash[:], h.Sum(nil))
	return i, nil
}

// execute runs a command and writes its result, marking replays.
func (s *Server) execute(w http.ResponseWriter, r *http.Request, cmd ledger.Command, params any, status int) error {
	id, err := idem(r, params)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	out, err := s.l.Do(ctx, cmd, id)
	if out.Replayed {
		w.Header().Set(replayedHeader, "true")
		s.m.Counter("ledgerd_idempotent_replays_total", "", "Requests answered from the idempotency store.").Inc()
	}
	if err != nil {
		return err
	}
	switch {
	case out.Transfer != nil:
		writeJSON(w, status, out.Transfer)
	default:
		writeJSON(w, status, out.Account)
	}
	return nil
}

func pathID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !ledger.ValidID(id) {
		return "", badRequest("%q is not a valid id", id)
	}
	return id, nil
}

func queryUint(r *http.Request, name string, def, max uint64) (uint64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || len(v) > maxQueryValueBytes || n > max {
		return 0, badRequest("%s must be a whole number up to %d", name, max)
	}
	return n, nil
}

func queryDate(r *http.Request, name string) (time.Time, bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}, false, nil
	}
	d, err := time.Parse(dateLayout, v)
	if err != nil {
		return time.Time{}, false, badRequest("%s must be a date YYYY-MM-DD", name)
	}
	return d, true, nil
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	return nil
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) error {
	if s.l.Err() != nil {
		return &apiError{http.StatusServiceUnavailable, "unavailable", "the ledger stopped accepting writes after a storage error; restart to recover"}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "head": s.l.Head()})
	return nil
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	return s.m.Write(w)
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) error {
	var body ledger.AccountSpec
	if err := decode(r, &body, false); err != nil {
		return err
	}
	return s.execute(w, r, ledger.Command{CreateAccount: &body}, body, http.StatusCreated)
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	asOf, err := queryUint(r, "as_of_seq", 0, 1<<63)
	if err != nil {
		return err
	}
	a, err := s.l.Account(id, asOf)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, a)
	return nil
}

func (s *Server) statement(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	after, err := queryUint(r, "after_seq", 0, 1<<63)
	if err != nil {
		return err
	}
	limit, err := queryUint(r, "limit", defaultLimit, ledger.MaxStatementLimit)
	if err != nil {
		return err
	}
	if limit == 0 {
		return badRequest("limit must be between 1 and %d", ledger.MaxStatementLimit)
	}
	lines, next, err := s.l.Statement(id, after, int(limit))
	if err != nil {
		return err
	}
	resp := map[string]any{"account": id, "lines": lines, "has_more": next != 0}
	if next != 0 {
		resp["next_after_seq"] = next
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) error {
	var body ledger.TransferSpec
	if err := decode(r, &body, false); err != nil {
		return err
	}
	return s.execute(w, r, ledger.Command{CreateTransfer: &body}, body, http.StatusCreated)
}

func (s *Server) getTransfer(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	tr, err := s.l.Transfer(id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, tr)
	return nil
}

func (s *Server) postTransfer(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var body ledger.Amounts
	if err := decode(r, &body, true); err != nil {
		return err
	}
	return s.execute(w, r, ledger.Command{Post: &ledger.PostSpec{TransferID: id, Amounts: body}}, body, http.StatusOK)
}

func (s *Server) voidTransfer(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var body struct{}
	if err := decode(r, &body, true); err != nil {
		return err
	}
	return s.execute(w, r, ledger.Command{Void: &ledger.VoidSpec{TransferID: id}}, body, http.StatusOK)
}

func (s *Server) reverseTransfer(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var body struct {
		ledger.Amounts
		Reference string            `json:"reference,omitempty"`
		Metadata  map[string]string `json:"metadata,omitempty"`
	}
	if err := decode(r, &body, true); err != nil {
		return err
	}
	cmd := ledger.Command{Reverse: &ledger.ReverseSpec{TransferID: id, Amounts: body.Amounts, Reference: body.Reference, Metadata: body.Metadata}}
	return s.execute(w, r, cmd, body, http.StatusCreated)
}

// reconcile matches a CSV statement against the account's posted movements.
// It reads only; nothing is stored.
func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if err := requireType(r, "text/csv"); err != nil {
		return err
	}
	window, err := queryUint(r, "window_days", DefaultWindowDays, recon.MaxWindowDays)
	if err != nil {
		return err
	}
	from, hasFrom, err := queryDate(r, "from")
	if err != nil {
		return err
	}
	to, hasTo, err := queryDate(r, "to")
	if err != nil {
		return err
	}
	acct, err := s.l.Account(id, 0)
	if err != nil {
		return err
	}
	select {
	case s.recon <- struct{}{}:
		defer func() { <-s.recon }()
	default:
		w.Header().Set("Retry-After", "1")
		return &apiError{http.StatusTooManyRequests, "resource_exhausted", fmt.Sprintf("at most %d reconciliations run at once; retry shortly", MaxConcurrentReconciles)}
	}
	lines, err := recon.Parse(r.Body)
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe), errors.Is(err, recon.ErrTooLarge):
		return &apiError{http.StatusRequestEntityTooLarge, "invalid_argument", recon.ErrTooLarge.Error()}
	case err != nil:
		return badRequest("statement: %v", err)
	}
	for _, ln := range lines {
		if ln.Currency != acct.Currency {
			return &apiError{http.StatusUnprocessableEntity, string(ledger.CodeCurrencyMismatch),
				fmt.Sprintf("statement row %d is in %s but account %s holds %s", ln.Row, ln.Currency, id, acct.Currency)}
		}
	}
	lo, hi, ok := recon.Period(lines)
	if hasFrom {
		lo = from
	}
	if hasTo {
		hi = to
	}
	if !ok && !(hasFrom && hasTo) {
		return badRequest("the statement has no lines; give from and to to list unmatched ledger movements")
	}
	if hi.Before(lo) || hi.Sub(lo) > MaxReconcileDays*24*time.Hour {
		return badRequest("the period must run forwards and cover at most %d days", MaxReconcileDays)
	}
	pad := time.Duration(window) * 24 * time.Hour
	_, movements, err := s.l.Movements(id, lo.Add(-pad), hi.Add(24*time.Hour+pad))
	if err != nil {
		return err
	}
	postings := make([]recon.Posting, len(movements))
	for i, m := range movements {
		postings[i] = recon.Posting{Seq: m.Seq, Time: m.Time, TransferID: m.TransferID, Reference: m.Reference, Amount: m.Amount}
	}
	rep := recon.Reconcile(lines, postings, lo, hi, int(window))
	writeJSON(w, http.StatusOK, struct {
		Account  string `json:"account"`
		Currency string `json:"currency"`
		recon.Report
	}{id, acct.Currency, rep})
	return nil
}

func (s *Server) invariants(w http.ResponseWriter, r *http.Request) error {
	start := time.Now()
	head, v := s.l.Audit()
	if len(v) > 0 {
		s.log.Error("ledger invariants violated", "request_id", r.Context().Value(requestIDKey), "count", len(v), "first", v[0])
	}
	if v == nil {
		v = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(v) == 0, "violations": v, "checked_through_seq": head,
		"stats": s.l.Stats(), "duration_ms": float64(time.Since(start).Microseconds()) / 1000})
	return nil
}
