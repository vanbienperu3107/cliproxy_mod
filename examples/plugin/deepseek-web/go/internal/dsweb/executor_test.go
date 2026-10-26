package dsweb

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeUpstream struct {
	mu            sync.Mutex
	completionReq map[string]any
	answer        string
	answerType    string
	contentType   string
	authCode      int
	mute          bool
}

func (f *fakeUpstream) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/users/current":
			if f.authCode != 0 {
				writeJSON(w, map[string]any{"code": f.authCode, "msg": "invalid token"})
				return
			}
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
				"biz_data": map[string]any{"token": "access-token"},
			}})
		case "/api/v0/chat_session/create":
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
				"biz_data": map[string]any{"chat_session": map[string]any{"id": "session-1"}},
			}})
		case "/api/v0/chat_session/delete":
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{}})
		case "/api/v0/chat/create_pow_challenge":
			salt := "salt"
			expire := int64(123)
			digest := deepSeekHashV1Hex(salt + "_123_0")
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
				"biz_data": map[string]any{"challenge": map[string]any{
					"algorithm":   deepSeekPowAlgorithm,
					"challenge":   digest,
					"salt":        salt,
					"difficulty":  1,
					"expire_at":   expire,
					"signature":   "sig",
					"target_path": deepSeekCompletionPath,
				}},
			}})
		case "/api/v0/chat/completion":
			body, _ := io.ReadAll(r.Body)
			var decoded map[string]any
			_ = json.Unmarshal(body, &decoded)
			f.mu.Lock()
			f.completionReq = decoded
			f.mu.Unlock()

			if r.Header.Get("x-ds-pow-response") == "" {
				t.Error("completion request carried no proof of work")
			}

			if f.mute {
				writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
					"biz_code": 5, "biz_msg": "user is muted",
				}})
				return
			}

			w.Header().Set("Content-Type", f.contentType)
			if f.answerType == "json" {
				writeJSON(w, map[string]any{"code": 0, "data": map[string]any{}})
				return
			}
			_, _ = io.WriteString(w, f.answer)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func sseBody(events ...string) string {
	var b strings.Builder
	for _, event := range events {
		b.WriteString("data: ")
		b.WriteString(event)
		b.WriteString("\n\n")
	}
	return b.String()
}

func testExecutor(t *testing.T, upstream *fakeUpstream) (*Executor, *httptest.Server, []byte) {
	t.Helper()
	server := httptest.NewServer(upstream.handler(t))

	creds, err := json.Marshal(map[string]any{
		"userToken":      "user-token-" + t.Name(),
		"baseUrl":        server.URL + deepSeekCompletionPath,
		"persistSession": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Executor{Client: server.Client()}, server, creds
}

func requestBody(t *testing.T, model string, messages []map[string]any, tools []map[string]any) []byte {
	t.Helper()
	body := map[string]any{"model": model, "messages": messages}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestExecuteCollectsText(t *testing.T) {
	upstream := &fakeUpstream{
		contentType: "text/event-stream",
		answer: sseBody(
			`{"p":"response","o":"BATCH","v":{"response":{"thinking_enabled":true,"message_id":7,"fragments":[{"type":"THINK","content":"hmm"},{"type":"ANSWER","content":"hello"}]}}}`,
			`{"p":"response/status","v":"FINISHED"}`,
		),
	}
	executor, server, creds := testExecutor(t, upstream)
	defer server.Close()

	payload := requestBody(t, "deepseek-v4-pro", []map[string]any{{"role": "user", "content": "hi"}}, nil)
	resp, err := executor.Execute(t.Context(), pluginapi.ExecutorRequest{
		Model:       "deepseek-v4-pro",
		Payload:     payload,
		StorageJSON: creds,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Payload, &completion); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %d", len(completion.Choices))
	}
	if completion.Choices[0].Message.Content != "hello" {
		t.Fatalf("content = %q", completion.Choices[0].Message.Content)
	}
	if completion.Choices[0].Message.ReasoningContent != "hmm" {
		t.Fatalf("reasoning = %q", completion.Choices[0].Message.ReasoningContent)
	}
	if completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", completion.Choices[0].FinishReason)
	}

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.completionReq["prompt"] != "hi" {
		t.Fatalf("upstream prompt = %v", upstream.completionReq["prompt"])
	}
}

func TestExecuteReturnsToolCalls(t *testing.T) {
	upstream := &fakeUpstream{
		contentType: "text/event-stream",
		answer: sseBody(
			`{"p":"response","o":"BATCH","v":{"response":{"fragments":[{"type":"ANSWER","content":"{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Hanoi\"}}"}]}}}`,
			`{"p":"response/status","v":"FINISHED"}`,
		),
	}
	executor, server, creds := testExecutor(t, upstream)
	defer server.Close()

	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":       "get_weather",
			"parameters": map[string]any{"type": "object"},
		},
	}}
	payload := requestBody(t, "deepseek-v4-pro", []map[string]any{{"role": "user", "content": "weather?"}}, tools)
	resp, err := executor.Execute(t.Context(), pluginapi.ExecutorRequest{
		Model:       "deepseek-v4-pro",
		Payload:     payload,
		StorageJSON: creds,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var completion struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Payload, &completion); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	choice := completion.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", choice.Message.ToolCalls)
	}
	if choice.Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("name = %q", choice.Message.ToolCalls[0].Function.Name)
	}
	if choice.Message.ToolCalls[0].Function.Arguments != `{"city":"Hanoi"}` {
		t.Fatalf("arguments = %q", choice.Message.ToolCalls[0].Function.Arguments)
	}
}

