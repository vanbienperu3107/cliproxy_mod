// Package logging — fork-local PostgreSQL chat-history logger.
//
// This file is an addition of vanbienperu3107/cliproxy_mod, not upstream code. It
// implements logging.RequestLogger by persisting each conversational turn into
// PostgreSQL, grouped into sessions.
//
// Design constraints that are not obvious from the interface:
//
//   - IsEnabled() must return true for the whole process lifetime. Returning false
//     makes ResponseWriterWrapper buffer only 4xx/5xx responses and never open a
//     streaming writer, so only failed turns would be recorded. And a value that
//     CHANGES mid-request leaks the streaming goroutine, because Finalize returns
//     before close(chunkChannel) when the logger reads as disabled.
//
//   - SetEnabled(bool) is deliberately NOT implemented. server.go picks that method up
//     via type assertion and server_reload.go then calls it with cfg.RequestLog on every
//     config reload. Since this fork runs with request-log:false, implementing it would
//     silently switch chat history off on the first reload, with no error anywhere.
//
//   - Nothing here may block or fail the request path. Every write is queued; a full
//     queue drops the record, and database errors are logged and swallowed.
package logging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	log "github.com/sirupsen/logrus"
)

const (
	sessionHeader        = "X-Session-Id"
	userAgentHeader      = "User-Agent"
	contentTypeHeader    = "Content-Type"
	anonSessionPrefix    = "sess_anon_"
	writeTimeout         = 10 * time.Second
	poolAcquireTimeout   = 5 * time.Second
	maxResponseTextBytes = 1 << 20
)

// ChatTurn is one recorded request/response exchange.
type ChatTurn struct {
	SessionKey   string
	RequestID    string
	CreatedAt    time.Time
	Model        string
	URI          string
	StatusCode   int
	Streaming    bool
	DurationMS   int64
	TTFBMS       int64
	RequestBody  string
	ResponseText string
	UserAgent    string
	ErrorText    string
	Truncated    bool
}

// PostgresRequestLogger implements RequestLogger by writing turns to PostgreSQL.
type PostgresRequestLogger struct {
	pool       *pgxpool.Pool
	queue      chan *ChatTurn
	done       chan struct{}
	closeOnce  sync.Once
	wg         sync.WaitGroup
	maxCapture int64
	paths      map[string]struct{}
	dropped    atomic.Int64
	written    atomic.Int64
	failed     atomic.Int64
}

// NewPostgresRequestLogger connects to PostgreSQL and starts the writer goroutine.
//
// A connection failure is NOT fatal: it returns an error and the caller falls back to
// the file logger, because losing chat history must never stop the proxy from serving.
func NewPostgresRequestLogger(ctx context.Context, dsn string, maxCapture int64, queueSize int, paths []string) (*PostgresRequestLogger, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("chat-history: bad dsn: %w", err)
	}
	// Small pool: this is a low-volume side channel, not the hot path.
	poolCfg.MaxConns = 4
	poolCfg.MinConns = 0

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("chat-history: connect: %w", err)
	}

	pathSet := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		pathSet[p] = struct{}{}
	}

	l := &PostgresRequestLogger{
		pool:       pool,
		queue:      make(chan *ChatTurn, queueSize),
		done:       make(chan struct{}),
		maxCapture: maxCapture,
		paths:      pathSet,
	}
	l.wg.Add(1)
	go l.run()
	return l, nil
}

// IsEnabled always reports true. See the package comment for why this must not vary.
func (l *PostgresRequestLogger) IsEnabled() bool { return true }

// MaxCaptureBytes satisfies the middleware's captureLimiter interface.
func (l *PostgresRequestLogger) MaxCaptureBytes() int64 { return l.maxCapture }

// ShouldCapturePath satisfies the middleware's pathSelector interface.
func (l *PostgresRequestLogger) ShouldCapturePath(path string) bool {
	if len(l.paths) == 0 {
		return true
	}
	_, ok := l.paths[path]
	return ok
}

// Stats returns counters for observability.
func (l *PostgresRequestLogger) Stats() (written, dropped, failed int64) {
	return l.written.Load(), l.dropped.Load(), l.failed.Load()
}

// Close drains the queue and releases the pool.
func (l *PostgresRequestLogger) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
		l.wg.Wait()
		l.pool.Close()
	})
	return nil
}

