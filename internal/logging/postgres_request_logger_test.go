package logging

import (
	"strings"
	"testing"
	"time"
)

// TestSessionKeyPrefersClientHeader documents the measured production behaviour:
// every real client sends X-Session-Id (481/481 requests over 14 days), so no
// prefix-matching heuristic is needed to group turns.
func TestSessionKeyPrefersClientHeader(t *testing.T) {
	headers := map[string][]string{
		"X-Session-Id": {"ses_f7bef9b4cffe4TRnMecA5mrXuW"},
		"User-Agent":   {"opencode/1.18.29"},
	}
	if got := sessionKeyFrom(headers, []byte(`{"model":"gpt-5.5"}`)); got != "ses_f7bef9b4cffe4TRnMecA5mrXuW" {
		t.Fatalf("session key = %q, want the client-supplied id", got)
	}
}

// TestSessionKeyHeaderLookupIsCaseInsensitive guards against Go's canonical header
// casing differing from what a client sent.
func TestSessionKeyHeaderLookupIsCaseInsensitive(t *testing.T) {
	headers := map[string][]string{"x-session-id": {"ses_abc"}}
	if got := sessionKeyFrom(headers, nil); got != "ses_abc" {
		t.Fatalf("session key = %q, want ses_abc", got)
	}
}

// TestSessionKeyFallsBackToAnonymous covers clients that send no session id.
func TestSessionKeyFallsBackToAnonymous(t *testing.T) {
	headers := map[string][]string{"User-Agent": {"curl/8.0"}}
	got := sessionKeyFrom(headers, []byte(`{"model":"gpt-5.5"}`))
	if !strings.HasPrefix(got, anonSessionPrefix) {
		t.Fatalf("session key = %q, want the %q prefix", got, anonSessionPrefix)
	}
	// Stable for the same inputs within a day.
	if again := sessionKeyFrom(headers, []byte(`{"model":"gpt-5.5"}`)); again != got {
		t.Fatalf("anonymous key not stable: %q then %q", got, again)
	}
	// Different client, different session.
	other := sessionKeyFrom(map[string][]string{"User-Agent": {"zed/1.0"}}, []byte(`{"model":"gpt-5.5"}`))
	if other == got {
		t.Fatal("different clients collapsed into one anonymous session")
	}
}

// TestSanitizeStripsNulAndInvalidUTF8 is the regression test for the insert path:
// PostgreSQL rejects U+0000 in text and jsonb alike, and truncating at a byte boundary
// can split a multi-byte rune.
func TestSanitizeStripsNulAndInvalidUTF8(t *testing.T) {
	if got := sanitize("ok"); got != "ok" {
		t.Fatalf("sanitize(%q) = %q, want unchanged", "ok", got)
	}
	if got := sanitize("a\x00b"); got != "ab" {
		t.Fatalf("sanitize kept a NUL: %q", got)
	}
	// "xin chào" cut mid-rune.
	broken := string([]byte("xin ch\xc3"))
	got := sanitize(broken)
	for _, r := range got {
		if r == 0xFFFD {
			t.Fatalf("sanitize left a replacement char in %q", got)
		}
	}
	if strings.ContainsRune(got, 0) {
		t.Fatalf("sanitize left a NUL in %q", got)
	}
}

// TestModelFromBody covers the model extraction used for the session's model list.
func TestModelFromBody(t *testing.T) {
	if got := modelFrom([]byte(`{"model":"gpt-5.6-sol","messages":[]}`)); got != "gpt-5.6-sol" {
		t.Fatalf("modelFrom = %q, want gpt-5.6-sol", got)
	}
	if got := modelFrom([]byte("not json")); got != "" {
		t.Fatalf("modelFrom on invalid JSON = %q, want empty", got)
	}
	if got := modelFrom(nil); got != "" {
		t.Fatalf("modelFrom(nil) = %q, want empty", got)
	}
}

// TestCapString covers the body cap helper.
func TestCapString(t *testing.T) {
	if got := capString("abcdef", 3); got != "abc" {
		t.Fatalf("capString = %q, want abc", got)
	}
	if got := capString("abc", 0); got != "abc" {
		t.Fatalf("capString with no limit = %q, want abc", got)
	}
	if got := capString("abc", 10); got != "abc" {
		t.Fatalf("capString under limit = %q, want abc", got)
	}
}

// TestEnqueueDropsWhenFullAndNeverBlocks is the fail-open guarantee: a stalled writer
// must never hold up a live request.
func TestEnqueueDropsWhenFullAndNeverBlocks(t *testing.T) {
	l := &PostgresRequestLogger{
		queue: make(chan *ChatTurn, 1),
		done:  make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			l.enqueue(&ChatTurn{SessionKey: "ses_x"})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enqueue blocked when the queue was full")
	}

	if _, dropped, _ := l.Stats(); dropped != 99 {
		t.Fatalf("dropped = %d, want 99", dropped)
	}
}

// TestStreamWriterCapsResponseText keeps a long stream from growing without bound.
func TestStreamWriterCapsResponseText(t *testing.T) {
	l := &PostgresRequestLogger{
		queue: make(chan *ChatTurn, 1),
		done:  make(chan struct{}),
	}
	w := &postgresStreamWriter{logger: l, turn: &ChatTurn{SessionKey: "ses_x"}}

	chunk := []byte(strings.Repeat("a", 4096))
	for i := 0; i < (maxResponseTextBytes/len(chunk))+10; i++ {
		w.WriteChunkAsync(chunk)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	turn := <-l.queue
	if len(turn.ResponseText) > maxResponseTextBytes {
		t.Fatalf("response text = %d bytes, want <= %d", len(turn.ResponseText), maxResponseTextBytes)
	}
	if !turn.Truncated {
		t.Fatal("Truncated = false after exceeding the response cap")
	}
}

// TestStreamWriterCloseIsIdempotent guards against a double enqueue.
func TestStreamWriterCloseIsIdempotent(t *testing.T) {
	l := &PostgresRequestLogger{
		queue: make(chan *ChatTurn, 4),
		done:  make(chan struct{}),
	}
	w := &postgresStreamWriter{logger: l, turn: &ChatTurn{SessionKey: "ses_x"}}
	_ = w.Close()
	_ = w.Close()

	if len(l.queue) != 1 {
		t.Fatalf("queued %d turns, want 1", len(l.queue))
	}
}

// TestShouldCapturePath covers the allowlist behaviour.
func TestShouldCapturePath(t *testing.T) {
	l := &PostgresRequestLogger{paths: map[string]struct{}{"/v1/chat/completions": {}}}
	if !l.ShouldCapturePath("/v1/chat/completions") {
		t.Fatal("allowlisted path rejected")
	}
	if l.ShouldCapturePath("/v1/models") {
		t.Fatal("non-allowlisted path accepted")
	}

	open := &PostgresRequestLogger{}
	if !open.ShouldCapturePath("/anything") {
		t.Fatal("empty allowlist should capture everything")
	}
}

// TestIsEnabledIsConstant documents why this value must not vary: a false value makes
// the middleware capture only failed turns, and a value that changes mid-request leaks
// the streaming goroutine in ResponseWriterWrapper.Finalize.
func TestIsEnabledIsConstant(t *testing.T) {
	l := &PostgresRequestLogger{}
	if !l.IsEnabled() || !l.IsEnabled() {
		t.Fatal("IsEnabled must always report true")
	}
}