func TestStartStreamEmitsSSE(t *testing.T) {
	upstream := &fakeUpstream{
		contentType: "text/event-stream",
		answer: sseBody(
			`{"p":"response","o":"BATCH","v":{"response":{"fragments":[{"type":"ANSWER","content":"hello"}]}}}`,
			`{"p":"response/status","v":"FINISHED"}`,
		),
	}
	server := httptest.NewServer(upstream.handler(t))

	creds, _ := json.Marshal(map[string]any{
		"userToken":      "stream-token-" + t.Name(),
		"baseUrl":        server.URL + deepSeekCompletionPath,
		"persistSession": false,
	})

	var mu sync.Mutex
	var chunks [][]byte
	closed := make(chan string, 1)
	executor := &Executor{
		Client: server.Client(),
		Emit: func(_ string, payload []byte) error {
			mu.Lock()
			defer mu.Unlock()
			chunks = append(chunks, append([]byte(nil), payload...))
			return nil
		},
		Close: func(_ string, errMsg string) {
			closed <- errMsg
		},
	}

	payload := requestBody(t, "deepseek-v4-pro", []map[string]any{{"role": "user", "content": "hi"}}, nil)
	resp, err := executor.StartStream(t.Context(), pluginapi.ExecutorRequest{
		Model:       "deepseek-v4-pro",
		Payload:     payload,
		StorageJSON: creds,
	}, "stream-1")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	if got := resp.Headers.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}

	select {
	case errMsg := <-closed:
		if errMsg != "" {
			t.Fatalf("stream closed with error: %s", errMsg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not close")
	}

	mu.Lock()
	frames := append([][]byte(nil), chunks...)
	mu.Unlock()
	if len(frames) != 2 {
		t.Fatalf("chunks = %d, want 2: %q", len(frames), frames)
	}
	for _, frame := range frames {
		if bytes.HasPrefix(frame, []byte("data:")) {
			t.Fatalf("chunk must be a bare JSON payload, got %q", frame)
		}
		var chunk map[string]any
		if err := json.Unmarshal(frame, &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %v (%q)", err, frame)
		}
	}
	first, last := string(frames[0]), string(frames[1])
	if !strings.Contains(first, `"content":"hello"`) {
		t.Fatalf("first chunk missing content: %s", first)
	}
	if !strings.Contains(last, `"finish_reason":"stop"`) {
		t.Fatalf("last chunk missing stop: %s", last)
	}
}

func TestExecuteMapsInvalidTokenTo401(t *testing.T) {
	upstream := &fakeUpstream{authCode: deepSeekCodeAuthFailed}
	executor, server, creds := testExecutor(t, upstream)
	defer server.Close()

	payload := requestBody(t, "deepseek-v4-pro", []map[string]any{{"role": "user", "content": "hi"}}, nil)
	_, err := executor.Execute(t.Context(), pluginapi.ExecutorRequest{
		Model:       "deepseek-v4-pro",
		Payload:     payload,
		StorageJSON: creds,
	})
	if err == nil {
		t.Fatal("expected an auth error")
	}
	var httpErr *HTTPError
	if !asHTTPError(err, &httpErr) {
		t.Fatalf("error is not *HTTPError: %v", err)
	}
	if httpErr.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", httpErr.StatusCode())
	}
}

func TestExecuteMapsMuteTo429(t *testing.T) {
	upstream := &fakeUpstream{
		contentType: "application/json",
		answerType:  "json",
		mute:        true,
	}
	executor, server, creds := testExecutor(t, upstream)
	defer server.Close()

	payload := requestBody(t, "deepseek-v4-pro", []map[string]any{{"role": "user", "content": "hi"}}, nil)
	_, err := executor.Execute(t.Context(), pluginapi.ExecutorRequest{
		Model:       "deepseek-v4-pro",
		Payload:     payload,
		StorageJSON: creds,
	})
	if err == nil {
		t.Fatal("expected a mute error")
	}
	var httpErr *HTTPError
	if !asHTTPError(err, &httpErr) {
		t.Fatalf("error is not *HTTPError: %v", err)
	}
	if httpErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", httpErr.StatusCode())
	}
}

func TestParseCredentialsRejectsEmptyStorage(t *testing.T) {
	if _, err := ParseCredentials(nil); err == nil {
		t.Fatal("empty storage accepted")
	}
}

func TestNormalizeModelID(t *testing.T) {
	if got := NormalizeModelID("deepseek-web/deepseek-v4-pro"); got != "deepseek-v4-pro" {
		t.Fatalf("normalized = %q", got)
	}
	if got := NormalizeModelID("deepseek-v4-pro"); got != "deepseek-v4-pro" {
		t.Fatalf("normalized = %q", got)
	}
}

func asHTTPError(err error, target **HTTPError) bool {
	return errors.As(err, target)
}
