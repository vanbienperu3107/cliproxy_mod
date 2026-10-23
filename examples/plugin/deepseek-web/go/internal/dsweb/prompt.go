package dsweb

import (
	"encoding/json"
	"strings"
)

// The completion endpoint takes a single prompt string and no message array, so
// a whole conversation has to be folded into one document. Sending only the last
// user message makes an agent client lose the original task after a couple of
// tool-result turns, because only the newest line ever reaches the model.

type deepSeekMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

type deepSeekTurnLine struct {
	role string
	text string
}

const deepSeekDefaultAutoHistoryWindow = 20

// deepSeekMessageText extracts the plain text of one message. This format has no
// image or attachment slot, so only text parts survive.
func deepSeekMessageText(content json.RawMessage) string {
	var asString string
	if err := json.Unmarshal(content, &asString); err == nil {
		return asString
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		if part.Type == "text" && part.Text != "" {
			b.WriteString(part.Text)
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// deepSeekMessagesToPrompt renders an OpenAI conversation as the single prompt
// string the web API accepts.
//
// transcript forces the role-tagged form even for a single-turn conversation:
// session persistence needs the rendering of one conversation to have the same
// shape from turn to turn, or each prompt is a different document and none can
// be expressed as an extension of the previous one.
func deepSeekMessagesToPrompt(messages []deepSeekMessage, historyWindow int, transcript bool) string {
	if len(messages) == 0 {
		return ""
	}

	var systemParts []string
	var conversation []deepSeekTurnLine
	toolNames := make(map[string]string)
	lastUser := ""

	for _, message := range messages {
		text := strings.TrimSpace(deepSeekMessageText(message.Content))
		switch message.Role {
		case "system":
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user", "assistant":
			if message.Role == "user" {
				if text != "" {
					conversation = append(conversation, deepSeekTurnLine{role: "user", text: text})
				}
				lastUser = text
				deepSeekCollectToolNames(message.ToolCalls, toolNames)
				break
			}
			conversation = deepSeekAppendAssistant(conversation, text, message.ToolCalls)
			deepSeekCollectToolNames(message.ToolCalls, toolNames)
		case "tool":
			if text == "" {
				continue
			}
			name := message.Name
			if resolved, ok := toolNames[message.ToolCallID]; ok && resolved != "" {
				name = resolved
			}
			if name == "" {
				name = "tool"
			}
			conversation = append(conversation, deepSeekTurnLine{role: "tool", text: "(" + name + ") " + text})
		}
	}

	var parts []string
	if len(systemParts) > 0 {
		parts = append(parts, strings.Join(systemParts, "\n\n"))
	}

	if transcript {
		parts = append(parts, deepSeekRenderTranscript(conversation))
		return strings.Join(parts, "\n\n")
	}

	window := historyWindow
	if window <= 0 && len(conversation) > 1 {
		window = deepSeekDefaultAutoHistoryWindow
	}

	if window > 0 && len(conversation) > 1 {
		recent := conversation
		if len(recent) > window {
			recent = recent[len(recent)-window:]
		}
		parts = append(parts, deepSeekRenderTranscript(recent))
	} else if lastUser != "" {
		parts = append(parts, lastUser)
	}

	return strings.Join(parts, "\n\n")
}

func deepSeekRenderTranscript(turns []deepSeekTurnLine) string {
	lines := make([]string, 0, len(turns))
	for _, turn := range turns {
		switch turn.role {
		case "assistant":
			lines = append(lines, "Assistant: "+turn.text)
		case "tool":
			lines = append(lines, "Tool result "+turn.text)
		default:
			lines = append(lines, "User: "+turn.text)
		}
	}
	return strings.Join(lines, "\n\n")
}

type deepSeekToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func deepSeekCollectToolNames(toolCalls json.RawMessage, into map[string]string) {
	if len(toolCalls) == 0 {
		return
	}
	var calls []deepSeekToolCall
	if err := json.Unmarshal(toolCalls, &calls); err != nil {
		return
	}
	for _, call := range calls {
		if call.ID != "" && call.Function.Name != "" {
			into[call.ID] = call.Function.Name
		}
	}
}

// deepSeekRequestEnvelope is the subset of the inbound request body that changes
// what is sent upstream.
type deepSeekRequestEnvelope struct {
	Model    string            `json:"model"`
	Messages []deepSeekMessage `json:"messages"`
	// Tools never reach the upstream as a parameter, because this endpoint has
	// no field for them.
	Tools           []deepSeekToolSpec `json:"tools"`
	ThinkingEnabled bool               `json:"thinking_enabled"`
	Thinking        bool               `json:"thinking"`
	ReasoningEffort string             `json:"reasoning_effort"`
	SearchEnabled   bool               `json:"search_enabled"`
	Search          bool               `json:"search"`
	WebSearch       bool               `json:"web_search"`
}

type deepSeekModelOptions struct {
	modelType string
	thinking  bool
	search    bool
}

func resolveDeepSeekModelOptions(model string, body deepSeekRequestEnvelope) deepSeekModelOptions {
	name := strings.ToLower(model)
	options := deepSeekModelOptions{
		modelType: "default",
		thinking: strings.Contains(name, "r1") ||
			strings.Contains(name, "think") ||
			strings.Contains(name, "reason") ||
			body.ThinkingEnabled || body.Thinking || body.ReasoningEffort != "",
		search: strings.Contains(name, "search") ||
			body.SearchEnabled || body.Search || body.WebSearch,
	}
	if strings.Contains(name, "pro") || strings.Contains(name, "expert") {
		options.modelType = "expert"
	}
	return options
}

// deepSeekRenderToolCalls renders an assistant turn's tool_calls for the
// transcript: the web API has no field for them, so the model only knows it
// already made a call by reading it back as text.
func deepSeekRenderToolCalls(toolCalls json.RawMessage) string {
	if len(toolCalls) == 0 {
		return ""
	}
	var calls []deepSeekToolCall
	if err := json.Unmarshal(toolCalls, &calls); err != nil {
		return ""
	}
	lines := make([]string, 0, len(calls))
	for _, call := range calls {
		if call.Function.Name == "" {
			continue
		}
		line := "[called " + call.Function.Name
		if call.Function.Arguments != "" {
			line += " with " + call.Function.Arguments
		}
		lines = append(lines, line+"]")
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// deepSeekAppendAssistant records an assistant turn, repairing one that the
// extractor let through as text: replaying unparsed call markup teaches the
// model that markup is how this conversation calls tools.
func deepSeekAppendAssistant(conversation []deepSeekTurnLine, text string, toolCalls json.RawMessage) []deepSeekTurnLine {
	call := deepSeekRenderToolCalls(toolCalls)

	kept := deepSeekDsmlStripped(text)
	for _, called := range deepSeekFindToolCallObjects(kept) {
		kept = deepSeekRemoveObject(kept, called)
	}

	if call != "" {
		if kept != "" {
			conversation = append(conversation, deepSeekTurnLine{role: "assistant", text: kept})
		}
		return append(conversation, deepSeekTurnLine{role: "assistant", text: call})
	}
	if kept != "" {
		return append(conversation, deepSeekTurnLine{role: "assistant", text: kept})
	}
	if summary := deepSeekMarkupCallSummary(text); summary != "" {
		return append(conversation, deepSeekTurnLine{role: "assistant", text: summary})
	}
	return conversation
}

func deepSeekFindToolCallObjects(text string) []string {
	var objects []string
	for _, obj := range deepSeekFindJSONObjects(text) {
		var call struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"arguments"`
			Para json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal([]byte(obj), &call); err != nil || call.Name == "" {
			continue
		}
		if len(call.Args) == 0 && len(call.Para) == 0 {
			continue
		}
		objects = append(objects, obj)
	}
	return objects
}

func deepSeekRemoveObject(text, obj string) string {
	return strings.TrimSpace(strings.Replace(text, obj, "", 1))
}

func deepSeekMarkupCallSummary(text string) string {
	names := make([]string, 0, 2)
	for _, invoke := range deepSeekDSMLInvokes(text) {
		attributes, _, _ := strings.Cut(invoke, ">")
		if name := deepSeekDSMLAttribute(attributes, "name"); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "[called " + strings.Join(names, ", ") + "]"
}
