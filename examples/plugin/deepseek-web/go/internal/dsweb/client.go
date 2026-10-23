package dsweb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	deepSeekWebBase        = "https://chat.deepseek.com"
	deepSeekCompletionPath = "/api/v0/chat/completion"
	deepSeekAccessTokenTTL = time.Hour
	deepSeekPoWTimeout     = 30 * time.Second
	deepSeekMaxBodyBytes   = 16 << 20
	deepSeekPowAlgorithm   = "DeepSeekHashV1"

	deepSeekCodeAuthFailed       = 40003
	deepSeekCodeMissingToken     = 40002
	deepSeekCodeUserBanned       = 40012
	deepSeekCodeIPRestricted     = 40029
	deepSeekCodeMuted            = 50006
	deepSeekCodeRequestEncrypted = 40006
)

// deepSeekTurn is the per-request state for one completion: where this
// credential's endpoints are, which client dials them, and how closely the
// request imitates the site's own client.
type deepSeekTurn struct {
	client    *http.Client
	endpoints deepSeekEndpoints
	origin    string
	cookie    string
}

func newDeepSeekTurn(client *http.Client, baseURL string, cookie string) deepSeekTurn {
	if client == nil {
		client = http.DefaultClient
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = deepSeekWebBase + deepSeekCompletionPath
	}
	return deepSeekTurn{
		client:    client,
		endpoints: deepSeekEndpointsFor(baseURL),
		origin:    deepSeekOrigin(baseURL),
		cookie:    strings.TrimSpace(cookie),
	}
}

func (t deepSeekTurn) webHeaders(extra map[string]string) map[string]string {
	headers := deepSeekBrowserHeaders(t.origin)
	for k, v := range extra {
		headers[k] = v
	}
	if t.cookie != "" {
		if _, ok := headers["cookie"]; !ok {
			headers["cookie"] = t.cookie
		}
	}
	return headers
}

func deepSeekBrowserHeaders(origin string) map[string]string {
	return map[string]string{
		"accept":          "*/*",
		"accept-encoding": "gzip, deflate, br, zstd",
		"accept-language": "en-US,en;q=0.9",
		"content-type":    "application/json",
		"referer":         origin + "/sign_in",
		"sec-fetch-dest":  "empty",
		"sec-fetch-mode":  "cors",
		"sec-fetch-site":  "same-origin",
		"user-agent":      deepSeekWebUserAgent,
		"priority":        "u=1, i",
	}
}

const deepSeekWebUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

type deepSeekTokenCache struct {
	mu    sync.RWMutex
	items map[string]deepSeekTokenEntry
}

type deepSeekTokenEntry struct {
	accessToken string
	expiresAt   time.Time
}

const maxDeepSeekCachedTokens = 100

var deepSeekTokens = &deepSeekTokenCache{items: make(map[string]deepSeekTokenEntry)}

func (c *deepSeekTokenCache) get(userToken string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.items[userToken]
	if !ok || !time.Now().Before(entry.expiresAt) {
		return "", false
	}
	return entry.accessToken, true
}

func (c *deepSeekTokenCache) put(userToken, accessToken string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[userToken]; !exists && len(c.items) >= maxDeepSeekCachedTokens {
		c.evictOldestLocked()
	}
	c.items[userToken] = deepSeekTokenEntry{
		accessToken: accessToken,
		expiresAt:   time.Now().Add(deepSeekAccessTokenTTL),
	}
}

func (c *deepSeekTokenCache) drop(userToken string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, userToken)
}

func (c *deepSeekTokenCache) evictOldestLocked() {
	var oldestKey string
	var oldestExpiry time.Time
	for key, entry := range c.items {
		if oldestKey == "" || entry.expiresAt.Before(oldestExpiry) {
			oldestKey, oldestExpiry = key, entry.expiresAt
		}
	}
	if oldestKey != "" {
		delete(c.items, oldestKey)
	}
}

type deepSeekEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type deepSeekBizData struct {
	Token       string               `json:"token"`
	ChatSession deepSeekChatSession  `json:"chat_session"`
	Challenge   deepSeekPowChallenge `json:"challenge"`
}

type deepSeekChatSession struct {
	ID string `json:"id"`
}

type deepSeekPowChallenge struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	Difficulty int    `json:"difficulty"`
	ExpireAt   int64  `json:"expire_at"`
	Signature  string `json:"signature"`
	TargetPath string `json:"target_path"`
}

