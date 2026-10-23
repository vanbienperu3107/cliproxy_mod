package dsweb

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// DSML is the tool-call dialect the site models actually speak. The prompt asks
// for a single JSON object, but these models have a native tool syntax they
// prefer, and when they reach for it the answer comes back as a DSML block.
//
// Four spelling variations are in the wild and all of them are matched here:
// the bar may be full-width (U+FF5C) or ASCII, the marker may be padded with
// spaces, and a closing tag may or may not carry its slash.

var (
	deepSeekDSMLMarker = regexp.MustCompile(`[｜|]{1,2}\s*DSML\s*[｜|]{1,2}`)

	deepSeekDSMLBlockOpen     = regexp.MustCompile(`<[｜|]{1,2}\s*DSML\s*[｜|]{1,2}`)
	deepSeekDSMLCallsOpen     = regexp.MustCompile(`<[｜|]{1,2}\s*DSML\s*[｜|]{1,2}\s*calls\b`)
	deepSeekDSMLInvokeOpen    = regexp.MustCompile(`<[｜|]{1,2}\s*DSML\s*[｜|]{1,2}\s*invoke\b`)
	deepSeekDSMLParameterOpen = regexp.MustCompile(`<[｜|]{1,2}\s*DSML\s*[｜|]{1,2}\s*parameter\b`)

	deepSeekDSMLCallsClose     = regexp.MustCompile(`</?[｜|]{1,2}\s*DSML\s*[｜|]{1,2}\s*calls\s*>`)
	deepSeekDSMLInvokeClose    = regexp.MustCompile(`</?[｜|]{1,2}\s*DSML\s*[｜|]{1,2}\s*invoke\s*>`)
	deepSeekDSMLParameterClose = regexp.MustCompile(`</?[｜|]{1,2}\s*DSML\s*[｜|]{1,2}\s*parameter\s*>`)
)

func deepSeekHasDSML(text string) bool {
	return deepSeekDSMLMarker.MatchString(text)
}

// deepSeekExtractDSML recovers every tool call written in DSML. Only invokes
// naming a tool the client declared are returned.
func deepSeekExtractDSML(text string, tools []deepSeekToolSpec) []deepSeekToolRequest {
	invokes := deepSeekDSMLInvokes(text)
	if len(invokes) == 0 {
		return nil
	}

	calls := make([]deepSeekToolRequest, 0, len(invokes))
	for _, invoke := range invokes {
		call, ok := deepSeekParseDSMLInvoke(invoke, tools)
		if !ok {
			continue
		}
		if slices.ContainsFunc(calls, func(existing deepSeekToolRequest) bool {
			return existing.Name == call.Name && existing.Arguments == call.Arguments
		}) {
			continue
		}
		calls = append(calls, call)
	}
	if len(calls) == 0 {
		return nil
	}
	return calls
}

func deepSeekDSMLInvokes(text string) []string {
	var invokes []string
	rest := text
	for {
		openAt := deepSeekDSMLInvokeOpen.FindStringIndex(rest)
		if openAt == nil {
			return invokes
		}
		rest = rest[openAt[0]:]

		openEnd := strings.Index(rest, ">")
		if openEnd < 0 {
			return invokes
		}
		head := rest[:openEnd+1]
		rest = rest[openEnd+1:]

		closeAt := deepSeekDSMLInvokeClose.FindStringIndex(rest)
		if closeAt == nil {
			return invokes
		}
		invokes = append(invokes, head+rest[:closeAt[1]])
		rest = rest[closeAt[1]:]
	}
}

func deepSeekParseDSMLInvoke(invoke string, tools []deepSeekToolSpec) (deepSeekToolRequest, bool) {
	attributes, body, found := strings.Cut(invoke, ">")
	if !found {
		return deepSeekToolRequest{}, false
	}

	name := deepSeekDSMLAttribute(attributes, "name")
	if name == "" || !deepSeekToolDeclared(name, tools) {
		return deepSeekToolRequest{}, false
	}

	return deepSeekToolRequest{
		Name:      name,
		Arguments: deepSeekDSMLArguments(body),
		Raw:       invoke,
	}, true
}

func deepSeekDSMLAttribute(attributes, key string) string {
	marker := key + "="
	rest := attributes
	for {
		at := strings.Index(rest, marker)
		if at < 0 {
			return ""
		}
		wholeAttribute := at == 0 || isDSMLAttrBreak(rest[at-1])
		rest = rest[at+len(marker):]
		if !wholeAttribute {
			continue
		}
		if rest == "" {
			return ""
		}
		quote := rest[0]
		if quote != '"' && quote != '\'' {
			return ""
		}
		value, _, ok := strings.Cut(rest[1:], string(quote))
		if !ok {
			return ""
		}
		return value
	}
}

func isDSMLAttrBreak(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func deepSeekDSMLArguments(body string) string {
	params := deepSeekDSMLParameters(body)

	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	slices.Sort(names)

	var b strings.Builder
	b.WriteString("{")
	for i, name := range names {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q:%s", name, deepSeekDSMLValue(params[name]))
	}
	b.WriteString("}")
	return b.String()
}

func deepSeekDSMLParameters(body string) map[string]string {
	params := make(map[string]string)
	rest := body
	for {
		openAt := deepSeekDSMLParameterOpen.FindStringIndex(rest)
		if openAt == nil {
			return params
		}
		rest = rest[openAt[1]:]

		openEnd := strings.Index(rest, ">")
		if openEnd < 0 {
			return params
		}
		attributes := rest[:openEnd]
		rest = rest[openEnd+1:]

		closeAt := deepSeekDSMLParameterClose.FindStringIndex(rest)
		if closeAt == nil {
			return params
		}
		if name := deepSeekDSMLAttribute(attributes, "name"); name != "" {
			params[name] = rest[:closeAt[0]]
		}
		rest = rest[closeAt[1]:]
	}
}

func deepSeekDSMLValue(body string) string {
	encoded, err := json.Marshal(strings.TrimSpace(body))
	if err != nil {
		return `""`
	}
	return string(encoded)
}

// deepSeekStripDSML removes a DSML element from an answer. An element that was
// never closed is treated as running to the end of the answer.
func deepSeekStripDSML(text string) string {
	for _, element := range []struct {
		open  *regexp.Regexp
		close *regexp.Regexp
	}{
		{deepSeekDSMLCallsOpen, deepSeekDSMLCallsClose},
		{deepSeekDSMLInvokeOpen, deepSeekDSMLInvokeClose},
		{deepSeekDSMLParameterOpen, deepSeekDSMLParameterClose},
	} {
		openAt := element.open.FindStringIndex(text)
		if openAt == nil {
			continue
		}
		searchFrom := openAt[1]
		if tagEnd := strings.Index(text[searchFrom:], ">"); tagEnd >= 0 {
			searchFrom += tagEnd + 1
		}
		if end := element.close.FindStringIndex(text[searchFrom:]); end != nil {
			return strings.TrimSpace(text[:openAt[0]] + text[searchFrom+end[1]:])
		}
		return strings.TrimSpace(text[:openAt[0]])
	}
	return text
}

func deepSeekDSMLInvokeCount(text string) int {
	return len(deepSeekDSMLInvokeOpen.FindAllStringIndex(text, -1))
}
