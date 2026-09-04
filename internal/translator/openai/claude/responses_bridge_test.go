package claude

import (
	"bytes"
	"context"
	"testing"

	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/openai/responses"
	"github.com/tidwall/gjson"
)

func TestConvertClaudeToResponsesNonStreamPreservesUsage(t *testing.T) {
	original := []byte(`{"model":"public","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`)
	translated := ConvertClaudeRequestToOpenAIResponses("upstream", original, false)
	if got := gjson.GetBytes(translated, "model").String(); got != "upstream" {
		t.Fatalf("translated model = %q; body = %s", got, translated)
	}
	if !gjson.GetBytes(translated, "input").Exists() {
		t.Fatalf("translated input is missing: %s", translated)
	}

	rawResponse := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"upstream",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],` +
		`"usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17,` +
		`"input_tokens_details":{"cached_tokens":3}}}`)
	out := ConvertOpenAIResponsesResponseToClaudeNonStream(
		context.Background(),
		"upstream",
		original,
		translated,
		rawResponse,
		nil,
	)
	if got := gjson.GetBytes(out, "content.0.text").String(); got != "hello" {
		t.Fatalf("response content = %q; body = %s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.input_tokens").Int(); got != 9 {
		t.Fatalf("input tokens = %d; body = %s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.output_tokens").Int(); got != 5 {
		t.Fatalf("output tokens = %d; body = %s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.cache_read_input_tokens").Int(); got != 3 {
		t.Fatalf("cache read tokens = %d; body = %s", got, out)
	}
}

func TestConvertResponsesToClaudeStreamEmitsFinalUsage(t *testing.T) {
	original := []byte(`{"model":"public","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	translated := ConvertClaudeRequestToOpenAIResponses("upstream", original, true)
	var param any
	var outputs [][]byte
	for _, event := range [][]byte{
		[]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"upstream"}}` + "\n\n"),
		[]byte(`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n"),
		[]byte(`data: {"type":"response.completed","response":{"id":"resp_1","model":"upstream",` +
			`"usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17,` +
			`"input_tokens_details":{"cached_tokens":3}}}}` + "\n\n"),
	} {
		outputs = append(outputs, ConvertOpenAIResponsesResponseToClaude(
			context.Background(),
			"upstream",
			original,
			translated,
			event,
			&param,
		)...)
	}

	var usage []byte
	for _, output := range outputs {
		payload := bytes.TrimSpace(output)
		if bytes.HasPrefix(payload, []byte("data:")) {
			payload = bytes.TrimSpace(payload[len("data:"):])
		}
		if gjson.GetBytes(payload, "type").String() == "message_delta" &&
			gjson.GetBytes(payload, "usage").Exists() {
			usage = payload
		}
	}
	if len(usage) == 0 {
		t.Fatalf("final message_delta usage is missing; outputs = %q", outputs)
	}
	if got := gjson.GetBytes(usage, "usage.input_tokens").Int(); got != 9 {
		t.Fatalf("input tokens = %d; chunk = %s", got, usage)
	}
	if got := gjson.GetBytes(usage, "usage.output_tokens").Int(); got != 5 {
		t.Fatalf("output tokens = %d; chunk = %s", got, usage)
	}
	if got := gjson.GetBytes(usage, "usage.cache_read_input_tokens").Int(); got != 3 {
		t.Fatalf("cache read tokens = %d; chunk = %s", got, usage)
	}
}

func TestConvertClaudeRequestToResponsesPreservesSupportedFeatures(t *testing.T) {
	original := []byte(`{
		"model":"public",
		"stream":true,
		"max_tokens":456,
		"thinking":{"type":"enabled","budget_tokens":4096},
		"system":"Follow policy",
		"tool_choice":{"type":"tool","name":"lookup"},
		"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}
		]
	}`)
	out := ConvertClaudeRequestToOpenAIResponses("upstream", original, true)
	root := gjson.ParseBytes(out)

	if got := root.Get("instructions").String(); got != "Follow policy" {
		t.Fatalf("instructions = %q; body = %s", got, out)
	}
	if !claudeBridgeResponsesInputHasContentType(root.Get("input"), "input_image") {
		t.Fatalf("input image is missing; body = %s", out)
	}
	for _, itemType := range []string{"function_call", "function_call_output"} {
		if !claudeBridgeResponsesInputHasType(root.Get("input"), itemType) {
			t.Fatalf("%s is missing; body = %s", itemType, out)
		}
	}
	if got := root.Get("tools.0.name").String(); got != "lookup" {
		t.Fatalf("tool name = %q; body = %s", got, out)
	}
	if got := root.Get("tool_choice.name").String(); got != "lookup" {
		t.Fatalf("tool choice = %q; body = %s", got, out)
	}
	if got := root.Get("reasoning.effort").String(); got == "" {
		t.Fatalf("reasoning effort is missing; body = %s", out)
	}
	if got := root.Get("max_output_tokens").Int(); got != 456 {
		t.Fatalf("max output tokens = %d; body = %s", got, out)
	}
	if !root.Get("stream").Bool() {
		t.Fatalf("stream flag is missing; body = %s", out)
	}
}

func claudeBridgeResponsesInputHasType(input gjson.Result, itemType string) bool {
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		found = item.Get("type").String() == itemType
		return !found
	})
	return found
}

func claudeBridgeResponsesInputHasContentType(input gjson.Result, contentType string) bool {
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		item.Get("content").ForEach(func(_, content gjson.Result) bool {
			found = content.Get("type").String() == contentType
			return !found
		})
		return !found
	})
	return found
}