// enqueue hands a turn to the writer goroutine, dropping it when the queue is full.
func (l *PostgresRequestLogger) enqueue(turn *ChatTurn) {
	if turn == nil {
		return
	}
	select {
	case <-l.done:
		return
	default:
	}
	select {
	case l.queue <- turn:
	default:
		// Full queue: drop rather than block a live request.
		if n := l.dropped.Add(1); n == 1 || n%100 == 0 {
			log.Warnf("chat-history: queue full, dropped %d record(s)", n)
		}
	}
}

func (l *PostgresRequestLogger) run() {
	defer l.wg.Done()
	for {
		select {
		case turn := <-l.queue:
			l.writeSafely(turn)
		case <-l.done:
			// Drain what is already queued, then stop.
			for {
				select {
				case turn := <-l.queue:
					l.writeSafely(turn)
				default:
					return
				}
			}
		}
	}
}

// writeSafely never lets a panic or a database error escape into the process.
func (l *PostgresRequestLogger) writeSafely(turn *ChatTurn) {
	defer func() {
		if r := recover(); r != nil {
			l.failed.Add(1)
			log.Errorf("chat-history: panic while writing turn: %v", r)
		}
	}()
	if err := l.write(turn); err != nil {
		l.failed.Add(1)
		log.WithError(err).Warn("chat-history: write failed")
		return
	}
	l.written.Add(1)
}

