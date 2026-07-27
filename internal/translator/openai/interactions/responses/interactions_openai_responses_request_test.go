package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToInteractions(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-test",
		"instructions":"be brief",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]},
			{"type":"function_call","name":"lookup","call_id":"call_1","arguments":"{\"q\":\"x\"}"},
			{"type":"function_call_output","call_id":"call_1","output":{"ok":true}}
		],
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"tool_choice":"auto",
		"reasoning":{"effort":"high","summary":"auto"},
		"response_format":{"type":"json_object"},
		"stream":true
	}`)
	out := ConvertOpenAIResponsesRequestToInteractions("gpt-test", raw, true)
	if got := gjson.GetBytes(out, "input.0.type").String(); got != "user_input" {
		t.Fatalf("input.0.type = %q, want user_input. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "text" {
		t.Fatalf("content.0.type = %q, want text. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "hi" {
		t.Fatalf("input text = %q, want hi. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.0.content.1.mime_type").String(); got != "image/png" {
		t.Fatalf("image mime_type = %q, want image/png. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.1.call_id").String(); got != "call_1" {
		t.Fatalf("function call_id = %q, want call_1. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.2.type").String(); got != "function_result" {
		t.Fatalf("function result type = %q, want function_result. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.2.name").String(); got != "lookup" {
		t.Fatalf("function result name = %q, want lookup. Output: %s", got, string(out))
	}
	sys := gjson.GetBytes(out, "system_instruction")
	if sys.Type != gjson.String {
		t.Fatalf("system_instruction type = %v, want string. Output: %s", sys.Type, string(out))
	}
	if got := sys.String(); got != "be brief" {
		t.Fatalf("system_instruction = %q, want be brief. Output: %s", got, string(out))
	}
	if gjson.GetBytes(out, "system_instruction.parts").Exists() {
		t.Fatalf("system_instruction.parts should not be forwarded. Output: %s", string(out))
	}
	if got := gjson.GetBytes(out, "generation_config.thinking_level").String(); got != "high" {
		t.Fatalf("thinking_level = %q, want high. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "lookup" {
		t.Fatalf("tool name = %q, want lookup. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "generation_config.tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "response_format.type").String(); got != "json_object" {
		t.Fatalf("response_format.type = %q, want json_object. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToInteractionsPreservesRequestStream(t *testing.T) {
	out := ConvertOpenAIResponsesRequestToInteractions("gpt-test", []byte(`{"model":"gpt-test","input":"hi","stream":true}`), false)
	if got := gjson.GetBytes(out, "stream").Bool(); !got {
		t.Fatalf("stream = %v, want true. Output: %s", got, string(out))
	}

	out = ConvertOpenAIResponsesRequestToInteractions("gpt-test", []byte(`{"model":"gpt-test","input":"hi","stream":false}`), true)
	if got := gjson.GetBytes(out, "stream").Bool(); got {
		t.Fatalf("stream = %v, want false. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToInteractionsPreservesPreviousResponseID(t *testing.T) {
	out := ConvertOpenAIResponsesRequestToInteractions("gpt-test", []byte(`{"model":"gpt-test","input":"hi","previous_response_id":"resp_123"}`), false)
	if got := gjson.GetBytes(out, "previous_interaction_id").String(); got != "resp_123" {
		t.Fatalf("previous_interaction_id = %q, want resp_123. Output: %s", got, string(out))
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesWithToolMessages(t *testing.T) {
	raw := []byte(`{"model":"gpt-test","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]},{"type":"function_call","name":"lookup","call_id":"call_1","arguments":{"q":"x"}},{"type":"function_result","name":"lookup","call_id":"call_1","result":{"ok":true}}]}`)
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", raw, false)

	foundFunctionCall := false
	foundFunctionOutput := false
	gjson.GetBytes(out, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "function_call" {
			foundFunctionCall = true
			if item.Get("name").String() != "lookup" {
				t.Fatalf("name = %q, want lookup", item.Get("name").String())
			}
		}
		if item.Get("type").String() == "function_call_output" {
			foundFunctionOutput = true
		}
		return true
	})
	if !foundFunctionCall {
		t.Fatal("function_call input not found")
	}
	if !foundFunctionOutput {
		t.Fatal("function_call_output input not found")
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesPreservesStringSystemAndThinkingConfig(t *testing.T) {
	raw := []byte(`{"model":"gpt-test","system_instruction":"You are a helpful assistant.","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"lookup","type":"function","parameters":{"type":"object"}}],"generation_config":{"tool_choice":"auto","thinking_level":"high","thinking_summaries":"auto"},"stream":true}`)
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", raw, true)
	if got := gjson.GetBytes(out, "instructions").String(); got != "You are a helpful assistant." {
		t.Fatalf("instructions = %q, want system instruction. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "reasoning.summary").String(); got != "auto" {
		t.Fatalf("reasoning.summary = %q, want auto. Output: %s", got, string(out))
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesPreservesInteractionStream(t *testing.T) {
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", []byte(`{"model":"gpt-test","input":"hi","stream":true}`), false)
	if got := gjson.GetBytes(out, "stream").Bool(); !got {
		t.Fatalf("stream = %v, want true. Output: %s", got, string(out))
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesPreservesPreviousInteractionID(t *testing.T) {
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", []byte(`{"model":"gpt-test","input":"hi","previous_interaction_id":"interaction_123"}`), false)
	if got := gjson.GetBytes(out, "previous_response_id").String(); got != "interaction_123" {
		t.Fatalf("previous_response_id = %q, want interaction_123. Output: %s", got, string(out))
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesPreservesToolCallID(t *testing.T) {
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", []byte(`{"model":"gpt-test","input":[{"type":"function_call","name":"lookup","call_id":"call_gateway","arguments":{"q":"x"}},{"type":"function_result","name":"lookup","call_id":"call_gateway","result":{"ok":true}}]}`), false)

	foundFunctionCall := false
	foundFunctionOutput := false
	gjson.GetBytes(out, "input").ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "function_call":
			foundFunctionCall = true
			if got := item.Get("call_id").String(); got != "call_gateway" {
				t.Fatalf("function_call call_id = %q, want call_gateway. Output: %s", got, string(out))
			}
		case "function_call_output":
			foundFunctionOutput = true
			if got := item.Get("call_id").String(); got != "call_gateway" {
				t.Fatalf("function_call_output call_id = %q, want call_gateway. Output: %s", got, string(out))
			}
		}
		return true
	})
	if !foundFunctionCall {
		t.Fatal("function_call input not found")
	}
	if !foundFunctionOutput {
		t.Fatal("function_call_output input not found")
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesConvertsSimpleTools(t *testing.T) {
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", []byte(`{"model":"gpt-test","tools":[{"name":"lookup","description":"Find data","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}],"input":"hi"}`), false)
	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "function" {
		t.Fatalf("tools.0.type = %q, want function. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "lookup" {
		t.Fatalf("tools.0.name = %q, want lookup. Output: %s", got, string(out))
	}
	if gjson.GetBytes(out, "tools.0.function").Exists() {
		t.Fatalf("tools.0.function should not be forwarded. Output: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.parameters.properties.q.type").String(); got != "string" {
		t.Fatalf("tools.0.parameters.properties.q.type = %q, want string. Output: %s", got, string(out))
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesConvertsFunctionDeclarationsTools(t *testing.T) {
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", []byte(`{"model":"gpt-test","tools":[{"function_declarations":[{"name":"lookup","description":"Find data","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}],"input":"hi"}`), false)
	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "function" {
		t.Fatalf("tools.0.type = %q, want function. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "lookup" {
		t.Fatalf("tools.0.name = %q, want lookup. Output: %s", got, string(out))
	}
	if gjson.GetBytes(out, "tools.0.function_declarations").Exists() {
		t.Fatalf("tools.0.function_declarations should not be forwarded. Output: %s", string(out))
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesWithImageContent(t *testing.T) {
	raw := []byte(`{"model":"gpt-test","input":[{"type":"user_input","content":[{"type":"text","text":"describe"},{"type":"image","mime_type":"image/png","data":"aGVsbG8="}]}]}`)
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", raw, false)
	if got := gjson.GetBytes(out, "input.0.content.1.type").String(); got != "input_image" {
		t.Fatalf("content.1.type = %q, want input_image", got)
	}
	if got := gjson.GetBytes(out, "input.0.content.1.image_url").String(); got != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image_url = %q, want data URL", got)
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesPreservesNonImageMediaContent(t *testing.T) {
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", []byte(`{"model":"gpt-test","input":[{"type":"model_output","content":[{"type":"audio","mime_type":"audio/wav","data":"UklGRg=="},{"type":"video","mime_type":"video/mp4","data":"AAAAIGZ0eXA="},{"type":"document","mime_type":"application/pdf","data":"JVBERi0="}]}]}`), false)

	if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "output_text" {
		t.Fatalf("audio fallback type = %q, want output_text. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.0.content.1.type").String(); got != "output_file" {
		t.Fatalf("video type = %q, want output_file. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.0.content.2.type").String(); got != "output_file" {
		t.Fatalf("document type = %q, want output_file. Output: %s", got, string(out))
	}
	if gjson.GetBytes(out, "input.0.content.#(type==\"output_image\")").Exists() {
		t.Fatalf("non-image media must not be converted to output_image. Output: %s", string(out))
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesWithAssistantTextContent(t *testing.T) {
	raw := []byte(`{"model":"gpt-test","input":[{"type":"model_output","content":[{"type":"text","text":"hello"}]}]}`)
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", raw, false)
	if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "output_text" {
		t.Fatalf("content.0.type = %q, want output_text", got)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "hello" {
		t.Fatalf("content.0.text = %q, want hello", got)
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesWithUserObjectContent(t *testing.T) {
	raw := []byte(`{"model":"gpt-test","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]}]}`)
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", raw, false)
	if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "input_text" {
		t.Fatalf("content.0.type = %q, want input_text", got)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "hi" {
		t.Fatalf("content.0.text = %q, want hi", got)
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesWithStringFunctionArguments(t *testing.T) {
	raw := []byte(`{"model":"gpt-test","input":[{"type":"function_call","name":"lookup","call_id":"call_1","arguments":{"q":"x"}},{"type":"function_result","name":"lookup","call_id":"call_1","result":{"ok":true}}]}`)
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", raw, false)

	found := false
	gjson.GetBytes(out, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "function_call" {
			found = true
			if item.Get("arguments").Type != gjson.String {
				t.Fatalf("arguments should be string, got %v", item.Get("arguments").Type)
			}
			if got := item.Get("arguments").String(); got != `{"q":"x"}` {
				t.Fatalf("arguments = %q, want {\"q\":\"x\"}", got)
			}
		}
		return true
	})
	if !found {
		t.Fatal("function_call input not found")
	}
}

func TestConvertInteractionsRequestToOpenAIResponsesPreservesExpressibleFields(t *testing.T) {
	out := ConvertInteractionsRequestToOpenAIResponses("gpt-test", []byte(`{"model":"gpt-test","generation_config":{"max_output_tokens":321},"tool_choice":{"type":"function","function":{"name":"lookup"}},"response_modalities":["text","image"],"service_tier":"priority","store":true,"background":true,"webhook_config":{"url":"https://example.com"},"input":"hi"}`), false)
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "function" {
		t.Fatalf("tool_choice.type = %q, want function. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "lookup" {
		t.Fatalf("tool_choice.name = %q, want lookup. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "modalities.0").String(); got != "text" {
		t.Fatalf("modalities.0 = %q, want text. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "modalities.1").String(); got != "image" {
		t.Fatalf("modalities.1 = %q, want image. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "max_output_tokens").Int(); got != 321 {
		t.Fatalf("max_output_tokens = %d, want 321. Output: %s", got, string(out))
	}
	for _, path := range []string{"store", "background", "webhook_config"} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should not be forwarded. Output: %s", path, string(out))
		}
	}
}

func TestInteractionsResponsesRequestNormalizesStructuredOutput(t *testing.T) {
	responses := ConvertInteractionsRequestToOpenAIResponses(
		"gpt-test",
		[]byte(`{"model":"gpt-test","input":"hi","response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"},"strict":true}}}`),
		false,
	)
	if got := gjson.GetBytes(responses, "text.format.name").String(); got != "answer" {
		t.Fatalf("Responses format name = %q. Output: %s", got, responses)
	}
	if !gjson.GetBytes(responses, "text.format.schema").Exists() || gjson.GetBytes(responses, "text.format.json_schema").Exists() {
		t.Fatalf("Responses format has wrong shape: %s", responses)
	}

	interactions := ConvertOpenAIResponsesRequestToInteractions(
		"gpt-test",
		[]byte(`{"model":"gpt-test","input":"hi","text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true}}}`),
		false,
	)
	if got := gjson.GetBytes(interactions, "response_format.json_schema.name").String(); got != "answer" {
		t.Fatalf("Interactions format name = %q. Output: %s", got, interactions)
	}
	if !gjson.GetBytes(interactions, "response_format.json_schema.schema").Exists() {
		t.Fatalf("Interactions format schema is missing: %s", interactions)
	}
}

func TestInteractionsResponsesRequestNormalizesToolChoice(t *testing.T) {
	interactions := ConvertOpenAIResponsesRequestToInteractions(
		"gpt-test",
		[]byte(`{"model":"gpt-test","input":"hi","tool_choice":{"type":"function","name":"lookup"}}`),
		false,
	)
	if got := gjson.GetBytes(interactions, "generation_config.tool_choice.function.name").String(); got != "lookup" {
		t.Fatalf("Interactions tool choice name = %q. Output: %s", got, interactions)
	}

	responses := ConvertInteractionsRequestToOpenAIResponses("gpt-test", interactions, false)
	if got := gjson.GetBytes(responses, "tool_choice.name").String(); got != "lookup" {
		t.Fatalf("Responses tool choice name = %q. Output: %s", got, responses)
	}
	if gjson.GetBytes(responses, "tool_choice.function").Exists() {
		t.Fatalf("Responses tool choice retained Chat nesting: %s", responses)
	}
}

func TestConvertOpenAIResponsesRequestToInteractionsPreservesGenerationFields(t *testing.T) {
	out := ConvertOpenAIResponsesRequestToInteractions(
		"gpt-test",
		[]byte(`{"model":"gpt-test","input":"hi","max_output_tokens":654,"service_tier":"priority"}`),
		false,
	)
	if got := gjson.GetBytes(out, "generation_config.max_output_tokens").Int(); got != 654 {
		t.Fatalf("max_output_tokens = %d, want 654. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority. Output: %s", got, string(out))
	}
}
