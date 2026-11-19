package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/laiwf94/ledgerd/internal/metrics"
)

// The live entry stream serves the journal feed over Server-Sent Events.
//
// A stream does not queue entries for its consumer. The journal is
// append-only and in memory, so each stream keeps only its position and
// reads the next page of the feed itself, with no lock held (see
// ledger.Entries); the writer never waits for a stream and never copies an
// entry for one. When a page reaches the head the stream waits on
// ledger.EntriesGrown, which the writer closes after a commit that appends
// entries, at no cost while nobody waits.
//
// Each event carries its entry's sequence as its id, so an EventSource that
// reconnects sends it back as Last-Event-ID and the stream resumes right
// after it: no gaps, and no duplicates beyond the at-least-once of a
// consumer that had not finished processing an event when it dropped.
// A stream opened with alerts=include or alerts=only carries, in each
// entry's event, the balance alerts that entry raised, so Last-Event-ID
// resumes the alerts exactly as it resumes the entries.
//
// Heartbeats carry the stream's position as their id too, so a consumer
// whose filter matched nothing for a while resumes from there instead of
// rescanning.
//
// Each connection has one buffer, bounded by a page of streamPage entries,
// and every write of it must reach the client within the write timeout. A
// consumer that does not read fast enough fills its TCP window, the write
// misses its deadline and the server closes the connection; the consumer
// reconnects with Last-Event-ID when it is ready again.

// Stream defaults.
const (
	DefaultStreamHeartbeat    = 15 * time.Second
	DefaultStreamWriteTimeout = 10 * time.Second
	DefaultMaxStreams         = 1024
	// streamPage is how many entries one write carries at most.
	streamPage = 256
	// streamRetry is the reconnection delay the stream asks clients for.
	streamRetry       = 2 * time.Second
	lastEventIDHeader = "Last-Event-ID"
)

// streams tracks the open entry streams of a server.
type streams struct {
	slots     chan struct{} // one token per open stream
	heartbeat time.Duration
	timeout   time.Duration
	stop      chan struct{} // closed by CloseStreams
	stopOnce  sync.Once
	open      atomic.Int64

	mEvents, mAlerts, mSlow, mRefused *metrics.Counter
}

func newStreams(cfg Config, m *metrics.Registry) *streams {
	st := &streams{heartbeat: cfg.StreamHeartbeat, timeout: cfg.StreamWriteTimeout, stop: make(chan struct{})}
	if st.heartbeat <= 0 {
		st.heartbeat = DefaultStreamHeartbeat
	}
	if st.timeout <= 0 {
		st.timeout = DefaultStreamWriteTimeout
	}
	n := cfg.MaxStreams
	if n <= 0 {
		n = DefaultMaxStreams
	}
	st.slots = make(chan struct{}, n)
	m.Gauge("ledgerd_entry_streams", "Open live entry streams.", func() float64 { return float64(st.open.Load()) })
	st.mEvents = m.Counter("ledgerd_entry_stream_events_total", "", "Entries sent on live entry streams.")
	st.mAlerts = m.Counter("ledgerd_entry_stream_alerts_total", "", "Balance alerts sent with entries on live entry streams.")
	st.mSlow = m.Counter("ledgerd_entry_stream_slow_disconnects_total", "", "Entry streams closed because the consumer did not read in time.")
	st.mRefused = m.Counter("ledgerd_entry_stream_refused_total", "", "Entry streams refused because the stream limit was reached.")
	return st
}

// CloseStreams ends every open entry stream and refuses new ones. A server
// calls it when it starts shutting down, since http.Server.Shutdown waits
// for handlers and a stream would otherwise never return; register it with
// http.Server.RegisterOnShutdown. Clients reconnect with Last-Event-ID.
func (s *Server) CloseStreams() {
	s.streams.stopOnce.Do(func() { close(s.streams.stop) })
}

