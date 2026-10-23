package dsweb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	streamQueueWait = 30 * time.Second
)

// Executor is the plugin executor. Emit and Close are the host stream callbacks: the
// ABI cannot be reached from this package, so main wires them to callHost.
type Executor struct {
	Client *http.Client
	Emit   func(streamID string, payload []byte) error
	Close  func(streamID string, errMsg string)
}

func (p *Executor) httpClient() *http.Client {
	if p != nil && p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

// Execute performs one non-streaming turn.
func (p *Executor) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	payload, err := p.execute(ctx, req, false, "")
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// StartStream validates the request synchronously — so a bad credential or an
// empty messages array still reaches the client as a status — then answers with
// SSE headers and runs the turn in the background, emitting through the host.
func (p *Executor) StartStream(ctx context.Context, req pluginapi.ExecutorRequest, streamID string) (pluginapi.ExecutorStreamResponse, error) {
	turn, err := p.openTurn(ctx, req, true)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, err
	}
	body, enforcement, err := peekStreamEvent(turn.response.Body)
	if err != nil {
		deepSeekAbandonSession(turn.session, ctx, turn.turn, turn.accessToken)
		turn.close()
		return pluginapi.ExecutorStreamResponse{}, newHTTPError(http.StatusBadGateway, "deepseek-web: read upstream stream: %v", err)
	}
	if enforcement != nil {
		deepSeekAbandonSession(turn.session, ctx, turn.turn, turn.accessToken)
		turn.close()
		return pluginapi.ExecutorStreamResponse{}, enforcement.error()
	}

	headers := http.Header{
		"Content-Type":  []string{"text/event-stream"},
		"Cache-Control": []string{"no-cache"},
		"Connection":    []string{"keep-alive"},
	}

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				p.closeStream(streamID, fmt.Sprintf("deepseek-web: stream panic: %v", recovered))
			}
		}()
		defer turn.close()
		// The host request context ends when the C call returns, so the turn
		// gets its own lifetime; closing the stream is what tells the host.
		runCtx := context.WithoutCancel(ctx)
		_, err := p.runTurn(runCtx, turn, true, streamID, body)
		p.closeStream(streamID, errorMessage(err))
	}()

	return pluginapi.ExecutorStreamResponse{Headers: headers}, nil
}

