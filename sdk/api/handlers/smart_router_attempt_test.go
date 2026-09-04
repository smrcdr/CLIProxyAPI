package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type fakeSmartRouterProtocolExecutor struct {
	mu             sync.Mutex
	nonStreamCalls []ProtocolExecutionRequest
	countCalls     []ProtocolExecutionRequest
	streamCalls    []ProtocolExecutionRequest
	response       ModelExecutionResponse
	stream         ModelExecutionStream
	errMsg         *interfaces.ErrorMessage
	streamErrMsg   *interfaces.ErrorMessage
}

func (f *fakeSmartRouterProtocolExecutor) ExecuteProtocolWithAuthManager(_ context.Context, request ProtocolExecutionRequest) (ModelExecutionResponse, *interfaces.ErrorMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nonStreamCalls = append(f.nonStreamCalls, request)
	return f.response, f.errMsg
}

func (f *fakeSmartRouterProtocolExecutor) ExecuteProtocolCountWithAuthManager(_ context.Context, request ProtocolExecutionRequest) (ModelExecutionResponse, *interfaces.ErrorMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countCalls = append(f.countCalls, request)
	return f.response, f.errMsg
}

func (f *fakeSmartRouterProtocolExecutor) ExecuteProtocolStreamWithAuthManager(_ context.Context, request ProtocolExecutionRequest) (ModelExecutionStream, *interfaces.ErrorMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streamCalls = append(f.streamCalls, request)
	return f.stream, f.streamErrMsg
}

func (f *fakeSmartRouterProtocolExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.nonStreamCalls) + len(f.countCalls) + len(f.streamCalls)
}

type fakeSmartRouterTranslators struct {
	request   bool
	stream    bool
	nonStream bool
}

func (f fakeSmartRouterTranslators) HasRequestTransformer(sdktranslator.Format, sdktranslator.Format) bool {
	return f.request
}

func (f fakeSmartRouterTranslators) HasStreamResponseTransformer(sdktranslator.Format, sdktranslator.Format) bool {
	return f.stream
}

func (f fakeSmartRouterTranslators) HasNonStreamResponseTransformer(sdktranslator.Format, sdktranslator.Format) bool {
	return f.nonStream
}

func smartRouterAttemptRequestForTest(entry, upstream config.RouterProtocol) smartrouter.AttemptRequest {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer downstream-secret")
	headers.Set("Cookie", "session=secret")
	headers.Set("X-SmartAPI-Affinity-Key", "client-spoof")
	headers.Set("X-SmartRouter-Diagnostics", "client-spoof")
	headers.Set("X-Request-ID", "request-1")
	return smartrouter.AttemptRequest{
		Selection: smartrouter.Selection{
			Route: smartrouter.Route{
				ID:            "route-primary",
				UpstreamID:    "pool-eu",
				UpstreamModel: "upstream-model",
			},
			Upstream: smartrouter.Upstream{
				ID:                      "pool-eu",
				Protocol:                upstream,
				Headers:                 map[string]string{"X-Upstream-Static": "configured"},
				TrustedPool:             true,
				ForwardSmartAPIAffinity: true,
			},
			SnapshotRevision: 7,
		},
		PublicModel:   "public-model",
		AffinityKey:   "internal-affinity",
		EntryProtocol: entry,
		Endpoint:      config.RouterEndpointResponses,
		Body:          []byte(`{"model":"public-model","input":"hello"}`),
		Headers:       headers,
		Query:         url.Values{"beta": []string{"true"}},
	}
}