// deepSeekAPIError is a DeepSeek application-level failure. Auth and rate
// limiting both arrive as HTTP 200 with a code in the body.
type deepSeekAPIError struct {
	Code    int
	Message string
}

func (e *deepSeekAPIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("deepseek-web: error %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("deepseek-web: error %d", e.Code)
}

func deepSeekUserToken(raw string) string {
	token := strings.TrimSpace(raw)
	if token == "" {
		return ""
	}
	var wrapper struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(token), &wrapper); err == nil && wrapper.Value != "" {
		return strings.TrimSpace(wrapper.Value)
	}
	return token
}

func (t deepSeekTurn) do(ctx context.Context, method, url, bearer string, headers map[string]string, payload any) (json.RawMessage, error) {
	var body []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("deepseek-web: marshal %s request: %w", url, err)
		}
		body = encoded
	}

	allHeaders := t.webHeaders(headers)
	if bearer != "" {
		allHeaders["authorization"] = "Bearer " + bearer
	}
	// The site marks a POST as cross-origin and leaves a GET unmarked.
	if method == http.MethodPost {
		allHeaders["origin"] = t.origin
	} else {
		delete(allHeaders, "origin")
	}

	resp, err := t.send(ctx, method, url, body, allHeaders)
	if err != nil {
		return nil, fmt.Errorf("deepseek-web: %s: %w", url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, deepSeekMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("deepseek-web: read %s: %w", url, err)
	}

	var envelope deepSeekEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("deepseek-web: %s returned an unparseable body (HTTP %d): %w", url, resp.StatusCode, err)
	}
	if envelope.Code != 0 {
		return nil, &deepSeekAPIError{Code: envelope.Code, Message: envelope.Msg}
	}
	return envelope.Data, nil
}

func (t deepSeekTurn) send(ctx context.Context, method, url string, body []byte, headers map[string]string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}

	errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, &HTTPError{
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("deepseek-web: HTTP %d (body unreadable: %v)", resp.StatusCode, readErr),
		}
	}
	return nil, &HTTPError{
		Status:  resp.StatusCode,
		Message: fmt.Sprintf("deepseek-web: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody))),
	}
}

func deepSeekBizDataOf(data json.RawMessage) (*deepSeekBizData, error) {
	var holder struct {
		BizData *deepSeekBizData `json:"biz_data"`
	}
	if err := json.Unmarshal(data, &holder); err == nil && holder.BizData != nil {
		return holder.BizData, nil
	}
	var direct deepSeekBizData
	if err := json.Unmarshal(data, &direct); err != nil {
		return nil, fmt.Errorf("deepseek-web: unwrap data: %w", err)
	}
	return &direct, nil
}

func (t deepSeekTurn) acquireAccessToken(ctx context.Context, userToken string) (string, error) {
	if cached, ok := deepSeekTokens.get(userToken); ok {
		return cached, nil
	}

	data, err := t.do(ctx, http.MethodGet, t.endpoints.usersCurrent, userToken, nil, nil)
	if err != nil {
		if isDeepSeekAuthError(err) {
			deepSeekTokens.drop(userToken)
			return "", &HTTPError{
				Status:  http.StatusUnauthorized,
				Message: "deepseek-web: userToken is invalid or expired — get a fresh one from chat.deepseek.com localStorage",
			}
		}
		return "", err
	}

	biz, err := deepSeekBizDataOf(data)
	if err != nil {
		return "", err
	}
	if biz.Token == "" {
		return "", fmt.Errorf("deepseek-web: users/current returned no access token")
	}

	deepSeekTokens.put(userToken, biz.Token)
	return biz.Token, nil
}

func (t deepSeekTurn) createSession(ctx context.Context, accessToken string) (string, error) {
	data, err := t.do(ctx, http.MethodPost, t.endpoints.sessionCreate, accessToken, nil, map[string]any{})
	if err != nil {
		return "", err
	}
	biz, err := deepSeekBizDataOf(data)
	if err != nil {
		return "", err
	}
	if biz.ChatSession.ID == "" {
		return "", fmt.Errorf("deepseek-web: chat_session/create returned no session id")
	}
	return biz.ChatSession.ID, nil
}

