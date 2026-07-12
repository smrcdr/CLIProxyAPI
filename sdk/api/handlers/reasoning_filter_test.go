package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStripReasoningResponseChatPreservesUsageAndTools(t *testing.T) {
	input := []byte(`{"choices":[{"message":{"role":"assistant","reasoning":"secret","reasoning_content":"secret","reasoning_details":[{"text":"secret"}],"content":"before<think>hidden</think>after","tool_calls":[{"id":"call_1","type":"function"}]},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":8,"completion_tokens_details":{"reasoning_tokens":5}}}`)
	output, err := stripReasoningResponse("openai", input)
	if err != nil {
		t.Fatal(err)
	}
	assertNotContains(t, output, "secret", "<think>", "reasoning_content", "reasoning_details")
	assertContains(t, output, `"content":"beforeafter"`, `"tool_calls"`, `"finish_reason":"tool_calls"`, `"reasoning_tokens":5`)
}

func TestStripReasoningResponseResponses(t *testing.T) {
	input := []byte(`{"id":"resp_1","output":[{"id":"r1","type":"reasoning","summary":[{"type":"summary_text","text":"secret"}]},{"id":"m1","type":"message","content":[{"type":"output_text","text":"ok<thinking>hidden</thinking>!"}]}],"usage":{"output_tokens":9,"output_tokens_details":{"reasoning_tokens":7}}}`)
	output, err := stripReasoningResponse("openai-response", input)
	if err != nil {
		t.Fatal(err)
	}
	assertNotContains(t, output, "secret", "hidden", `"type":"reasoning"`)
	assertContains(t, output, `"text":"ok!"`, `"reasoning_tokens":7`)
}

func TestStripReasoningResponseMessagesRenumbersBlocks(t *testing.T) {
	input := []byte(`{"content":[{"type":"thinking","thinking":"secret","signature":"sig"},{"type":"text","text":"hello<think>x</think> world"},{"type":"tool_use","id":"tool_1","name":"lookup","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":6}}`)
	output, err := stripReasoningResponse("claude", input)
	if err != nil {
		t.Fatal(err)
	}
	assertNotContains(t, output, "secret", "signature", `"type":"thinking"`)
	assertContains(t, output, `"text":"hello world"`, `"type":"tool_use"`, `"stop_reason":"tool_use"`, `"output_tokens":6`)
}

func TestReasoningStreamFilterChatHandlesSplitThinkTags(t *testing.T) {
	filter := newReasoningStreamFilter("openai")
	chunks := [][]byte{
		[]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"secret\",\"content\":\"hello <thi\"}}]}\n\n"),
		[]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"nk>hidden\"}}]}\n\n"),
		[]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\" still hidden</think>world\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\"}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"completion_tokens_details\":{\"reasoning_tokens\":4}}}\n\ndata: [DONE]\n\n"),
	}
	output := collectFilteredChunks(t, filter, chunks)
	assertNotContains(t, output, "secret", "hidden", "reasoning_content", "<think")
	assertContains(t, output, "hello ", "world", "call_1", "reasoning_tokens", "[DONE]")
}

func TestReasoningStreamFilterResponsesDropsEventsAndRenumbers(t *testing.T) {
	filter := newReasoningStreamFilter("openai-response")
	chunks := [][]byte{
		[]byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":4,\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"r1\"}}\n\n"),
		[]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":5,\"output_index\":1,\"content_index\":0,\"delta\":\"A<thin\"}\n\n"),
		[]byte("event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"sequence_number\":6,\"delta\":\"secret\"}\n\n"),
		[]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":7,\"output_index\":1,\"content_index\":0,\"delta\":\"king>x</thinking>B\"}\n\n"),
		[]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":8,\"response\":{\"output\":[{\"type\":\"reasoning\",\"summary\":[]},{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"AB\"}]}],\"usage\":{\"output_tokens_details\":{\"reasoning_tokens\":3}}}}\n\n"),
	}
	output := collectFilteredChunks(t, filter, chunks)
	assertNotContains(t, output, "secret", `"type":"reasoning"`, "<thinking>")
	assertContains(t, output, `"sequence_number":0`, `"sequence_number":1`, `"sequence_number":2`, `"reasoning_tokens":3`)
}

func TestReasoningStreamFilterMessagesDropsBlocksAndRenumbers(t *testing.T) {
	filter := newReasoningStreamFilter("claude")
	chunks := [][]byte{
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n"),
		[]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"secret\"}}\n\n"),
		[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"),
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"),
		[]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello <think>hidden\"}}\n\n"),
		[]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"more</think>world\"}}\n\n"),
		[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n\n"),
	}
	output := collectFilteredChunks(t, filter, chunks)
	assertNotContains(t, output, "secret", "hidden", "more", `"thinking"`, `"index":1`)
	assertContains(t, output, "hello ", "world", `"index":0`, `"output_tokens":9`)
}

func TestReasoningFilterFailClosedAndDisabledIdentity(t *testing.T) {
	if _, err := stripReasoningResponse("openai", []byte(`{"choices":`)); err == nil {
		t.Fatal("expected malformed JSON to fail closed")
	}
	original := []byte(`{"reasoning":"kept while disabled"}`)
	handler := &BaseAPIHandler{}
	output, _, errMessage := handler.finalizeResponse("openai", original, nil)
	if errMessage != nil || string(output) != string(original) {
		t.Fatalf("disabled filter changed response: output=%s err=%v", output, errMessage)
	}
}

func collectFilteredChunks(t *testing.T, filter *reasoningStreamFilter, chunks [][]byte) []byte {
	t.Helper()
	var output []byte
	for _, chunk := range chunks {
		frames, err := filter.Write(chunk)
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range frames {
			output = append(output, frame...)
		}
	}
	if _, err := filter.Flush(); err != nil {
		t.Fatal(err)
	}
	var eventCount int
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "data: {") {
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
				t.Fatalf("invalid output SSE JSON: %v\n%s", err, output)
			}
			eventCount++
		}
	}
	if eventCount == 0 {
		t.Fatalf("no JSON events emitted: %s", output)
	}
	return output
}

func assertContains(t *testing.T, output []byte, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(string(output), needle) {
			t.Errorf("output does not contain %q:\n%s", needle, output)
		}
	}
}

func assertNotContains(t *testing.T, output []byte, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if strings.Contains(string(output), needle) {
			t.Errorf("output contains %q:\n%s", needle, output)
		}
	}
}