// streamEntries serves GET /v1/entries/stream.
func (s *Server) streamEntries(w http.ResponseWriter, r *http.Request) error {
	st := s.streams
	f, after, _, err := parseEntryQuery(r, false)
	if err != nil {
		return err
	}
	if v := r.Header.Get(lastEventIDHeader); v != "" {
		// A reconnecting EventSource repeats the original URL, so its
		// Last-Event-ID wins over the after_seq in it.
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || len(v) > maxQueryValueBytes || n > 1<<63 {
			return badRequest("%s must be a whole number up to %d", lastEventIDHeader, uint64(1<<63))
		}
		after = n
	}
	select {
	case <-st.stop:
		return &apiError{http.StatusServiceUnavailable, "unavailable", "the server is shutting down"}
	default:
	}
	// Take the wake-up before the first read, so a commit between the read
	// and the wait is not missed. The first page also validates the filter
	// and position while an error can still be a status code.
	grown := s.l.EntriesGrown()
	page, err := s.l.Entries(f, after, streamPage)
	if err != nil {
		return err
	}
	select {
	case st.slots <- struct{}{}:
	default:
		st.mRefused.Inc()
		return &apiError{http.StatusServiceUnavailable, "unavailable", "too many open entry streams"}
	}
	st.open.Add(1)
	defer func() {
		st.open.Add(-1)
		<-st.slots
	}()

	// The server's write timeout covers a whole response; every write of a
	// stream gets its own deadline instead.
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	buf.WriteString("retry: " + strconv.FormatInt(streamRetry.Milliseconds(), 10) + "\n\n")
	// send writes the buffer out within the write timeout. It returns false
	// when the stream must end: the consumer has gone, or it is too slow.
	send := func() bool {
		err := rc.SetWriteDeadline(time.Now().Add(st.timeout))
		if err == nil {
			_, err = w.Write(buf.Bytes())
		}
		if err == nil {
			err = rc.Flush()
		}
		buf.Reset()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				st.mSlow.Inc()
				s.log.Warn("closing a slow entry stream", "request_id", r.Context().Value(requestIDKey), "position", after)
			}
			return false
		}
		return true
	}
	heartbeat := time.NewTimer(st.heartbeat)
	defer heartbeat.Stop()
	for {
		for i := range page.Entries {
			e := &page.Entries[i]
			buf.WriteString("id: ")
			buf.WriteString(strconv.FormatUint(e.Seq, 10))
			buf.WriteString("\nevent: entry\ndata: ")
			var v any = e
			if page.Alerts != nil {
				v = page.Item(i)
			}
			if err := enc.Encode(v); err != nil { // ends in the newline closing the data line
				return nil
			}
			buf.WriteByte('\n')
		}
		st.mEvents.Add(uint64(len(page.Entries)))
		st.mAlerts.Add(uint64(page.AlertCount()))
		after = page.Next
		if buf.Len() > 0 {
			if !send() {
				return nil
			}
			heartbeat.Reset(st.heartbeat)
		}
		for !page.HasMore() {
			select {
			case <-grown:
			case <-heartbeat.C:
				// The position, not the last entry sent: entries in between
				// did not match and never will.
				buf.WriteString("event: heartbeat\nid: ")
				buf.WriteString(strconv.FormatUint(after, 10))
				buf.WriteString("\ndata: {\"head_seq\":")
				buf.WriteString(strconv.FormatUint(page.Head, 10))
				buf.WriteString("}\n\n")
				if !send() {
					return nil
				}
				heartbeat.Reset(st.heartbeat)
				continue
			case <-r.Context().Done():
				return nil
			case <-st.stop:
				return nil
			}
			break
		}
		select {
		case <-r.Context().Done():
			return nil
		case <-st.stop:
			return nil
		default:
		}
		grown = s.l.EntriesGrown()
		if page, err = s.l.Entries(f, after, streamPage); err != nil {
			return nil
		}
	}
}
