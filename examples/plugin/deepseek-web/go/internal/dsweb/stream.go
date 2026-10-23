package dsweb

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

// The upstream stream is a generic pub/sub envelope: `p` names the topic, `v`
// carries the payload, and `o` marks a batched patch that rewrites entries
// already streamed instead of appending new ones.

type deepSeekStreamEvent struct {
	P string          `json:"p"`
	O string          `json:"o"`
	V json.RawMessage `json:"v"`
}

type deepSeekFragment struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type deepSeekResponsePayload struct {
	Response struct {
		ThinkingEnabled bool               `json:"thinking_enabled"`
		MessageID       *int64             `json:"message_id"`
		Fragments       []deepSeekFragment `json:"fragments"`
	} `json:"response"`
}

type deepSeekSearchResult struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Snippet   string `json:"snippet"`
	Date      string `json:"date"`
	CiteIndex int    `json:"cite_index"`
}

type deepSeekCitePatch struct {
	P string `json:"p"`
	V int    `json:"v"`
}

const (
	deepSeekFragmentThink    = "THINK"
	deepSeekFragmentAnswer   = "ANSWER"
	deepSeekFragmentResponse = "RESPONSE"
)

const (
	deepSeekChannelThinking = "thinking"
	deepSeekChannelContent  = "content"
)

// deepSeekStreamAccumulator turns the upstream event stream into the text a
// client should see.
type deepSeekStreamAccumulator struct {
	thinkingOnly bool

	channel  string
	content  strings.Builder
	thinking strings.Builder
	cites    []deepSeekSearchResult

	sawFinished bool

	responseMessageID *int64

	enforcement *deepSeekEnforcement
}

func newDeepSeekStreamAccumulator(model string) *deepSeekStreamAccumulator {
	return &deepSeekStreamAccumulator{thinkingOnly: deepSeekIsThinkingModel(model)}
}

func deepSeekIsThinkingModel(model string) bool {
	name := strings.ToLower(model)
	return strings.Contains(name, "r1") ||
		strings.Contains(name, "think") ||
		strings.Contains(name, "reason")
}

func (a *deepSeekStreamAccumulator) consume(payload []byte) {
	if enforcement := deepSeekEnforcementFromEvent(payload); enforcement != nil {
		a.enforcement = enforcement
	}

	var event deepSeekStreamEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return
	}
	a.apply(event)
}

func (a *deepSeekStreamAccumulator) apply(event deepSeekStreamEvent) {
	if event.P == "" {
		a.applyTopicless(event.V)
		return
	}

	switch event.P {
	case "response/status":
		if s, ok := decodeJSONString(event.V); ok && s == "FINISHED" {
			a.sawFinished = true
		}
		return
	case "response/search_status":
		return
	case "response/search_results":
		a.applySearchResults(event)
		return
	case "response/fragments":
		a.applyFragments(event.V)
		return
	case "response":
		a.applyResponseTopic(event.V)
		return
	}

	if event.O == "APPEND" && strings.HasPrefix(event.P, "response/fragments/") && strings.HasSuffix(event.P, "/content") {
		if s, ok := decodeJSONString(event.V); ok {
			a.append(s)
		}
		return
	}

	if s, ok := decodeJSONString(event.V); ok {
		a.append(s)
	}
}

func (a *deepSeekStreamAccumulator) applyTopicless(value json.RawMessage) {
	if s, ok := decodeJSONString(value); ok {
		a.append(s)
		return
	}
	var payload deepSeekResponsePayload
	if err := json.Unmarshal(value, &payload); err == nil {
		a.applyResponsePayload(payload)
	}
}

func (a *deepSeekStreamAccumulator) applyResponseTopic(value json.RawMessage) {
	var entries []deepSeekResponsePayload
	if err := json.Unmarshal(value, &entries); err == nil && len(entries) > 0 {
		for _, entry := range entries {
			a.applyResponsePayload(entry)
		}
		return
	}

	var payload deepSeekResponsePayload
	if err := json.Unmarshal(value, &payload); err == nil {
		a.applyResponsePayload(payload)
	}
}

func (a *deepSeekStreamAccumulator) applyResponsePayload(payload deepSeekResponsePayload) {
	if payload.Response.ThinkingEnabled {
		a.channel = deepSeekChannelThinking
	}
	if payload.Response.MessageID != nil {
		a.responseMessageID = payload.Response.MessageID
	}
	for _, fragment := range payload.Response.Fragments {
		a.appendFragment(fragment)
	}
}

