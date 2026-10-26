package dsweb

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
)

// Tool calling on this upstream is a convention, not a protocol feature: there
// is no `tools[]` parameter on the completion endpoint and no tool-call topic in
// the event stream. A model told a tool exists answers by typing a JSON object
// (or a DSML block) through ordinary fragment appends. Everything here exists
// because of that.

type deepSeekToolSpec struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// UnmarshalJSON accepts both the OpenAI wrapper and the flat Claude spelling.
func (s *deepSeekToolSpec) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("deepseek-web: parse tool declaration: %w", err)
	}

	s.Type = raw.Type
	s.Function.Name = raw.Function.Name
	s.Function.Description = raw.Function.Description
	s.Function.Parameters = raw.Function.Parameters

	if s.Function.Name == "" {
		s.Function.Name = raw.Name
	}
	if s.Function.Description == "" {
		s.Function.Description = raw.Description
	}
	if len(s.Function.Parameters) == 0 {
		s.Function.Parameters = raw.InputSchema
	}
	if len(s.Function.Parameters) == 0 {
		s.Function.Parameters = raw.Parameters
	}
	return nil
}

type deepSeekToolRequest struct {
	Name      string
	Arguments string
	Raw       string
}

func deepSeekSchemaText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return `{"type":"object","properties":{}}`
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if trimmed := strings.TrimSpace(asString); deepSeekValidJSON(trimmed) {
			return trimmed
		}
		return `{"type":"object","properties":{}}`
	}
	if deepSeekValidJSON(string(raw)) {
		return string(raw)
	}
	return `{"type":"object","properties":{}}`
}

func deepSeekRenderTools(tools []deepSeekToolSpec) string {
	var b strings.Builder
	b.WriteString("You have access to the following tools.\n\n<tools>\n")
	for _, tool := range tools {
		if tool.Function.Name == "" {
			continue
		}
		fmt.Fprintf(&b, "{\"name\": %q, \"description\": %q, \"parameters\": %s}\n",
			tool.Function.Name, tool.Function.Description,
			deepSeekSchemaText(tool.Function.Parameters))
	}
	b.WriteString("</tools>\n\n")
	b.WriteString("To use a tool, reply with a single JSON object and nothing else: " +
		`{"name": "<tool name>", "arguments": {<arguments matching that tool's parameters>}}` + ". " +
		"Do not wrap it in prose or a code fence.")
	b.WriteString("\n\nThe tool syntax above is the only one this client understands. " +
		"Native markup is not transmitted: a reply made of `invoke`, `parameter` or " +
		"`calls` elements — including any spelled with a DSML namespace — is discarded " +
		"unread, and you will appear to have answered nothing.\n")
	b.WriteString("\n\nRules for using tools:\n")
	b.WriteString("- The tools above are installed and runnable. You can use them; a " +
		"request is not outside your abilities because it needs one.\n")
	b.WriteString("- Never answer that you lack a capability the list above provides. " +
		"Call the tool instead.\n")
	b.WriteString("- If a request needs information you do not have, call a tool that " +
		"can fetch it before answering.\n")
	b.WriteString("- A tool that runs commands executes on a real machine with network " +
		"access. \"I have no internet access\" is not a reason to refuse while such " +
		"a tool is declared — the tool has it, even when you do not.\n")
	b.WriteString("- A line reading \"[called <name> with <arguments>]\" means you already " +
		"made that call and its result is the following \"Tool result\" line. Do not repeat it.\n")
	b.WriteString("- Once you have a tool result for what was asked, answer the user in plain " +
		"text. Calling a tool again for the same information is an error.\n")
	b.WriteString("- A \"Tool result\" line is your only source of fact. Never state a " +
		"figure, date, temperature, price or count that is not in one of those lines, " +
		"and never present invented data as though a tool returned it.\n")
	b.WriteString("- When a tool result is empty, an error, or says nothing was found, " +
		"say so plainly. Do not substitute what you recall, and do not describe output " +
		"that was not returned.\n")
	b.WriteString("- Only call a tool when you actually need data you do not already have.\n")
	b.WriteString("- If several calls are needed, put them all in one reply rather than " +
		"answering one at a time.\n")
	return b.String()
}