// deleteSession closes an upstream chat session. Best effort by design: a
// failure here must not fail a turn that already produced an answer.
func (t deepSeekTurn) deleteSession(ctx context.Context, accessToken, sessionID string) {
	if sessionID == "" {
		return
	}
	payload := map[string]any{"chat_session_id": sessionID}
	_, _ = t.do(ctx, http.MethodPost, t.endpoints.sessionDelete, accessToken, nil, payload)
}

type deepSeekPowResponse struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	Answer     int    `json:"answer"`
	Signature  string `json:"signature"`
	TargetPath string `json:"target_path"`
}

func solveDeepSeekPow(ctx context.Context, challenge deepSeekPowChallenge) (string, error) {
	if challenge.Algorithm != deepSeekPowAlgorithm {
		return "", fmt.Errorf("deepseek-web: unsupported pow algorithm %q", challenge.Algorithm)
	}

	prefix := challenge.Salt + "_" + strconv.FormatInt(challenge.ExpireAt, 10) + "_"
	nonce, err := findPowNonce(ctx, prefix, challenge.Challenge, challenge.Difficulty, deepSeekPoWTimeout)
	if err != nil {
		return "", err
	}
	if nonce < 0 {
		return "", fmt.Errorf("deepseek-web: no proof-of-work nonce within %d candidates", challenge.Difficulty)
	}

	encoded, err := json.Marshal(deepSeekPowResponse{
		Algorithm:  challenge.Algorithm,
		Challenge:  challenge.Challenge,
		Salt:       challenge.Salt,
		Answer:     nonce,
		Signature:  challenge.Signature,
		TargetPath: challenge.TargetPath,
	})
	if err != nil {
		return "", fmt.Errorf("deepseek-web: marshal pow response: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encoded), nil
}

// requestPow fetches and answers a fresh challenge for one completion attempt.
// The challenge is single-use upstream, so every attempt needs a new one.
func (t deepSeekTurn) requestPow(ctx context.Context, accessToken string) (string, error) {
	payload := map[string]any{"target_path": deepSeekCompletionPath}
	data, err := t.do(ctx, http.MethodPost, t.endpoints.powChallenge, accessToken, nil, payload)
	if err != nil {
		return "", err
	}
	biz, err := deepSeekBizDataOf(data)
	if err != nil {
		return "", err
	}
	if biz.Challenge.Challenge == "" {
		return "", fmt.Errorf("deepseek-web: create_pow_challenge returned no challenge")
	}
	return solveDeepSeekPow(ctx, biz.Challenge)
}

// deepSeekCompletionRequest is the payload of one completion call.
//
// parent_message_id is an integer, not a string: it is the upstream's own
// message id, and the first turn sends null.
type deepSeekCompletionRequest struct {
	ChatSessionID   string   `json:"chat_session_id"`
	ParentMessageID *int64   `json:"parent_message_id"`
	ModelType       string   `json:"model_type"`
	Prompt          string   `json:"prompt"`
	RefFileIDs      []string `json:"ref_file_ids"`
	ThinkingEnabled bool     `json:"thinking_enabled"`
	SearchEnabled   bool     `json:"search_enabled"`
	Stream          bool     `json:"stream"`
	Preempt         bool     `json:"preempt"`
}

// completion performs one completion attempt against a session and returns the
// still-open response body.
func (t deepSeekTurn) completion(ctx context.Context, accessToken string, request deepSeekCompletionRequest) (*http.Response, error) {
	powAnswer, err := t.requestPow(ctx, accessToken)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("deepseek-web: marshal completion: %w", err)
	}

	allHeaders := t.webHeaders(map[string]string{
		"x-ds-pow-response": powAnswer,
	})
	allHeaders["authorization"] = "Bearer " + accessToken
	allHeaders["origin"] = t.origin

	resp, err := t.send(ctx, http.MethodPost, t.endpoints.completion, payload, allHeaders)
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) &&
			(httpErr.Status == http.StatusUnauthorized || httpErr.Status == http.StatusForbidden) {
			return nil, &HTTPError{
				Status:  http.StatusUnauthorized,
				Message: "deepseek-web: access token expired — re-paste your userToken from chat.deepseek.com localStorage",
			}
		}
		return nil, fmt.Errorf("deepseek-web: completion: %w", err)
	}
	return resp, nil
}

func isDeepSeekAuthError(err error) bool {
	var apiErr *deepSeekAPIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == deepSeekCodeAuthFailed
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == http.StatusUnauthorized || httpErr.Status == http.StatusForbidden
	}
	return false
}