func (p *Executor) closeStream(streamID, errMsg string) {
	if p != nil && p.Close != nil {
		p.Close(streamID, errMsg)
	}
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type preparedTurn struct {
	envelope deepSeekRequestEnvelope
	creds    Credentials
	prompt   string
	model    string
	options  deepSeekModelOptions
}

// prepareTurn parses and validates everything that can be rejected before a
// single upstream call is made.
func prepareTurn(req pluginapi.ExecutorRequest) (preparedTurn, error) {
	var envelope deepSeekRequestEnvelope
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	if len(body) == 0 {
		return preparedTurn{}, newHTTPError(http.StatusBadRequest, "deepseek-web: request body is empty")
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return preparedTurn{}, newHTTPError(http.StatusBadRequest, "deepseek-web: parse request: %v", err)
	}

	creds, err := ParseCredentials(req.StorageJSON)
	if err != nil {
		return preparedTurn{}, err
	}
	if len(envelope.Messages) == 0 {
		return preparedTurn{}, newHTTPError(http.StatusBadRequest, "deepseek-web: received an empty messages array")
	}

	model := NormalizeModelID(envelope.Model)
	if model == "" {
		model = NormalizeModelID(req.Model)
	}
	if model == "" {
		model = "deepseek-v4-pro"
	}
	if envelope.Model == "" {
		envelope.Model = model
	}

	prompt := deepSeekTurnPrompt(envelope, creds)
	if prompt == "" {
		return preparedTurn{}, newHTTPError(http.StatusBadRequest, "deepseek-web: request has no text content")
	}

	options := resolveDeepSeekModelOptions(model, envelope)
	return preparedTurn{
		envelope: envelope,
		creds:    creds,
		prompt:   prompt,
		model:    model,
		options:  options,
	}, nil
}

// deepSeekTurnPrompt renders the conversation as the single prompt string the
// web API accepts, with the tool declarations folded in. The declarations never
// reach the upstream as a parameter — the endpoint has no field for them — so
// they lead the prompt.
func deepSeekTurnPrompt(envelope deepSeekRequestEnvelope, creds Credentials) string {
	prompt := deepSeekMessagesToPrompt(envelope.Messages, creds.HistoryWindow, creds.PersistSession)
	if prompt == "" || len(envelope.Tools) == 0 {
		return prompt
	}
	return deepSeekRenderTools(envelope.Tools) + "\n\n" + prompt
}

type openTurn struct {
	prepared    preparedTurn
	turn        deepSeekTurn
	accessToken string
	session     deepSeekSession
	response    *http.Response
	release     func()
}

func (o *openTurn) close() {
	if o == nil {
		return
	}
	if o.response != nil && o.response.Body != nil {
		_ = o.response.Body.Close()
	}
	if o.release != nil {
		o.release()
	}
}

// openTurn performs everything up to the first upstream byte: credential
// exchange, session pinning, and the completion request. Keeping it separate
// lets StartStream report setup failures with their real HTTP status.
func (p *Executor) openTurn(ctx context.Context, req pluginapi.ExecutorRequest, streaming bool) (*openTurn, error) {
	prepared, err := prepareTurn(req)
	if err != nil {
		return nil, err
	}
	creds := prepared.creds

	deepSeekPause(ctx, creds.jitter())
	release, err := deepSeekGates.acquire(ctx, creds.UserToken, creds.MaxConcurrency, streamQueueWait)
	if err != nil {
		return nil, newHTTPError(http.StatusTooManyRequests, "deepseek-web: %s", err)
	}

	turn := newDeepSeekTurn(p.httpClient(), creds.BaseURL, creds.Cookie)
	accessToken, err := turn.acquireAccessToken(ctx, creds.UserToken)
	if err != nil {
		release()
		return nil, err
	}

	session, err := deepSeekAcquireSession(ctx, turn, accessToken, creds, clientSessionID(req), streaming, prepared.prompt)
	if err != nil {
		release()
		return nil, err
	}

	response, err := turn.completion(ctx, accessToken, deepSeekCompletionRequest{
		ChatSessionID:   session.id,
		ParentMessageID: session.claim.parentMessageID,
		ModelType:       prepared.options.modelType,
		Prompt:          session.claim.tail,
		RefFileIDs:      []string{},
		ThinkingEnabled: prepared.options.thinking,
		SearchEnabled:   prepared.options.search,
		Stream:          true,
	})
	if err != nil {
		deepSeekAbandonSession(session, ctx, turn, accessToken)
		release()
		return nil, err
	}

	if enforcement := deepSeekEnforcementFromResponse(response); enforcement != nil {
		deepSeekAbandonSession(session, ctx, turn, accessToken)
		_ = response.Body.Close()
		release()
		return nil, enforcement.error()
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType != "text/event-stream" {
		deepSeekAbandonSession(session, ctx, turn, accessToken)
		_ = response.Body.Close()
		release()
		return nil, newHTTPError(http.StatusBadGateway, "deepseek-web: upstream returned %q instead of an event stream", mediaType)
	}

	return &openTurn{
		prepared:    prepared,
		turn:        turn,
		accessToken: accessToken,
		session:     session,
		response:    response,
		release:     release,
	}, nil
}

func (p *Executor) execute(ctx context.Context, req pluginapi.ExecutorRequest, streaming bool, streamID string) ([]byte, error) {
	turn, err := p.openTurn(ctx, req, streaming)
	if err != nil {
		return nil, err
	}
	defer turn.close()
	return p.runTurn(ctx, turn, streaming, streamID, turn.response.Body)
}

func (p *Executor) runTurn(ctx context.Context, turn *openTurn, streaming bool, streamID string, body io.Reader) ([]byte, error) {
	prepared := turn.prepared
	accumulator := newDeepSeekStreamAccumulator(prepared.model)
	var payload []byte
	var runErr error
	if len(prepared.envelope.Tools) > 0 {
		payload, runErr = p.toolTurn(prepared, accumulator, body, streaming, streamID)
	} else if streaming {
		runErr = p.streamTurn(prepared, accumulator, body, streamID)
	} else {
		payload, runErr = collectTurn(prepared, accumulator, body)
	}

	if !deepSeekSettleSession(turn.session, prepared.prompt, accumulator, runErr) {
		turn.turn.deleteSession(ctx, turn.accessToken, turn.session.id)
	}
	return payload, runErr
}

// peekStreamEvent reads the first SSE event before the stream is committed, so
// an account refusal delivered as the opening event still maps to 429/403
// instead of a 200 stream that ends in an error chunk. The event is replayed.
func peekStreamEvent(body io.Reader) (io.Reader, *deepSeekEnforcement, error) {
	reader := bufio.NewReaderSize(body, 64*1024)
	var raw bytes.Buffer
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			raw.WriteString(line)
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				break
			}
			if strings.HasPrefix(trimmed, "data:") {
				chunk := strings.TrimPrefix(trimmed, "data:")
				chunk = strings.TrimPrefix(chunk, " ")
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(chunk)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, nil, err
		}
	}
	if data.Len() > 0 {
		if enforcement := deepSeekEnforcementFromEvent([]byte(data.String())); enforcement != nil {
			return nil, enforcement, nil
		}
	}
	return io.MultiReader(bytes.NewReader(raw.Bytes()), reader), nil, nil
}

