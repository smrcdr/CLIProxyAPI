package responses

import (
	"bytes"
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIChatCompletionsToResponsesNonStreamPreservesUsage(t *testing.T) {
	original := []byte(`{"model":"public","messages":[{"role":"user","content":"hello"}]}`)
	translated := ConvertOpenAIChatCompletionsRequestToOpenAIResponses("upstream", original, false)
	if got := gjson.GetBytes(translated, "model").String(); got != "upstream" {
		t.Fatalf("translated model = %q; body = %s", got, translated)
	}
	if !gjson.GetBytes(translated, "input").Exists() {
		t.Fatalf("translated input is missing: %s", translated)
	}

	rawResponse := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"upstream",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],` +
		`"usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17,` +
		`"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`)
	out := ConvertOpenAIResponsesResponseToOpenAIChatCompletionsNonStream(
		context.Background(),
		"upstream",
		original,
		translated,
		rawResponse,
		nil,
	)
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "hello" {
		t.Fatalf("response content = %q; body = %s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.prompt_tokens").Int(); got != 12 {
		t.Fatalf("prompt tokens = %d; body = %s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.completion_tokens").Int(); got != 5 {
		t.Fatalf("completion tokens = %d; body = %s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.total_tokens").Int(); got != 17 {
		t.Fatalf("total tokens = %d; body = %s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.prompt_tokens_details.cached_tokens").Int(); got != 3 {
		t.Fatalf("cached tokens = %d; body = %s", got, out)
	}
}

func TestConvertOpenAIResponsesToChatStreamEmitsFinalUsage(t *testing.T) {
	original := []byte(`{"model":"public","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	translated := ConvertOpenAIChatCompletionsRequestToOpenAIResponses("upstream", original, true)
	var param any
	var outputs [][]byte
	for _, event := range [][]byte{
		[]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"upstream"}}` + "\n\n"),
		[]byte(`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n"),
		[]byte(`data: {"type":"response.completed","response":{"id":"resp_1","model":"upstream",` +
			`"usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17,` +
			`"input_tokens_details":{"cached_tokens":3}}}}` + "\n\n"),
	} {
		outputs = append(outputs, ConvertOpenAIResponsesResponseToOpenAIChatCompletions(
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
		if gjson.GetBytes(payload, "usage").Exists() {
			usage = payload
		}
	}
	if len(usage) == 0 {
		t.Fatalf("final usage chunk is missing; outputs = %q", outputs)
	}
	if got := gjson.GetBytes(usage, "usage.prompt_tokens").Int(); got != 12 {
		t.Fatalf("prompt tokens = %d; chunk = %s", got, usage)
	}
	if got := gjson.GetBytes(usage, "usage.completion_tokens").Int(); got != 5 {
		t.Fatalf("completion tokens = %d; chunk = %s", got, usage)
	}
}

func TestConvertOpenAIChatCompletionsRequestToResponsesPreservesSupportedFeatures(t *testing.T) {
	original := []byte(`{
		"model":"public",
		"stream":true,
		"max_completion_tokens":321,
		"service_tier":"priority",
		"reasoning_effort":"high",
		"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"},"strict":true}},
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],
		"messages":[
			{"role":"developer","content":"Follow policy"},
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"done"}
		]
	}`)
	out := ConvertOpenAIChatCompletionsRequestToOpenAIResponses("upstream", original, true)
	root := gjson.ParseBytes(out)

	if got := root.Get("instructions").String(); got != "Follow policy" {
		t.Fatalf("instructions = %q; body = %s", got, out)
	}
	if !smartRouterResponsesInputHasContentType(root.Get("input"), "input_image") {
		t.Fatalf("input image is missing; body = %s", out)
	}
	for _, itemType := range []string{"function_call", "function_call_output"} {
		if !smartRouterResponsesInputHasType(root.Get("input"), itemType) {
			t.Fatalf("%s is missing; body = %s", itemType, out)
		}
	}
	if got := root.Get("tools.0.name").String(); got != "lookup" {
		t.Fatalf("tool name = %q; body = %s", got, out)
	}
	if got := root.Get("tool_choice.name").String(); got != "lookup" {
		t.Fatalf("tool choice = %q; body = %s", got, out)
	}
	if got := root.Get("reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning effort = %q; body = %s", got, out)
	}
	if got := root.Get("text.format.name").String(); got != "answer" {
		t.Fatalf("structured output name = %q; body = %s", got, out)
	}
	if got := root.Get("max_output_tokens").Int(); got != 321 {
		t.Fatalf("max output tokens = %d; body = %s", got, out)
	}
	if got := root.Get("service_tier").String(); got != "priority" {
		t.Fatalf("service tier = %q; body = %s", got, out)
	}
	if !root.Get("stream").Bool() {
		t.Fatalf("stream flag is missing; body = %s", out)
	}
}

func smartRouterResponsesInputHasType(input gjson.Result, itemType string) bool {
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		found = item.Get("type").String() == itemType
		return !found
	})
	return found
}

func smartRouterResponsesInputHasContentType(input gjson.Result, contentType string) bool {
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