func (l *PostgresRequestLogger) write(turn *ChatTurn) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()

	// Rows are written one at a time on purpose. Batching would let a single rejected
	// row abort every other row in the same transaction, and rejected rows are expected
	// here: bodies are truncated at the capture cap and may carry invalid UTF-8.
	var sessionID int64
	err := l.pool.QueryRow(ctx, `
		INSERT INTO chat_history.chat_sessions (session_key, first_seen_at, last_seen_at, turn_count, client_ua, models)
		VALUES ($1, $2, $2, 1, $3, CASE WHEN $4 = '' THEN '{}'::text[] ELSE ARRAY[$4] END)
		ON CONFLICT (session_key) DO UPDATE SET
			last_seen_at = EXCLUDED.last_seen_at,
			turn_count   = chat_history.chat_sessions.turn_count + 1,
			client_ua    = COALESCE(NULLIF(EXCLUDED.client_ua, ''), chat_history.chat_sessions.client_ua),
			models       = CASE
				WHEN $4 = '' OR $4 = ANY(chat_history.chat_sessions.models) THEN chat_history.chat_sessions.models
				ELSE array_append(chat_history.chat_sessions.models, $4)
			END
		RETURNING id`,
		turn.SessionKey, turn.CreatedAt, sanitize(turn.UserAgent), sanitize(turn.Model),
	).Scan(&sessionID)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}

	requestBody := sanitize(turn.RequestBody)
	var requestJSON any
	if json.Valid([]byte(requestBody)) {
		requestJSON = requestBody
	}

	_, err = l.pool.Exec(ctx, `
		INSERT INTO chat_history.chat_turns (
			session_id, request_id, created_at, model, uri, status_code, streaming,
			duration_ms, ttfb_ms, request_body, request_json, response_text, error_text, truncated
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		sessionID, sanitize(turn.RequestID), turn.CreatedAt, sanitize(turn.Model),
		sanitize(turn.URI), turn.StatusCode, turn.Streaming, turn.DurationMS, turn.TTFBMS,
		requestBody, requestJSON, sanitize(turn.ResponseText), sanitize(turn.ErrorText),
		turn.Truncated,
	)
	if err != nil {
		return fmt.Errorf("insert turn: %w", err)
	}
	return nil
}

// LogRequest records a non-streaming request/response cycle.
func (l *PostgresRequestLogger) LogRequest(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte,
	apiResponseErrors []*interfaces.ErrorMessage,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	turn := &ChatTurn{
		SessionKey:   sessionKeyFrom(requestHeaders, body),
		RequestID:    requestID,
		CreatedAt:    requestTimestamp,
		Model:        modelFrom(body),
		URI:          url,
		StatusCode:   statusCode,
		Streaming:    false,
		RequestBody:  capString(string(body), l.maxCapture),
		ResponseText: capString(string(response), maxResponseTextBytes),
		UserAgent:    headerValue(requestHeaders, userAgentHeader),
		ErrorText:    joinErrors(apiResponseErrors),
	}
	if !requestTimestamp.IsZero() && !apiResponseTimestamp.IsZero() {
		turn.DurationMS = apiResponseTimestamp.Sub(requestTimestamp).Milliseconds()
	}
	l.enqueue(turn)
	return nil
}

// LogStreamingRequest starts recording a streaming exchange.
func (l *PostgresRequestLogger) LogStreamingRequest(
	url, method string,
	headers map[string][]string,
	body []byte,
	requestID string,
) (StreamingLogWriter, error) {
	return &postgresStreamWriter{
		logger: l,
		turn: &ChatTurn{
			SessionKey:  sessionKeyFrom(headers, body),
			RequestID:   requestID,
			CreatedAt:   time.Now(),
			Model:       modelFrom(body),
			URI:         url,
			Streaming:   true,
			RequestBody: capString(string(body), l.maxCapture),
			UserAgent:   headerValue(headers, userAgentHeader),
		},
		started: time.Now(),
	}, nil
}

// postgresStreamWriter accumulates streamed chunks and enqueues one turn on Close.
type postgresStreamWriter struct {
	logger    *PostgresRequestLogger
	turn      *ChatTurn
	chunks    strings.Builder
	started   time.Time
	firstAt   time.Time
	closeOnce sync.Once
	mu        sync.Mutex
}

func (w *postgresStreamWriter) WriteChunkAsync(chunk []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.chunks.Len() >= maxResponseTextBytes {
		w.turn.Truncated = true
		return
	}
	remaining := maxResponseTextBytes - w.chunks.Len()
	if len(chunk) > remaining {
		w.chunks.Write(chunk[:remaining])
		w.turn.Truncated = true
		return
	}
	w.chunks.Write(chunk)
}

func (w *postgresStreamWriter) WriteStatus(status int, headers map[string][]string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.turn.StatusCode = status
	return nil
}

func (w *postgresStreamWriter) WriteAPIRequest(apiRequest []byte) error  { return nil }
func (w *postgresStreamWriter) WriteAPIResponse(apiResponse []byte) error { return nil }
func (w *postgresStreamWriter) WriteAPIWebsocketTimeline(timeline []byte) error {
	return nil
}

func (w *postgresStreamWriter) SetFirstChunkTimestamp(timestamp time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.firstAt = timestamp
}

func (w *postgresStreamWriter) Close() error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		turn := w.turn
		turn.ResponseText = w.chunks.String()
		turn.DurationMS = time.Since(w.started).Milliseconds()
		if !w.firstAt.IsZero() {
			turn.TTFBMS = w.firstAt.Sub(w.started).Milliseconds()
		}
		w.mu.Unlock()
		w.logger.enqueue(turn)
	})
	return nil
}

// sessionKeyFrom prefers the client-supplied session id.
//
// Measured on 14 days of production traffic: every real client (opencode/1.18.x) sends
// X-Session-Id, 481/481 requests, sessions spanning up to 72.9 hours. The anonymous
// fallback exists only for clients that do not.
func sessionKeyFrom(headers map[string][]string, body []byte) string {
	if v := headerValue(headers, sessionHeader); v != "" {
		return v
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		headerValue(headers, userAgentHeader),
		modelFrom(body),
		time.Now().UTC().Format("2006-01-02"),
	}, "|")))
	return anonSessionPrefix + hex.EncodeToString(sum[:])[:16]
}

func headerValue(headers map[string][]string, name string) string {
	if headers == nil {
		return ""
	}
	for k, v := range headers {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// modelFrom pulls the model name out of a request body without unmarshalling all of it.
func modelFrom(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

func joinErrors(errs []*interfaces.ErrorMessage) string {
	if len(errs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		if e == nil || e.Error == nil {
			continue
		}
		parts = append(parts, e.Error.Error())
	}
	return strings.Join(parts, "; ")
}

func capString(s string, limit int64) string {
	if limit <= 0 || int64(len(s)) <= limit {
		return s
	}
	return s[:limit]
}

// sanitize strips NUL bytes and invalid UTF-8.
//
// PostgreSQL rejects U+0000 in both text and jsonb. NULs reach us for real: a body whose
// decompression fails is passed through raw (decodeCapturedRequestBodyForLog), and
// truncating at a byte boundary can also split a multi-byte rune.
func sanitize(s string) string {
	if s == "" {
		return s
	}
	if !strings.ContainsRune(s, 0) && utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "")
}