// toolTurn buffers the whole answer: the model types its call one character at
// a time, so a streamed answer could end mid-JSON with no way to tell a
// truncated call from prose.
func (p *Executor) toolTurn(prepared preparedTurn, accumulator *deepSeekStreamAccumulator, body io.Reader, streaming bool, streamID string) ([]byte, error) {
	if err := drainStream(body, accumulator, nil); err != nil {
		return nil, newHTTPError(http.StatusBadGateway, "deepseek-web: %v", err)
	}
	if enforcement := accumulator.enforcement; enforcement != nil {
		return nil, p.reportFailure(streaming, streamID, enforcement.error())
	}
	if !accumulator.sawFinished {
		return nil, p.reportFailure(streaming, streamID, newHTTPError(http.StatusBadGateway,
			"deepseek-web: stream ended before completion — the upstream session was cut off"))
	}

	text := accumulator.contentText()
	calls := deepSeekExtractToolCalls(text, prepared.envelope.Tools)
	if len(calls) > 0 {
		raws := make([]string, 0, len(calls))
		for _, call := range calls {
			raws = append(raws, call.Raw)
		}
		preamble := deepSeekSurroundingText(text, raws...)

		if streaming {
			return nil, p.emitToolCalls(prepared, accumulator, calls, preamble, streamID)
		}
		return marshalCompletion(prepared.model, toolCallMessage(calls, preamble))
	}

	if deepSeekHasDSML(text) {
		if stripped := deepSeekStripDSML(text); stripped != "" {
			text = stripped
		}
	}
	if streaming {
		return nil, p.emitPlainAnswer(prepared, accumulator, text, streamID)
	}
	message := map[string]any{"role": "assistant", "content": text}
	if thinking := accumulator.thinkingText(); thinking != "" {
		message["reasoning_content"] = thinking
	}
	return marshalCompletion(prepared.model, message)
}