func (a *deepSeekStreamAccumulator) applyFragments(value json.RawMessage) {
	var fragments []deepSeekFragment
	if err := json.Unmarshal(value, &fragments); err == nil && len(fragments) > 0 {
		for _, fragment := range fragments {
			a.appendFragment(fragment)
		}
		return
	}

	var fragment deepSeekFragment
	if err := json.Unmarshal(value, &fragment); err == nil {
		a.appendFragment(fragment)
	}
}

func (a *deepSeekStreamAccumulator) appendFragment(fragment deepSeekFragment) {
	if fragment.Content == "" {
		return
	}
	switch strings.ToUpper(fragment.Type) {
	case deepSeekFragmentThink:
		a.channel = deepSeekChannelThinking
	case deepSeekFragmentAnswer, deepSeekFragmentResponse:
		a.channel = deepSeekChannelContent
	}
	a.append(fragment.Content)
}

func (a *deepSeekStreamAccumulator) append(text string) {
	switch a.channel {
	case deepSeekChannelThinking:
		a.thinking.WriteString(text)
	case deepSeekChannelContent:
		a.content.WriteString(text)
	default:
		if a.thinkingOnly {
			a.thinking.WriteString(text)
		} else {
			a.content.WriteString(text)
		}
	}
}

func (a *deepSeekStreamAccumulator) applySearchResults(event deepSeekStreamEvent) {
	if event.O == "BATCH" {
		var patches []deepSeekCitePatch
		if err := json.Unmarshal(event.V, &patches); err != nil {
			return
		}
		for _, patch := range patches {
			index, ok := deepSeekParseCiteIndex(patch.P)
			if ok && index >= 0 && index < len(a.cites) {
				a.cites[index].CiteIndex = patch.V
			}
		}
		return
	}

	var results []deepSeekSearchResult
	if err := json.Unmarshal(event.V, &results); err == nil && len(results) > 0 {
		a.cites = results
	}
}

func deepSeekParseCiteIndex(path string) (int, bool) {
	const suffix = "/cite_index"
	if !strings.HasSuffix(path, suffix) {
		return 0, false
	}
	head := strings.TrimSuffix(path, suffix)
	if head == "" {
		return 0, false
	}
	index := 0
	for _, c := range head {
		if c < '0' || c > '9' {
			return 0, false
		}
		index = index*10 + int(c-'0')
	}
	return index, true
}

func (a *deepSeekStreamAccumulator) citations() string {
	var b strings.Builder
	for _, cite := range a.cites {
		title := cite.Title
		if title == "" {
			title = cite.URL
		}
		if title == "" {
			continue
		}
		b.WriteString("\n[")
		b.WriteString(strconv.Itoa(cite.CiteIndex))
		b.WriteString("] ")
		b.WriteString(title)
		if cite.URL != "" && cite.URL != title {
			b.WriteString(" (")
			b.WriteString(cite.URL)
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// contentText is the visible answer with any citations appended.
func (a *deepSeekStreamAccumulator) contentText() string {
	return a.content.String() + a.citations()
}

func (a *deepSeekStreamAccumulator) thinkingText() string {
	return a.thinking.String()
}

func decodeJSONString(value json.RawMessage) (string, bool) {
	if len(value) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return "", false
	}
	return s, true
}

// drainStream reads SSE payloads from upstream, folding each into the
// accumulator. onDelta, when set, receives the newly appended visible and
// reasoning text after every event so the caller can forward it live. Citations
// are never part of a delta: they trail the answer and are emitted once, at the
// end.
func drainStream(upstream io.Reader, accumulator *deepSeekStreamAccumulator, onDelta func(content, thinking string)) error {
	reader := bufio.NewReaderSize(upstream, 64*1024)
	var data strings.Builder
	sentContent, sentThinking := 0, 0

	flush := func() {
		if data.Len() == 0 {
			return
		}
		payload := []byte(data.String())
		data.Reset()
		accumulator.consume(payload)
		if onDelta == nil {
			return
		}
		content := accumulator.content.String()
		thinking := accumulator.thinking.String()
		if len(thinking) > sentThinking {
			onDelta("", thinking[sentThinking:])
			sentThinking = len(thinking)
		}
		if len(content) > sentContent {
			onDelta(content[sentContent:], "")
			sentContent = len(content)
		}
	}

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				flush()
			case strings.HasPrefix(trimmed, "data:"):
				chunk := strings.TrimPrefix(trimmed, "data:")
				chunk = strings.TrimPrefix(chunk, " ")
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(chunk)
			}
		}
		if err != nil {
			flush()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