func TestSmartRouterAttemptExecutorUsesRouteLevelRuntime(t *testing.T) {
	fake := &fakeSmartRouterProtocolExecutor{response: ModelExecutionResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"X-Upstream": []string{"yes"}},
		Body:       []byte(`{"id":"response-1","model":"upstream-model","usage":{"input_tokens":3}}`),
	}}
	executor := NewSmartRouterAttemptExecutor(fake)
	executor.translators = fakeSmartRouterTranslators{request: true, nonStream: true}
	request := smartRouterAttemptRequestForTest(
		config.RouterProtocolOpenAIResponses,
		config.RouterProtocolAnthropicMessages,
	)

	response, err := executor.ExecuteAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("ExecuteAttempt() error = %v", err)
	}
	if response.StatusCode != http.StatusOK || response.Headers.Get("X-Upstream") != "yes" {
		t.Fatalf("response = %#v", response)
	}
	if body := string(response.Body); !strings.Contains(body, `"model":"public-model"`) || strings.Contains(body, `"model":"upstream-model"`) {
		t.Fatalf("public model was not restored: %s", body)
	}
	if fake.callCount() != 1 {
		t.Fatalf("runtime calls = %d, want 1", fake.callCount())
	}

	call := fake.nonStreamCalls[0]
	if call.EntryProtocol != sdktranslator.FormatOpenAIResponse.String() ||
		call.ExitProtocol != sdktranslator.FormatOpenAIResponse.String() ||
		call.ForcedProvider != "smartrouter-pool-eu" ||
		call.AuthSelectionModel != "upstream-model" ||
		call.Model != "upstream-model" ||
		call.Stream {
		t.Fatalf("protocol request = %#v", call)
	}
	for _, forbidden := range []string{"Authorization", "Cookie", "X-SmartRouter-Diagnostics"} {
		if value := call.Headers.Get(forbidden); value != "" {
			t.Fatalf("forwarded forbidden header %s=%q", forbidden, value)
		}
	}
	if got := call.Headers.Get(smartRouterAffinityHeader); got != "internal-affinity" {
		t.Fatalf("affinity header = %q, want internal value", got)
	}
	if got := call.Headers.Get("X-Upstream-Static"); got != "configured" {
		t.Fatalf("configured header = %q", got)
	}
	if got := call.Headers.Get("X-Request-ID"); got != "request-1" {
		t.Fatalf("ordinary header = %q", got)
	}
}

func TestSmartRouterAttemptExecutorUsesCountRuntimeForCountEndpoint(t *testing.T) {
	fake := &fakeSmartRouterProtocolExecutor{response: ModelExecutionResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"input_tokens":17}`),
	}}
	executor := NewSmartRouterAttemptExecutor(fake)
	executor.translators = fakeSmartRouterTranslators{request: true, nonStream: true}
	request := smartRouterAttemptRequestForTest(
		config.RouterProtocolAnthropicMessages,
		config.RouterProtocolOpenAIChatCompletions,
	)
	request.Endpoint = config.RouterEndpointCountTokens
	request.Body = []byte(`{"model":"public-model","messages":[{"role":"user","content":"hello"}]}`)

	response, err := executor.ExecuteAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("ExecuteAttempt() error = %v", err)
	}
	if response.StatusCode != http.StatusOK || string(response.Body) != `{"input_tokens":17}` {
		t.Fatalf("response = %#v", response)
	}
	if len(fake.countCalls) != 1 || len(fake.nonStreamCalls) != 0 || len(fake.streamCalls) != 0 {
		t.Fatalf("runtime calls = count %d inference %d stream %d", len(fake.countCalls), len(fake.nonStreamCalls), len(fake.streamCalls))
	}
	if fake.countCalls[0].Stream {
		t.Fatal("count request was marked as streaming")
	}
}

func TestSmartRouterAttemptExecutorUsesImageRuntimeWithoutProtocolTranslation(t *testing.T) {
	fake := &fakeSmartRouterProtocolExecutor{response: ModelExecutionResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"data":[{"b64_json":"aW1hZ2U="}]}`),
	}}
	executor := NewSmartRouterAttemptExecutor(fake)
	request := smartRouterAttemptRequestForTest(
		config.RouterProtocolOpenAIResponses,
		config.RouterProtocolOpenAIChatCompletions,
	)
	request.Endpoint = config.RouterEndpointImages
	request.Body = []byte(`{"model":"public-model","prompt":"draw","n":1,"size":"1536x1024","quality":"high","output_format":"png","response_format":"b64_json"}`)

	response, err := executor.ExecuteAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("ExecuteAttempt() error = %v", err)
	}
	if response.StatusCode != http.StatusOK || fake.callCount() != 1 || len(fake.nonStreamCalls) != 1 {
		t.Fatalf("response = %#v, calls = %d", response, fake.callCount())
	}
	call := fake.nonStreamCalls[0]
	if call.EntryProtocol != "openai-image" || call.ExitProtocol != "openai-image" ||
		call.Model != "upstream-model" || call.AuthSelectionModel != "upstream-model" || call.Stream {
		t.Fatalf("image protocol request = %#v", call)
	}
	for _, path := range []string{"prompt", "n", "size", "quality", "output_format", "response_format"} {
		if !gjson.GetBytes(call.Body, path).Exists() {
			t.Fatalf("image parameter %q was lost: %s", path, call.Body)
		}
	}
}

