package dsweb

import (
	"encoding/json"
	"strings"
	"testing"
)

func message(role, content string) deepSeekMessage {
	raw, _ := json.Marshal(content)
	return deepSeekMessage{Role: role, Content: raw}
}

func TestMessagesToPromptSingleTurnIsJustTheQuestion(t *testing.T) {
	got := deepSeekMessagesToPrompt([]deepSeekMessage{message("user", "hello")}, 0, false)
	if got != "hello" {
		t.Fatalf("prompt = %q, want %q", got, "hello")
	}
}

func TestMessagesToPromptTranscriptKeepsEveryRole(t *testing.T) {
	messages := []deepSeekMessage{
		message("system", "be terse"),
		message("user", "weather?"),
		message("assistant", "checking"),
		message("tool", "22C"),
	}
	got := deepSeekMessagesToPrompt(messages, 0, true)

	for _, want := range []string{"be terse", "User: weather?", "Assistant: checking", "Tool result (tool) 22C"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestMessagesToPromptHonoursHistoryWindow(t *testing.T) {
	messages := []deepSeekMessage{
		message("user", "one"),
		message("assistant", "two"),
		message("user", "three"),
		message("assistant", "four"),
	}
	got := deepSeekMessagesToPrompt(messages, 2, false)
	if strings.Contains(got, "one") || strings.Contains(got, "two") {
		t.Fatalf("prompt kept turns outside the window:\n%s", got)
	}
	if !strings.Contains(got, "three") || !strings.Contains(got, "four") {
		t.Fatalf("prompt dropped turns inside the window:\n%s", got)
	}
}

func TestRenderToolsFoldsDeclarationsIntoThePrompt(t *testing.T) {
	tools := []deepSeekToolSpec{toolSpec("get_weather", `{"type":"object"}`)}
	rendered := deepSeekRenderTools(tools)
	if !strings.Contains(rendered, "get_weather") || !strings.Contains(rendered, "<tools>") {
		t.Fatalf("tool declaration missing from prompt:\n%s", rendered)
	}
}

func TestResolveModelOptions(t *testing.T) {
	tests := []struct {
		model        string
		thinking     bool
		search       bool
		wantModelTyp string
	}{
		{"deepseek-v4-pro", false, false, "expert"},
		{"deepseek-v4-pro-think-search", true, true, "expert"},
		{"deepseek-v4-flash", false, false, "default"},
		{"deepseek-reasoner", true, false, "default"},
	}
	for _, tc := range tests {
		got := resolveDeepSeekModelOptions(tc.model, deepSeekRequestEnvelope{})
		if got.thinking != tc.thinking || got.search != tc.search || got.modelType != tc.wantModelTyp {
			t.Fatalf("%s => %+v, want thinking=%v search=%v type=%s", tc.model, got, tc.thinking, tc.search, tc.wantModelTyp)
		}
	}
}

func TestResolveModelOptionsFromBody(t *testing.T) {
	got := resolveDeepSeekModelOptions("deepseek-v4-flash", deepSeekRequestEnvelope{
		ThinkingEnabled: true,
		WebSearch:       true,
	})
	if !got.thinking || !got.search {
		t.Fatalf("body flags ignored: %+v", got)
	}
}

func toolSpec(name, parameters string) deepSeekToolSpec {
	spec := deepSeekToolSpec{Type: "function"}
	spec.Function.Name = name
	spec.Function.Parameters = json.RawMessage(parameters)
	return spec
}
