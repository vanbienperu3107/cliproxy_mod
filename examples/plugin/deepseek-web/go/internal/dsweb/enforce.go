package dsweb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Account-level enforcement: the way this site says "not this account, not
// now". Everything arrives as HTTP 200 with the refusal in the payload, so a
// reader that trusts the status reads a muted account as a cut-off session and
// the router keeps sending turns to it — which is how an account that was
// briefly muted earns a longer mute.

type deepSeekEnforcementKind string

const (
	deepSeekMuted          deepSeekEnforcementKind = "muted"
	deepSeekBanned         deepSeekEnforcementKind = "banned"
	deepSeekIPRestricted   deepSeekEnforcementKind = "ip_restricted"
	deepSeekRateLimited    deepSeekEnforcementKind = "rate_limited"
	deepSeekClientRejected deepSeekEnforcementKind = "client_rejected"
	deepSeekEnforced       deepSeekEnforcementKind = "enforced"
)

type deepSeekEnforcement struct {
	Kind       deepSeekEnforcementKind
	Message    string
	retryAfter time.Duration
}

func (e *deepSeekEnforcement) status() int {
	switch e.Kind {
	case deepSeekMuted, deepSeekRateLimited:
		return http.StatusTooManyRequests
	case deepSeekBanned, deepSeekIPRestricted:
		return http.StatusForbidden
	default:
		return http.StatusBadGateway
	}
}

func (e *deepSeekEnforcement) error() error {
	message := "deepseek-web: " + e.Message
	if e.retryAfter > 0 {
		message += " (available again after " + e.retryAfter.Round(time.Second).String() + ")"
	}
	return &HTTPError{Status: e.status(), Message: message, RetryAfter: e.retryAfter}
}

// deepSeekEnforcementFromBody reads a refusal out of a non-streamed 200 body.
func deepSeekEnforcementFromBody(body []byte) *deepSeekEnforcement {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil
	}

	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			BizCode int    `json:"biz_code"`
			BizMsg  string `json:"biz_msg"`
			BizData struct {
				IsMuted   int     `json:"is_muted"`
				MuteUntil float64 `json:"mute_until"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return nil
	}

	if muted, ok := deepSeekMuteFromEnvelope(envelope.Data.BizCode, envelope.Data.BizMsg,
		envelope.Data.BizData.IsMuted, envelope.Data.BizData.MuteUntil); ok {
		return muted
	}
	if envelope.Code != 0 {
		return deepSeekEnforcementForCode(envelope.Code, envelope.Msg)
	}
	return nil
}

func deepSeekEnforcementForCode(code int, message string) *deepSeekEnforcement {
	switch code {
	case deepSeekCodeAuthFailed, deepSeekCodeMissingToken:
		return nil
	case deepSeekCodeRequestEncrypted:
		return &deepSeekEnforcement{
			Kind:    deepSeekEnforced,
			Message: "the upstream refused the request body: " + message,
		}
	case deepSeekCodeUserBanned:
		return &deepSeekEnforcement{
			Kind:    deepSeekBanned,
			Message: "the upstream refused this account: " + message,
		}
	case deepSeekCodeIPRestricted:
		return &deepSeekEnforcement{
			Kind:    deepSeekIPRestricted,
			Message: "the upstream refused this address: " + message,
		}
	case deepSeekCodeMuted:
		return &deepSeekEnforcement{
			Kind:    deepSeekMuted,
			Message: "the upstream muted this account: " + message,
		}
	default:
		return &deepSeekEnforcement{
			Kind:    deepSeekEnforced,
			Message: fmt.Sprintf("upstream refused the turn (code %d): %s", code, message),
		}
	}
}

func deepSeekMuteFromEnvelope(bizCode int, bizMsg string, isMuted int, muteUntil float64) (*deepSeekEnforcement, bool) {
	muted := bizCode == deepSeekMuteBizCode || isMuted == 1 || strings.Contains(strings.ToLower(bizMsg), "mute")
	if !muted {
		return nil, false
	}

	message := bizMsg
	if message == "" {
		message = "account is muted by the upstream"
	}
	enforcement := &deepSeekEnforcement{Kind: deepSeekMuted, Message: message}
	if muteUntil > 0 {
		if until := time.Unix(int64(muteUntil), 0); until.After(time.Now()) {
			enforcement.retryAfter = time.Until(until)
			enforcement.Message = message + " until " + until.UTC().Format(time.RFC3339)
		}
	} else {
		enforcement.Message = message + " (no release time given)"
	}
	return enforcement, true
}

const deepSeekMuteBizCode = 5

// deepSeekEnforcementFromEvent reads a refusal out of one stream event payload.
func deepSeekEnforcementFromEvent(payload []byte) *deepSeekEnforcement {
	var envelope struct {
		Type         string `json:"type"`
		Content      string `json:"content"`
		Msg          string `json:"msg"`
		FinishReason string `json:"finish_reason"`
		V            *struct {
			Type         string `json:"type"`
			Content      string `json:"content"`
			Msg          string `json:"msg"`
			FinishReason string `json:"finish_reason"`
		} `json:"v"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil
	}
	if !strings.EqualFold(envelope.Type, "error") && envelope.V != nil {
		envelope.Type = envelope.V.Type
		envelope.Content = envelope.V.Content
		envelope.Msg = envelope.V.Msg
		envelope.FinishReason = envelope.V.FinishReason
	}
	if !strings.EqualFold(envelope.Type, "error") {
		return nil
	}

	message := envelope.Content
	if message == "" {
		message = envelope.Msg
	}
	if message == "" {
		message = "upstream refused the turn"
	}

	kind := deepSeekEnforced
	switch {
	case strings.Contains(envelope.FinishReason, "rate_limit"):
		kind = deepSeekRateLimited
	case strings.Contains(strings.ToLower(message), "频繁"):
		kind = deepSeekRateLimited
	case strings.Contains(envelope.FinishReason, "unsupported_client"):
		kind = deepSeekClientRejected
	}
	return &deepSeekEnforcement{Kind: kind, Message: message + " (" + envelope.FinishReason + ")"}
}
