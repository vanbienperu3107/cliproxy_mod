package dsweb

import (
	"strings"
	"testing"
)

func consumeAll(t *testing.T, accumulator *deepSeekStreamAccumulator, chunks ...string) {
	t.Helper()
	for _, chunk := range chunks {
		accumulator.consume([]byte(chunk))
	}
}

func TestAccumulatorSplitsThinkingAndAnswer(t *testing.T) {
	accumulator := newDeepSeekStreamAccumulator("deepseek-v4-pro")
	consumeAll(t, accumulator,
		`{"p":"response","o":"BATCH","v":{"response":{"thinking_enabled":true,"message_id":42,"fragments":[{"type":"THINK","content":"hmm "},{"type":"ANSWER","content":"hello"}]}}}`,
		`{"p":"response/status","v":"FINISHED"}`,
	)

	if got := accumulator.thinkingText(); got != "hmm " {
		t.Fatalf("thinking = %q", got)
	}
	if got := accumulator.contentText(); got != "hello" {
		t.Fatalf("content = %q", got)
	}
	if !accumulator.sawFinished {
		t.Fatal("FINISHED status not recorded")
	}
	if accumulator.responseMessageID == nil || *accumulator.responseMessageID != 42 {
		t.Fatalf("message id = %v", accumulator.responseMessageID)
	}
}

func TestAccumulatorAppendsFragmentContent(t *testing.T) {
	accumulator := newDeepSeekStreamAccumulator("deepseek-v4-pro")
	consumeAll(t, accumulator,
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"Hello "}`,
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"world"}`,
	)
	if got := accumulator.contentText(); got != "Hello world" {
		t.Fatalf("content = %q", got)
	}
}

func TestAccumulatorRendersCitations(t *testing.T) {
	accumulator := newDeepSeekStreamAccumulator("deepseek-v4-pro-search")
	consumeAll(t, accumulator,
		`{"p":"response/search_results","v":[{"url":"https://example.com/a","title":"A","snippet":"a"}]}`,
		`{"p":"response","o":"BATCH","v":{"response":{"fragments":[{"type":"ANSWER","content":"answer"}]}}}`,
		`{"p":"response/search_results","o":"BATCH","v":[{"p":"0/cite_index","v":1}]}`,
	)
	citations := accumulator.citations()
	if !strings.Contains(citations, "example.com/a") || !strings.Contains(citations, "1") {
		t.Fatalf("citations = %q", citations)
	}
}

func TestAccumulatorDetectsRateLimitEvent(t *testing.T) {
	accumulator := newDeepSeekStreamAccumulator("deepseek-v4-pro")
	consumeAll(t, accumulator,
		`{"type":"error","content":"消息发送过于频繁","finish_reason":"rate_limit_reached"}`,
	)
	if accumulator.enforcement == nil {
		t.Fatal("rate limit event not detected")
	}
	if status := accumulator.enforcement.status(); status != 429 {
		t.Fatalf("status = %d, want 429", status)
	}
}

func TestDrainStreamEmitsDeltas(t *testing.T) {
	body := strings.Join([]string{
		`data: {"p":"response","o":"BATCH","v":{"response":{"fragments":[{"type":"THINK","content":"a"},{"type":"ANSWER","content":"b"}]}}}`,
		``,
		`data: {"p":"response/fragments/-1/content","o":"APPEND","v":"c"}`,
		``,
		`data: {"p":"response/status","v":"FINISHED"}`,
		``,
	}, "\n")

	var content, thinking strings.Builder
	accumulator := newDeepSeekStreamAccumulator("deepseek-v4-pro")
	err := drainStream(strings.NewReader(body), accumulator, func(c, th string) {
		content.WriteString(c)
		thinking.WriteString(th)
	})
	if err != nil {
		t.Fatalf("drainStream: %v", err)
	}
	if thinking.String() != "a" {
		t.Fatalf("thinking deltas = %q", thinking.String())
	}
	if content.String() != "bc" {
		t.Fatalf("content deltas = %q", content.String())
	}
}