func TestSmartRouterAttemptExecutorRejectsAnthropicImageUpstreamBeforeExecution(t *testing.T) {
	fake := &fakeSmartRouterProtocolExecutor{}
	executor := NewSmartRouterAttemptExecutor(fake)
	request := smartRouterAttemptRequestForTest(
		config.RouterProtocolOpenAIResponses,
		config.RouterProtocolAnthropicMessages,
	)
	request.Endpoint = config.RouterEndpointImages

	response, err := executor.ExecuteAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("ExecuteAttempt() error = %v", err)
	}
	if response.StatusCode != http.StatusUnprocessableEntity || fake.callCount() != 0 {
		t.Fatalf("response = %#v, calls = %d", response, fake.callCount())
	}
}

func TestSmartRouterAttemptExecutorPreflightStopsUnsupportedConversion(t *testing.T) {
	fake := &fakeSmartRouterProtocolExecutor{}
	executor := NewSmartRouterAttemptExecutor(fake)
	executor.translators = fakeSmartRouterTranslators{}
	request := smartRouterAttemptRequestForTest(
		config.RouterProtocolOpenAIResponses,
		config.RouterProtocolAnthropicMessages,
	)

	response, err := executor.ExecuteAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("ExecuteAttempt() error = %v", err)
	}
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("StatusCode = %d, want 422", response.StatusCode)
	}
	if fake.callCount() != 0 {
		t.Fatalf("unsupported conversion reached runtime %d time(s)", fake.callCount())
	}
}

func TestSmartRouterAttemptExecutorBuiltInProtocolMatrix(t *testing.T) {
	executor := NewSmartRouterAttemptExecutor(&fakeSmartRouterProtocolExecutor{})
	protocols := []config.RouterProtocol{
		config.RouterProtocolOpenAIChatCompletions,
		config.RouterProtocolOpenAIResponses,
		config.RouterProtocolAnthropicMessages,
	}

	for _, entry := range protocols {
		for _, upstream := range protocols {
			entry := entry
			upstream := upstream
			t.Run(string(entry)+"_to_"+string(upstream), func(t *testing.T) {
				entryFormat, err := smartRouterProtocolFormat(entry)
				if err != nil {
					t.Fatalf("entry format: %v", err)
				}
				upstreamFormat, err := smartRouterProtocolFormat(upstream)
				if err != nil {
					t.Fatalf("upstream format: %v", err)
				}
				for _, stream := range []bool{false, true} {
					if err := executor.preflight(entryFormat, upstreamFormat, stream, []byte(`{"model":"test"}`)); err != nil {
						t.Fatalf("preflight(stream=%v) error = %v", stream, err)
					}
				}
			})
		}
	}
}