// deepSeekExtractToolCalls recovers every tool call from the model's answer.
// Every call must name one of the tools the client declared, in either dialect.
func deepSeekExtractToolCalls(text string, tools []deepSeekToolSpec) []deepSeekToolRequest {
	raw := strings.TrimSpace(text)
	if deepSeekHasDSML(raw) {
		return deepSeekExtractDSML(raw, tools)
	}
	return deepSeekJSONToolCalls(raw, tools)
}

func deepSeekJSONToolCalls(answer string, tools []deepSeekToolSpec) []deepSeekToolRequest {
	objects := deepSeekFindJSONObjects(answer)
	calls := make([]deepSeekToolRequest, 0, len(objects))
	for _, obj := range objects {
		if call, ok := deepSeekParseJSONCall(obj, tools); ok {
			calls = append(calls, call)
		}
	}
	if len(calls) == 0 {
		return nil
	}
	return calls
}

func deepSeekParseJSONCall(obj string, tools []deepSeekToolSpec) (deepSeekToolRequest, bool) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Params    json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(obj), &call); err != nil {
		return deepSeekToolRequest{}, false
	}

	arguments := call.Arguments
	if len(arguments) == 0 {
		arguments = call.Params
	}
	if call.Name == "" || !deepSeekToolDeclared(call.Name, tools) {
		return deepSeekToolRequest{}, false
	}

	encoded := "{}"
	if len(arguments) > 0 {
		if !deepSeekValidJSON(string(arguments)) {
			return deepSeekToolRequest{}, false
		}
		encoded = string(arguments)
	}
	return deepSeekToolRequest{Name: call.Name, Arguments: encoded, Raw: obj}, true
}

func deepSeekToolDeclared(name string, tools []deepSeekToolSpec) bool {
	for _, tool := range tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

func deepSeekValidJSON(s string) bool {
	var scratch json.RawMessage
	return json.Unmarshal([]byte(s), &scratch) == nil
}

func deepSeekToolCallID(name string) string {
	return "call_" + name + "_" + strconv.FormatUint(deepSeekToolCallSeq.Add(1), 10)
}

var deepSeekToolCallSeq atomic.Uint64

// deepSeekSurroundingText returns what the model wrote around the calls.
func deepSeekSurroundingText(answer string, raws ...string) string {
	if answer == "" || len(raws) == 0 {
		return ""
	}

	remaining := answer
	surrounding := ""
	for _, raw := range raws {
		if raw == "" {
			continue
		}
		idx := strings.Index(remaining, raw)
		if idx < 0 {
			continue
		}
		surrounding += remaining[:idx]
		remaining = remaining[idx+len(raw):]
	}
	surrounding += remaining

	surrounding = strings.TrimSpace(deepSeekDsmlStripped(surrounding))
	surrounding = strings.TrimSpace(strings.Trim(surrounding, "`"))
	return strings.TrimSpace(strings.TrimPrefix(surrounding, "json"))
}

func deepSeekDsmlStripped(text string) string {
	if !deepSeekHasDSML(text) {
		return text
	}
	return deepSeekStripDSML(text)
}

// deepSeekFindJSONObjects returns every balanced top-level JSON object in s.
func deepSeekFindJSONObjects(s string) []string {
	var objects []string
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		end := deepSeekMatchBrace(s, i)
		if end <= i {
			continue
		}
		candidate := s[i : end+1]
		if !deepSeekValidJSON(candidate) {
			continue
		}
		objects = append(objects, candidate)
		i = end
	}
	return objects
}

func deepSeekMatchBrace(s string, start int) int {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