func (p *Executor) streamTurn(prepared preparedTurn, accumulator *deepSeekStreamAccumulator, body io.Reader, streamID string) error {
	responseID := deepSeekResponseID()
	created := time.Now().Unix()
	roleAnnounced := false

	emit := func(delta map[string]any, finish any) error {
		if !roleAnnounced {
			roleAnnounced = true
			delta["role"] = "assistant"
		}
		chunk := map[string]any{
			"id":      responseID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   prepared.model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		return p.emit(streamID, encoded)
	}

	var emitErr error
	scanErr := drainStream(body, accumulator, func(content, thinking string) {
		if emitErr != nil || accumulator.enforcement != nil {
			return
		}
		if thinking != "" {
			emitErr = emit(map[string]any{"reasoning_content": thinking}, nil)
		}
		if emitErr == nil && content != "" {
			emitErr = emit(map[string]any{"content": content}, nil)
		}
	})
	if scanErr != nil {
		return p.reportFailure(true, streamID, newHTTPError(http.StatusBadGateway, "deepseek-web: %v", scanErr))
	}
	if emitErr != nil {
		return emitErr
	}

	if enforcement := accumulator.enforcement; enforcement != nil {
		return p.reportFailure(true, streamID, enforcement.error())
	}
	if !accumulator.sawFinished {
		return p.reportFailure(true, streamID, newHTTPError(http.StatusBadGateway,
			"deepseek-web: stream ended before completion — the upstream session was cut off"))
	}

	if citations := accumulator.citations(); citations != "" {
		if err := emit(map[string]any{"content": citations}, nil); err != nil {
			return err
		}
	}
	if err := emit(map[string]any{}, "stop"); err != nil {
		return err
	}
	return nil
}

func (p *Executor) emitPlainAnswer(prepared preparedTurn, accumulator *deepSeekStreamAccumulator, text string, streamID string) error {
	responseID := deepSeekResponseID()
	created := time.Now().Unix()
	emit := func(delta map[string]any, finish any) error {
		chunk := map[string]any{
			"id":      responseID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   prepared.model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		return p.emit(streamID, encoded)
	}

	first := map[string]any{"role": "assistant"}
	if thinking := accumulator.thinkingText(); thinking != "" {
		first["reasoning_content"] = thinking
	}
	if text != "" {
		first["content"] = text
	}
	if err := emit(first, nil); err != nil {
		return err
	}
	if err := emit(map[string]any{}, "stop"); err != nil {
		return err
	}
	return nil
}

func (p *Executor) emitToolCalls(prepared preparedTurn, accumulator *deepSeekStreamAccumulator, calls []deepSeekToolRequest, preamble string, streamID string) error {
	responseID := deepSeekResponseID()
	created := time.Now().Unix()

	emit := func(delta map[string]any, finish any) error {
		chunk := map[string]any{
			"id":      responseID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   prepared.model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		return p.emit(streamID, encoded)
	}

	first := map[string]any{"role": "assistant"}
	if thinking := accumulator.thinkingText(); thinking != "" {
		first["reasoning_content"] = thinking
	}
	if preamble != "" {
		first["content"] = preamble
	}
	if err := emit(first, nil); err != nil {
		return err
	}

	for index, call := range calls {
		delta := map[string]any{
			"tool_calls": []map[string]any{{
				"index": index,
				"id":    deepSeekToolCallID(call.Name),
				"type":  "function",
				"function": map[string]any{
					"name":      call.Name,
					"arguments": call.Arguments,
				},
			}},
		}
		if err := emit(delta, nil); err != nil {
			return err
		}
	}

	if err := emit(map[string]any{}, "tool_calls"); err != nil {
		return err
	}
	return nil
}

// collectTurn drives the stream to completion and returns one chat.completion.
func collectTurn(prepared preparedTurn, accumulator *deepSeekStreamAccumulator, body io.Reader) ([]byte, error) {
	if err := drainStream(body, accumulator, nil); err != nil {
		return nil, newHTTPError(http.StatusBadGateway, "deepseek-web: %v", err)
	}
	if enforcement := accumulator.enforcement; enforcement != nil {
		return nil, enforcement.error()
	}
	if !accumulator.sawFinished {
		return nil, newHTTPError(http.StatusBadGateway,
			"deepseek-web: stream ended before completion — the upstream session was cut off")
	}

	message := map[string]any{"role": "assistant", "content": accumulator.contentText()}
	if thinking := accumulator.thinkingText(); thinking != "" {
		message["reasoning_content"] = thinking
	}
	return marshalCompletion(prepared.model, message)
}

func toolCallMessage(calls []deepSeekToolRequest, preamble string) map[string]any {
	toolCalls := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		toolCalls = append(toolCalls, map[string]any{
			"id":   deepSeekToolCallID(call.Name),
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	message := map[string]any{"role": "assistant", "tool_calls": toolCalls}
	if preamble != "" {
		message["content"] = preamble
	}
	return message
}

func marshalCompletion(model string, message map[string]any) ([]byte, error) {
	body := map[string]any{
		"id":      deepSeekResponseID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": deepSeekFinishReason(message),
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("deepseek-web: marshal response: %w", err)
	}
	return encoded, nil
}

func deepSeekFinishReason(message map[string]any) string {
	if _, ok := message["tool_calls"]; ok {
		return "tool_calls"
	}
	return "stop"
}

// reportFailure returns the failure. StartStream forwards it through the host
// stream close callback, which turns it into a terminal error chunk; emitting an
// SSE error frame here would be double-framed by the host.
func (p *Executor) reportFailure(streaming bool, streamID string, err error) error {
	return err
}

func (p *Executor) emit(streamID string, payload []byte) error {
	if p == nil || p.Emit == nil || strings.TrimSpace(streamID) == "" {
		return nil
	}
	return p.Emit(streamID, payload)
}

func deepSeekResponseID() string {
	return "chatcmpl-deepseek-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// deepSeekEnforcementFromResponse reads a refusal out of a completion answer
// that is a JSON document rather than an event stream, putting the body back for
// the caller.
func deepSeekEnforcementFromResponse(response *http.Response) *deepSeekEnforcement {
	if response == nil || response.Body == nil {
		return nil
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, deepSeekMaxBodyBytes))
	_ = response.Body.Close()
	if err != nil || len(body) == 0 {
		response.Body = io.NopCloser(strings.NewReader(""))
		return nil
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return deepSeekEnforcementFromBody(body)
}

// clientSessionID names the client conversation this turn belongs to. The host
// exposes its own execution session id; the headers are a fallback for clients
// that name a conversation explicitly.
func clientSessionID(req pluginapi.ExecutorRequest) string {
	for _, key := range []string{"canonical_session_id", "execution_session_id", "derived_session_id", "session_id", "conversation_id", "sessionId"} {
		if value, ok := req.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	for _, header := range []string{"X-Session-Id", "X-Conversation-Id", "X-Session-Affinity"} {
		if value := strings.TrimSpace(req.Headers.Get(header)); value != "" {
			return value
		}
	}
	return ""
}

// deepSeekPause waits for a random part of the jitter window, or returns as soon
// as the caller's context ends.
func deepSeekPause(ctx context.Context, window time.Duration) {
	if window <= 0 {
		return
	}
	delay := time.Duration(rand.Int64N(int64(window)))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// deepSeekGate caps how many requests one account has in flight. A burst is what
// turns a busy account into a limited one, so over-cap turns wait instead of
// being sent.
type deepSeekGate struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

var deepSeekGates = &deepSeekGate{slots: make(map[string]chan struct{})}

func (g *deepSeekGate) acquire(ctx context.Context, key string, capacity int, wait time.Duration) (func(), error) {
	if capacity <= 0 || key == "" {
		return func() {}, nil
	}

	slots := g.slotsFor(key, capacity)

	select {
	case slots <- struct{}{}:
		return deepSeekGateRelease(slots), nil
	default:
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("%d requests already in flight for this account; waited %s", capacity, wait)
	}

	return deepSeekGateRelease(slots), nil
}

func deepSeekGateRelease(slots chan struct{}) func() {
	var once sync.Once
	return func() {
		once.Do(func() { <-slots })
	}
}

func (g *deepSeekGate) slotsFor(key string, capacity int) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	slots, ok := g.slots[key]
	if !ok {
		slots = make(chan struct{}, capacity)
		g.slots[key] = slots
	}
	return slots
}