func TestSmartRouterAttemptExecutorRejectsLossyFeatureBeforeUpstream(t *testing.T) {
	tests := []struct {
		name     string
		entry    config.RouterProtocol
		upstream config.RouterProtocol
		body     string
	}{
		{
			name:     "chat stop to responses",
			entry:    config.RouterProtocolOpenAIChatCompletions,
			upstream: config.RouterProtocolOpenAIResponses,
			body:     `{"model":"public-model","messages":[{"role":"user","content":"hello"}],"stop":"END"}`,
		},
		{
			name:     "chat structured output to anthropic",
			entry:    config.RouterProtocolOpenAIChatCompletions,
			upstream: config.RouterProtocolAnthropicMessages,
			body:     `{"model":"public-model","messages":[{"role":"user","content":"hello"}],"response_format":{"type":"json_object"}}`,
		},
		{
			name:     "responses custom tool to anthropic",
			entry:    config.RouterProtocolOpenAIResponses,
			upstream: config.RouterProtocolAnthropicMessages,
			body:     `{"model":"public-model","input":"hello","tools":[{"type":"custom","name":"apply_patch"}]}`,
		},
		{
			name:     "anthropic stop to responses",
			entry:    config.RouterProtocolAnthropicMessages,
			upstream: config.RouterProtocolOpenAIResponses,
			body:     `{"model":"public-model","messages":[{"role":"user","content":"hello"}],"stop_sequences":["END"]}`,
		},
		{
			name:     "anthropic structured output to chat",
			entry:    config.RouterProtocolAnthropicMessages,
			upstream: config.RouterProtocolOpenAIChatCompletions,
			body:     `{"model":"public-model","messages":[{"role":"user","content":"hello"}],"output_config":{"format":{"type":"json_schema"}}}`,
		},
		{
			name:     "rich chat tool result to responses",
			entry:    config.RouterProtocolOpenAIChatCompletions,
			upstream: config.RouterProtocolOpenAIResponses,
			body:     `{"model":"public-model","messages":[{"role":"tool","tool_call_id":"call_1","content":[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeSmartRouterProtocolExecutor{}
			executor := NewSmartRouterAttemptExecutor(fake)
			request := smartRouterAttemptRequestForTest(test.entry, test.upstream)
			request.Body = []byte(test.body)

			response, err := executor.ExecuteAttempt(context.Background(), request)
			if err != nil {
				t.Fatalf("ExecuteAttempt() error = %v", err)
			}
			if response.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("StatusCode = %d, want 422", response.StatusCode)
			}
			if fake.callCount() != 0 {
				t.Fatalf("lossy conversion reached runtime %d time(s)", fake.callCount())
			}
		})
	}
}

func TestSmartRouterAttemptExecutorPreservesUpstreamStatusWithoutPayload(t *testing.T) {
	fake := &fakeSmartRouterProtocolExecutor{errMsg: &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("secret upstream payload"),
		Addon:      http.Header{"Retry-After": []string{"30"}},
	}}
	executor := NewSmartRouterAttemptExecutor(fake)
	request := smartRouterAttemptRequestForTest(
		config.RouterProtocolOpenAIResponses,
		config.RouterProtocolOpenAIResponses,
	)

	response, err := executor.ExecuteAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("ExecuteAttempt() error = %v", err)
	}
	if response.StatusCode != http.StatusTooManyRequests || response.Headers.Get("Retry-After") != "30" {
		t.Fatalf("response = %#v", response)
	}
	if len(response.Body) != 0 {
		t.Fatalf("upstream error payload leaked into response: %q", response.Body)
	}
}

func TestSmartRouterAttemptExecutorStreamsTranslatedChunks(t *testing.T) {
	source := make(chan ModelExecutionChunk, 2)
	source <- ModelExecutionChunk{Payload: []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"upstream-model\"}}")}
	source <- ModelExecutionChunk{Err: &ModelExecutionStreamError{StatusCode: http.StatusBadRequest, Message: "secret detail"}}
	close(source)

	fake := &fakeSmartRouterProtocolExecutor{stream: ModelExecutionStream{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"X-Upstream": []string{"yes"}},
		Chunks:     source,
	}}
	executor := NewSmartRouterAttemptExecutor(fake)
	executor.translators = fakeSmartRouterTranslators{request: true, stream: true}
	request := smartRouterAttemptRequestForTest(
		config.RouterProtocolOpenAIResponses,
		config.RouterProtocolAnthropicMessages,
	)

	stream, err := executor.ExecuteStreamAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("ExecuteStreamAttempt() error = %v", err)
	}
	first := <-stream.Chunks
	if body := string(first.Data); !strings.Contains(body, `"model":"public-model"`) || strings.Contains(body, `"model":"upstream-model"`) {
		t.Fatalf("stream model was not restored: %s", body)
	}
	second := <-stream.Chunks
	var carrier interface{ StatusCode() int }
	if !errors.As(second.Err, &carrier) || carrier.StatusCode() != http.StatusBadRequest {
		t.Fatalf("stream error = %T %v, want status-bearing 400", second.Err, second.Err)
	}
	if strings.Contains(second.Err.Error(), "secret detail") {
		t.Fatalf("stream error leaked upstream detail: %v", second.Err)
	}
}

func TestSmartRouterAttemptExecutorDoesNotForwardAffinityToDirectUpstream(t *testing.T) {
	fake := &fakeSmartRouterProtocolExecutor{response: ModelExecutionResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"model":"upstream-model"}`),
	}}
	executor := NewSmartRouterAttemptExecutor(fake)
	request := smartRouterAttemptRequestForTest(
		config.RouterProtocolOpenAIResponses,
		config.RouterProtocolOpenAIResponses,
	)
	request.Selection.Upstream.TrustedPool = false
	request.Selection.Upstream.ForwardSmartAPIAffinity = true

	if _, err := executor.ExecuteAttempt(context.Background(), request); err != nil {
		t.Fatalf("ExecuteAttempt() error = %v", err)
	}
	if value := fake.nonStreamCalls[0].Headers.Get(smartRouterAffinityHeader); value != "" {
		t.Fatalf("direct upstream received affinity %q", value)
	}
}
