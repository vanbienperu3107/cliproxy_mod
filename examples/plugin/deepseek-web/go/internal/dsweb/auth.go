package dsweb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// Provider is the auth and model provider key this plugin registers.
	Provider = "deepseek-web"

	defaultMaxConcurrency = 2
	defaultJitter         = 400 * time.Millisecond
)

// Credentials is the provider-owned auth material, parsed from the auth file.
type Credentials struct {
	UserToken string `json:"userToken"`
	Cookie    string `json:"cookie,omitempty"`
	BaseURL   string `json:"baseUrl,omitempty"`

	// PersistSession pins one upstream chat session per client conversation
	// and sends only the new tail of the prompt. Disabling it re-sends the
	// whole transcript every turn, which grows quadratically upstream.
	PersistSession bool `json:"persistSession"`

	// HistoryWindow bounds the replayed transcript when persistence is off.
	// Zero means the built-in default.
	HistoryWindow int `json:"historyWindow,omitempty"`

	MaxConcurrency int `json:"maxConcurrency,omitempty"`
	JitterMS       int `json:"jitterMs,omitempty"`
}

func (c Credentials) withDefaults() Credentials {
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = defaultMaxConcurrency
	}
	if c.JitterMS < 0 {
		c.JitterMS = 0
	}
	if c.JitterMS == 0 {
		c.JitterMS = int(defaultJitter / time.Millisecond)
	}
	return c
}

func (c Credentials) jitter() time.Duration {
	if c.JitterMS <= 0 {
		return 0
	}
	return time.Duration(c.JitterMS) * time.Millisecond
}

// ParseCredentials reads the provider-owned storage blob. Unknown keys are
// ignored, so a host that adds bookkeeping fields does not break the plugin.
func ParseCredentials(storage []byte) (Credentials, error) {
	if len(strings.TrimSpace(string(storage))) == 0 {
		return Credentials{}, newHTTPError(http.StatusUnauthorized, "deepseek-web: auth storage is empty")
	}

	var raw map[string]any
	if err := json.Unmarshal(storage, &raw); err != nil {
		return Credentials{}, newHTTPError(http.StatusUnauthorized, "deepseek-web: auth storage is not valid JSON")
	}

	creds := Credentials{
		UserToken: deepSeekUserToken(firstString(raw, "userToken", "user_token", "apiKey", "api_key", "token", "value")),
		Cookie:    firstString(raw, "cookie", "cookies"),
		BaseURL:   firstString(raw, "baseUrl", "base_url", "baseURL"),
	}
	if persist, ok := raw["persistSession"].(bool); ok {
		creds.PersistSession = persist
	} else {
		creds.PersistSession = true
	}
	creds.HistoryWindow = firstInt(raw, "historyWindow", "history_window")
	creds.MaxConcurrency = firstInt(raw, "maxConcurrency", "max_concurrency")
	creds.JitterMS = firstInt(raw, "jitterMs", "jitter_ms")

	if creds.UserToken == "" {
		return Credentials{}, newHTTPError(http.StatusUnauthorized,
			"deepseek-web: no userToken in auth storage — copy it from chat.deepseek.com localStorage")
	}
	return creds.withDefaults(), nil
}

func firstString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := raw[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstInt(raw map[string]any, keys ...string) int {
	for _, key := range keys {
		switch value := raw[key].(type) {
		case float64:
			return int(value)
		case int:
			return value
		case json.Number:
			if parsed, err := value.Int64(); err == nil {
				return int(parsed)
			}
		}
	}
	return 0
}

// ParseAuth recognises a DeepSeek web credential and normalizes it. The raw
// auth file may be flat ({"type":"deepseek-web","userToken":"..."}) or the
// localStorage shape ({"value":"..."}).
func ParseAuth(req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	var raw map[string]any
	if err := json.Unmarshal(req.RawJSON, &raw); err != nil {
		return pluginapi.AuthParseResponse{}, nil
	}

	provider := strings.ToLower(strings.TrimSpace(firstString(raw, "type", "provider")))
	declared := provider == Provider
	token := deepSeekUserToken(firstString(raw, "userToken", "user_token", "apiKey", "api_key", "token", "value"))
	if !declared && token == "" {
		return pluginapi.AuthParseResponse{}, nil
	}
	if token == "" {
		return pluginapi.AuthParseResponse{Handled: true}, newHTTPError(http.StatusBadRequest,
			"deepseek-web: auth file has no userToken")
	}

	// Normalize the stored payload so the executor always reads the same keys,
	// whatever shape the operator pasted.
	raw["type"] = Provider
	raw["userToken"] = token
	delete(raw, "apiKey")
	delete(raw, "api_key")

	storage, err := json.Marshal(raw)
	if err != nil {
		return pluginapi.AuthParseResponse{Handled: true}, err
	}

	label := firstString(raw, "label", "email", "mobile")
	if label == "" {
		label = "DeepSeek Web " + tokenSuffix(token)
	}
	metadata := map[string]any{"type": Provider}
	if email := firstString(raw, "email"); email != "" {
		metadata["email"] = email
	}

	auth := pluginapi.AuthData{
		Provider:    Provider,
		ID:          authID(req, raw, token),
		FileName:    req.FileName,
		Label:       label,
		StorageJSON: storage,
		Metadata:    metadata,
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, nil
}

func authID(req pluginapi.AuthParseRequest, raw map[string]any, token string) string {
	if id := firstString(raw, "id", "authID", "auth_id"); id != "" {
		return id
	}
	if req.FileName != "" {
		return strings.TrimSuffix(req.FileName, ".json")
	}
	sum := sha256.Sum256([]byte(token))
	return Provider + "-" + hex.EncodeToString(sum[:4])
}

func tokenSuffix(token string) string {
	if len(token) <= 4 {
		return "token"
	}
	return token[len(token)-4:]
}

// RefreshAuth re-validates the stored credential by exchanging it for an
// access token. The site's access token lives an hour, so the host is told to
// refresh again after that.
func RefreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	creds, err := ParseCredentials(req.StorageJSON)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}

	deepSeekTokens.drop(creds.UserToken)
	turn := newDeepSeekTurn(&http.Client{}, creds.BaseURL, creds.Cookie)
	if _, err := turn.acquireAccessToken(ctx, creds.UserToken); err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}

	auth := pluginapi.AuthData{
		Provider:         Provider,
		ID:               req.AuthID,
		FileName:         firstString(req.Metadata, "file_name"),
		Label:            firstString(req.Metadata, "label"),
		StorageJSON:      req.StorageJSON,
		Metadata:         map[string]any{"type": Provider},
		NextRefreshAfter: time.Now().Add(deepSeekAccessTokenTTL),
	}
	return pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: auth.NextRefreshAfter}, nil
}

// StartLogin opens the site's sign-in page. The credential itself is a
// browser-login token that cannot be minted without a browser, so the flow
// tells the operator where to copy it from instead of pretending to poll.
func StartLogin(req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{
		Provider:  Provider,
		URL:       deepSeekWebBase + "/sign_in",
		State:     "paste-user-token",
		ExpiresAt: time.Now().Add(15 * time.Minute),
		Metadata: map[string]any{
			"instructions": "Sign in, open DevTools > Application > Local Storage > https://chat.deepseek.com, " +
				"copy the value of the key whose value contains a userToken, and save it as an auth JSON file: " +
				`{"type":"deepseek-web","userToken":"<token>"}`,
		},
	}, nil
}

func PollLogin(req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusError,
		Message: "deepseek-web login is a manual paste: save an auth file with type deepseek-web and your userToken",
	}, nil
}
