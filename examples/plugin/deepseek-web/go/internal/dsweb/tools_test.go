package dsweb

import (
	"encoding/json"
	"testing"
)

func TestExtractJSONToolCall(t *testing.T) {
	tools := []deepSeekToolSpec{toolSpec("get_weather", `{"type":"object"}`)}
	answer := `Let me check. {"name":"get_weather","arguments":{"city":"Hanoi"}}`

	calls := deepSeekExtractToolCalls(answer, tools)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].Name != "get_weather" {
		t.Fatalf("name = %q", calls[0].Name)
	}
	if calls[0].Arguments != `{"city":"Hanoi"}` {
		t.Fatalf("arguments = %q", calls[0].Arguments)
	}
	if got := deepSeekSurroundingText(answer, calls[0].Raw); got != "Let me check." {
		t.Fatalf("preamble = %q", got)
	}
}

func TestExtractJSONToolCallRejectsUndeclaredTool(t *testing.T) {
	tools := []deepSeekToolSpec{toolSpec("get_weather", `{}`)}
	answer := `{"name":"run_shell","arguments":{"cmd":"rm -rf /"}}`

	if calls := deepSeekExtractToolCalls(answer, tools); len(calls) != 0 {
		t.Fatalf("undeclared tool accepted: %+v", calls)
	}
}

func TestExtractDSMLToolCall(t *testing.T) {
	tools := []deepSeekToolSpec{toolSpec("get_weather", `{}`)}
	answer := "<｜DSML｜calls>\n<｜DSML｜invoke name=\"get_weather\">\n" +
		"<｜DSML｜parameter name=\"city\" string=\"true\">Hanoi</｜DSML｜parameter>\n" +
		"</｜DSML｜invoke>\n</｜DSML｜calls>"

	calls := deepSeekExtractToolCalls(answer, tools)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].Name != "get_weather" || calls[0].Arguments != `{"city":"Hanoi"}` {
		t.Fatalf("call = %+v", calls[0])
	}
}

func TestStripDSMLRemovesTheElement(t *testing.T) {
	answer := "before <｜DSML｜invoke name=\"get_weather\">x</｜DSML｜invoke> after"
	if got := deepSeekStripDSML(answer); got != "before  after" {
		t.Fatalf("strip = %q", got)
	}
}

func TestDSMLArgumentsAreStableJSON(t *testing.T) {
	body := `<｜DSML｜parameter name="b">2</｜DSML｜parameter><｜DSML｜parameter name="a">1</｜DSML｜parameter>`
	got := deepSeekDSMLArguments(body)
	if got != `{"a":"1","b":"2"}` {
		t.Fatalf("arguments = %q", got)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("arguments are not valid JSON: %v", err)
	}
}
