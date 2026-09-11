package middleware

import (
	"bytes"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

// cappingLogger is a RequestLogger that advertises a capture cap and a path allowlist.
type cappingLogger struct {
	limit int64
	paths map[string]bool
}

func (l *cappingLogger) LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return nil
}

func (l *cappingLogger) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, requestID string) (logging.StreamingLogWriter, error) {
	return nil, nil
}

func (l *cappingLogger) IsEnabled() bool          { return true }
func (l *cappingLogger) MaxCaptureBytes() int64   { return l.limit }
func (l *cappingLogger) ShouldCapturePath(p string) bool {
	if l.paths == nil {
		return true
	}
	return l.paths[p]
}

// TestCaptureRequestInfoCapsBodyAndPreservesDownstream is the regression test for the
// memory hazard this feature introduces: with a logger enabled, upstream capture is
// unbounded (shouldCaptureRequestBody returns true, captureRequestInfo does io.ReadAll),
// which is how a 57 MB body OOM-killed this container once.
//
// It asserts both halves of the fix: the LOGGED copy is capped, and the handler still
// receives the body in full.
func TestCaptureRequestInfoCapsBodyAndPreservesDownstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const limit = 64
	full := strings.Repeat("x", limit*10)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(full))
	c.Request = req

	info, err := captureRequestInfo(c, true, limit)
	if err != nil {
		t.Fatalf("captureRequestInfo: %v", err)
	}

	if int64(len(info.Body)) != limit {
		t.Fatalf("logged body = %d bytes, want %d", len(info.Body), limit)
	}
	if !info.BodyTruncated {
		t.Fatal("BodyTruncated = false, want true")
	}

	got, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		t.Fatalf("read restored body: %v", errRead)
	}
	if string(got) != full {
		t.Fatalf("downstream body corrupted: got %d bytes, want %d", len(got), len(full))
	}
}

// TestCaptureRequestInfoUnderCapDecodes verifies the normal path is untouched: a body
// below the cap is still decoded (and not marked truncated).
func TestCaptureRequestInfoUnderCapDecodes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := `{"model":"gpt-5.5"}`
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))

	info, err := captureRequestInfo(c, true, 1024)
	if err != nil {
		t.Fatalf("captureRequestInfo: %v", err)
	}
	if string(info.Body) != body {
		t.Fatalf("body = %q, want %q", info.Body, body)
	}
	if info.BodyTruncated {
		t.Fatal("BodyTruncated = true for a body under the cap")
	}
}

// TestCaptureRequestInfoZeroLimitKeepsUpstreamBehaviour guards the fallback: a logger
// without a cap must behave exactly like upstream.
func TestCaptureRequestInfoZeroLimitKeepsUpstreamBehaviour(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := strings.Repeat("y", 4096)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))

	info, err := captureRequestInfo(c, true, 0)
	if err != nil {
		t.Fatalf("captureRequestInfo: %v", err)
	}
	if len(info.Body) != len(body) {
		t.Fatalf("body = %d bytes, want %d", len(info.Body), len(body))
	}
	if info.BodyTruncated {
		t.Fatal("BodyTruncated = true with no cap configured")
	}
}

// TestCaptureLimitFor covers both the capped and the uncapped logger shape.
func TestCaptureLimitFor(t *testing.T) {
	if got := captureLimitFor(&cappingLogger{limit: 128}); got != 128 {
		t.Fatalf("captureLimitFor = %d, want 128", got)
	}
	if got := captureLimitFor(&cappingLogger{limit: 0}); got != 0 {
		t.Fatalf("captureLimitFor = %d, want 0 for an unlimited logger", got)
	}
}

// TestResponseBodyBufferRespectsCap covers the response half of the same hazard.
func TestResponseBodyBufferRespectsCap(t *testing.T) {
	w := &ResponseWriterWrapper{
		body:         &bytes.Buffer{},
		maxBodyBytes: 10,
	}
	w.appendBufferedBody([]byte("12345"))
	w.appendBufferedBody([]byte("67890ABCDE"))

	if w.body.Len() != 10 {
		t.Fatalf("buffered %d bytes, want 10", w.body.Len())
	}
	if !w.bodyTruncated {
		t.Fatal("bodyTruncated = false after exceeding the cap")
	}
	if got := w.body.String(); got != "1234567890" {
		t.Fatalf("buffered %q, want %q", got, "1234567890")
	}
}

// TestResponseBodyBufferUnlimited keeps the upstream behaviour when no cap is set.
func TestResponseBodyBufferUnlimited(t *testing.T) {
	w := &ResponseWriterWrapper{body: &bytes.Buffer{}}
	w.appendBufferedBody([]byte(strings.Repeat("z", 5000)))
	if w.body.Len() != 5000 {
		t.Fatalf("buffered %d bytes, want 5000", w.body.Len())
	}
	if w.bodyTruncated {
		t.Fatal("bodyTruncated = true with no cap")
	}
}

// TestPathSelectorGatesCapture documents that a logger can narrow the captured paths;
// upstream's shouldLogRequest only excludes management routes.
func TestPathSelectorGatesCapture(t *testing.T) {
	l := &cappingLogger{paths: map[string]bool{"/v1/chat/completions": true}}
	if !l.ShouldCapturePath("/v1/chat/completions") {
		t.Fatal("allowlisted path rejected")
	}
	if l.ShouldCapturePath("/v1/models") {
		t.Fatal("non-allowlisted path accepted")
	}
}
