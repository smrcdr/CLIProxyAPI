package smartrouter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
)

// fakeStreamAttempt scripts one upstream stream attempt.
type fakeStreamAttempt struct {
	statusCode int
	headers    http.Header
	chunks     []StreamChunk
	err        error
	// nilChunks scripts a 2xx attempt whose executor returned no channel.
	nilChunks bool
	// block keeps the stream open until the attempt context is cancelled.
	block bool
}

type fakeStreamExecutor struct {
	mu       sync.Mutex
	calls    []AttemptRequest
	script   []fakeStreamAttempt
	fallback fakeStreamAttempt
	closes   atomic.Int64
	finished atomic.Int64
	hook     func(call int, request AttemptRequest)
}

func (f *fakeStreamExecutor) ExecuteStreamAttempt(ctx context.Context, request AttemptRequest) (*StreamAttempt, error) {
	f.mu.Lock()
	call := len(f.calls)
	f.calls = append(f.calls, request)
	attempt := f.fallback
	if call < len(f.script) {
		attempt = f.script[call]
	}
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook(call, request)
	}
	if attempt.err != nil {
		return nil, attempt.err
	}
	result := &StreamAttempt{StatusCode: attempt.statusCode, Headers: attempt.headers}
	if attempt.nilChunks {
		return result, nil
	}

	chunks := make(chan StreamChunk)
	done := make(chan struct{})
	result.Chunks = chunks
	result.Close = func() {
		f.closes.Add(1)
		<-done
	}
	go func() {
		defer close(done)
		defer close(chunks)
		defer f.finished.Add(1)
		for _, chunk := range attempt.chunks {
			select {
			case chunks <- chunk:
			case <-ctx.Done():
				return
			}
		}
		if attempt.block {
			<-ctx.Done()
		}
	}()
	return result, nil
}

func (f *fakeStreamExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeStreamExecutor) call(index int) AttemptRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[index]
}

// recordingSink captures the committed downstream stream.
type recordingSink struct {
	mu         sync.Mutex
	commits    []StreamCommit
	sends      [][]byte
	failures   []*ExecutionError
	commitErr  error
	sendErr    error
	sendErrAt  int
	sendCalled int
}

func (s *recordingSink) Commit(commit StreamCommit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits = append(s.commits, commit)
	return s.commitErr
}

func (s *recordingSink) Send(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends = append(s.sends, append([]byte(nil), data...))
	s.sendCalled++
	if s.sendErr != nil && s.sendCalled > s.sendErrAt {
		return s.sendErr
	}
	return nil
}

func (s *recordingSink) Fail(err *ExecutionError) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, err)
	return nil
}

func (s *recordingSink) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var builder strings.Builder
	for _, chunk := range s.sends {
		builder.Write(chunk)
	}
	return builder.String()
}

func (s *recordingSink) commitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.commits)
}

func (s *recordingSink) failureCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.failures)
}

func streamCoordinatorForTest(t *testing.T, executor StreamAttemptExecutor, routeCount int) (*StreamCoordinator, *Selector) {
	t.Helper()
	snapshot, err := CompileSnapshot(coordinatorTestConfig(routeCount), 1)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	selector := NewSelector(NewSnapshotStore(snapshot), NewCircuitStore(DefaultCircuitPolicy()))
	selector.now = func() time.Time { return coordinatorTestTime }
	return NewStreamCoordinator(selector, executor), selector
}

func streamExecutionRequest() ExecutionRequest {
	request := textExecutionRequest()
	request.Stream = true
	return request
}

func dataChunk(payload string) StreamChunk {
	return StreamChunk{Data: []byte("data: " + payload + "\n\n")}
}

func responsesDelta(text string) StreamChunk {
	return dataChunk(fmt.Sprintf(`{"type":"response.output_text.delta","delta":%q}`, text))
}

func okStreamAttempt() fakeStreamAttempt {
	return fakeStreamAttempt{
		statusCode: 200,
		headers:    http.Header{"X-Upstream": []string{"yes"}},
		chunks: []StreamChunk{
			dataChunk(`{"type":"response.created"}`),
			responsesDelta("hello"),
			dataChunk(`{"type":"response.completed","response":{"usage":{"input_tokens":3}}}`),
			{Data: []byte("data: [DONE]\n\n")},
		},
	}
}

type streamStatusError struct {
	code int
}

func (e streamStatusError) Error() string       { return fmt.Sprintf("upstream stream failed: %d", e.code) }
func (e streamStatusError) StatusCode() int     { return e.code }
func (e streamStatusError) Unwrap() error       { return nil }
func statusChunkError(code int) StreamChunk     { return StreamChunk{Err: streamStatusError{code: code}} }
func plainChunkError(text string) StreamChunk   { return StreamChunk{Err: errors.New(text)} }
func headersWith(key, value string) http.Header { return http.Header{key: []string{value}} }

func TestStreamCoordinatorReportsFinalUsage(t *testing.T) {
	executor := &fakeStreamExecutor{fallback: okStreamAttempt()}
	coordinator, _ := streamCoordinatorForTest(t, executor, 1)
	sink := &recordingSink{}

	result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if result.UsageMissing || result.Usage == nil {
		t.Fatalf("usage = %#v, missing = %t", result.Usage, result.UsageMissing)
	}
	assertUsageToken(t, "input", result.Usage.InputTokens, usageToken(3))
	if result.Usage.OutputTokens != nil || result.Usage.TotalTokens != nil {
		t.Fatalf("unknown usage fields became known: %#v", result.Usage)
	}
}

func TestStreamCoordinatorMarksCompletedStreamWithoutUsage(t *testing.T) {
	executor := &fakeStreamExecutor{fallback: fakeStreamAttempt{
		statusCode: http.StatusOK,
		chunks:     []StreamChunk{responsesDelta("hello")},
	}}
	coordinator, _ := streamCoordinatorForTest(t, executor, 1)
	sink := &recordingSink{}

	result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if !result.UsageMissing || result.Usage != nil {
		t.Fatalf("usage = %#v, missing = %t", result.Usage, result.UsageMissing)
	}
}

func TestStreamCoordinatorSuccessCommitsOnce(t *testing.T) {
	executor := &fakeStreamExecutor{fallback: okStreamAttempt()}
	coordinator, _ := streamCoordinatorForTest(t, executor, 2)
	sink := &recordingSink{}

	result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if !result.Success || result.StreamState != StreamStateCompleted || !result.Committed {
		t.Fatalf("result = %#v, want completed committed success", result)
	}
	if executor.callCount() != 1 || len(result.Attempts) != 1 {
		t.Fatalf("calls = %d, attempts = %#v; want one attempt", executor.callCount(), result.Attempts)
	}
	if sink.commitCount() != 1 || sink.failureCount() != 0 {
		t.Fatalf("commits = %d, failures = %d", sink.commitCount(), sink.failureCount())
	}
	commit := sink.commits[0]
	if commit.StatusCode != 200 || commit.RouteID != "route-1" || commit.UpstreamID != "upstream-1" || commit.SnapshotRevision != 1 {
		t.Fatalf("commit = %#v", commit)
	}
	if commit.Headers.Get("X-Upstream") != "yes" {
		t.Fatalf("commit headers = %#v", commit.Headers)
	}
	body := sink.body()
	for _, want := range []string{"response.created", "hello", "response.completed", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("downstream body %q is missing %q", body, want)
		}
	}
	attempt := executor.call(0)
	if attempt.Selection.Route.UpstreamModel != "upstream-model-1" || attempt.Endpoint != config.RouterEndpointResponses {
		t.Fatalf("attempt request = %#v", attempt)
	}
}

func TestStreamCoordinatorPreCommitFailover(t *testing.T) {
	cases := []struct {
		name         string
		first        fakeStreamAttempt
		wantCategory FailureCategory
		wantStatus   int
		checkCircuit func(t *testing.T, status CircuitStatus)
	}{
		{
			name:         "non-2xx before stream start",
			first:        fakeStreamAttempt{statusCode: 503},
			wantCategory: FailureTransient,
			wantStatus:   503,
		},
		{
			name:         "auth failure before stream start",
			first:        fakeStreamAttempt{statusCode: 401},
			wantCategory: FailureAuth,
			wantStatus:   401,
			checkCircuit: func(t *testing.T, status CircuitStatus) {
				t.Helper()
				if status.State != CircuitOpen || !status.RequiresReset {
					t.Fatalf("circuit = %#v, want open awaiting reset", status)
				}
			},
		},
		{
			name:         "rate limit before stream start",
			first:        fakeStreamAttempt{statusCode: 429, headers: headersWith("Retry-After", "90")},
			wantCategory: FailureRateLimit,
			wantStatus:   429,
			checkCircuit: func(t *testing.T, status CircuitStatus) {
				t.Helper()
				want := coordinatorTestTime.Add(90 * time.Second)
				if status.State != CircuitOpen || !status.OpenUntil.Equal(want) {
					t.Fatalf("circuit = %#v, want open until %s", status, want)
				}
			},
		},
		{
			name:         "connection failure before headers",
			first:        fakeStreamAttempt{err: errors.New("connection refused")},
			wantCategory: FailureTransient,
			wantStatus:   0,
		},
		{
			name: "malformed pre-semantic event",
			first: fakeStreamAttempt{
				statusCode: 200,
				chunks:     []StreamChunk{{Data: []byte("data: {not json\n\n")}},
			},
			wantCategory: FailureProtocol,
			wantStatus:   200,
		},
		{
			name: "upstream error event before commitment",
			first: fakeStreamAttempt{
				statusCode: 200,
				chunks:     []StreamChunk{dataChunk(`{"type":"error","error":{"message":"boom"}}`)},
			},
			wantCategory: FailureProtocol,
			wantStatus:   200,
		},
		{
			name: "chunk error before commitment",
			first: fakeStreamAttempt{
				statusCode: 200,
				chunks:     []StreamChunk{{Data: []byte(": keepalive\n\n")}, statusChunkError(502)},
			},
			wantCategory: FailureTransient,
			wantStatus:   200,
		},
		{
			name:         "empty stream before commitment",
			first:        fakeStreamAttempt{statusCode: 200},
			wantCategory: FailureProtocol,
			wantStatus:   200,
		},
		{
			name:         "missing chunk channel",
			first:        fakeStreamAttempt{statusCode: 200, nilChunks: true},
			wantCategory: FailureProtocol,
			wantStatus:   200,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			executor := &fakeStreamExecutor{script: []fakeStreamAttempt{test.first}, fallback: okStreamAttempt()}
			coordinator, selector := streamCoordinatorForTest(t, executor, 2)
			sink := &recordingSink{}

			result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v, want failover success", err)
			}
			if !result.Success || len(result.Attempts) != 2 || executor.callCount() != 2 {
				t.Fatalf("result = %#v, calls = %d; want one failover", result, executor.callCount())
			}
			first := result.Attempts[0]
			if first.RouteID != "route-1" || first.FailureCategory != test.wantCategory || first.StatusCode != test.wantStatus {
				t.Fatalf("first attempt = %#v, want category %q status %d", first, test.wantCategory, test.wantStatus)
			}
			if result.RouteID != "route-2" {
				t.Fatalf("final route = %q, want route-2", result.RouteID)
			}
			if sink.commitCount() != 1 {
				t.Fatalf("commits = %d, want exactly one committed attempt", sink.commitCount())
			}
			if sink.commits[0].RouteID != "route-2" {
				t.Fatalf("committed route = %q, want route-2", sink.commits[0].RouteID)
			}
			if strings.Count(sink.body(), "response.created") != 1 {
				t.Fatalf("downstream body duplicated content: %q", sink.body())
			}
			if test.checkCircuit != nil {
				test.checkCircuit(t, selector.CircuitStatus("route-1"))
			}
		})
	}
}

func TestStreamCoordinatorKeepalivesDoNotCommit(t *testing.T) {
	preludes := map[string][]StreamChunk{
		"sse comments": {
			{Data: []byte(": keepalive\n\n")},
			{Data: []byte(": ping\n\n")},
		},
		"blank lines": {
			{Data: []byte("\n\n")},
			{Data: []byte("\n")},
		},
		"event and id fields": {
			{Data: []byte("event: response.created\n")},
			{Data: []byte("id: 1\n")},
			{Data: []byte("retry: 500\n\n")},
		},
		"done sentinel only": {
			{Data: []byte("data: [DONE]\n\n")},
		},
	}

	for name, prelude := range preludes {
		t.Run(name, func(t *testing.T) {
			first := fakeStreamAttempt{
				statusCode: 200,
				chunks:     append(append([]StreamChunk(nil), prelude...), statusChunkError(503)),
			}
			executor := &fakeStreamExecutor{script: []fakeStreamAttempt{first}, fallback: okStreamAttempt()}
			coordinator, _ := streamCoordinatorForTest(t, executor, 2)
			sink := &recordingSink{}

			result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
			if err != nil || !result.Success {
				t.Fatalf("ExecuteStream() = %#v, %v; want failover success", result, err)
			}
			if executor.callCount() != 2 || sink.commitCount() != 1 {
				t.Fatalf("calls = %d, commits = %d; keepalive prevented failover", executor.callCount(), sink.commitCount())
			}
			if sink.commits[0].RouteID != "route-2" {
				t.Fatalf("committed route = %q, want route-2", sink.commits[0].RouteID)
			}
			for _, chunk := range sink.sends {
				if strings.Contains(string(chunk), "keepalive") || strings.Contains(string(chunk), "retry:") {
					t.Fatalf("uncommitted prelude leaked downstream: %q", chunk)
				}
			}
		})
	}
}

func TestStreamCoordinatorSemanticEventPreventsFailover(t *testing.T) {
	protocols := []struct {
		name     string
		protocol config.RouterProtocol
		semantic StreamChunk
	}{
		{
			name:     "openai responses",
			protocol: config.RouterProtocolOpenAIResponses,
			semantic: responsesDelta("partial"),
		},
		{
			name:     "openai chat completions",
			protocol: config.RouterProtocolOpenAIChatCompletions,
			semantic: dataChunk(`{"object":"chat.completion.chunk","choices":[{"delta":{"content":"partial"}}]}`),
		},
		{
			name:     "anthropic messages",
			protocol: config.RouterProtocolAnthropicMessages,
			semantic: dataChunk(`{"type":"content_block_delta","delta":{"text":"partial"}}`),
		},
	}

	for _, test := range protocols {
		t.Run(test.name, func(t *testing.T) {
			first := fakeStreamAttempt{
				statusCode: 200,
				chunks:     []StreamChunk{test.semantic, statusChunkError(503)},
			}
			executor := &fakeStreamExecutor{script: []fakeStreamAttempt{first}, fallback: okStreamAttempt()}
			coordinator, _ := streamCoordinatorForTest(t, executor, 3)
			sink := &recordingSink{}
			request := streamExecutionRequest()
			request.EntryProtocol = test.protocol

			result, err := coordinator.ExecuteStream(context.Background(), request, sink)
			var execErr *ExecutionError
			if !errors.As(err, &execErr) {
				t.Fatalf("ExecuteStream() error = %T %v, want *ExecutionError", err, err)
			}
			if execErr.Category != FailureTransient || execErr.RouteID != "route-1" {
				t.Fatalf("partial failure error = %#v", execErr)
			}
			if executor.callCount() != 1 || len(result.Attempts) != 1 {
				t.Fatalf("post-commit failover happened: calls = %d, attempts = %#v", executor.callCount(), result.Attempts)
			}
			if result.StreamState != StreamStateFailedPartial || !result.Committed || result.Success {
				t.Fatalf("result = %#v, want committed partial failure", result)
			}
			if sink.commitCount() != 1 || sink.failureCount() != 1 {
				t.Fatalf("commits = %d, failures = %d", sink.commitCount(), sink.failureCount())
			}
			if body := sink.body(); strings.Count(body, "partial") != 1 {
				t.Fatalf("downstream body duplicated content: %q", body)
			}
		})
	}
}

func TestStreamCoordinatorPostCommitFailureDoesNotDuplicateContent(t *testing.T) {
	first := fakeStreamAttempt{
		statusCode: 200,
		chunks: []StreamChunk{
			dataChunk(`{"type":"response.created"}`),
			responsesDelta("chunk-one"),
			responsesDelta("chunk-two"),
			plainChunkError("upstream reset"),
		},
	}
	executor := &fakeStreamExecutor{script: []fakeStreamAttempt{first}, fallback: okStreamAttempt()}
	coordinator, _ := streamCoordinatorForTest(t, executor, 3)
	sink := &recordingSink{}

	result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
	var execErr *ExecutionError
	if !errors.As(err, &execErr) || execErr.Category != FailureTransient {
		t.Fatalf("ExecuteStream() error = %v, want transient partial failure", err)
	}
	if executor.callCount() != 1 {
		t.Fatalf("executor calls = %d, want no failover after commitment", executor.callCount())
	}
	body := sink.body()
	if strings.Count(body, "chunk-one") != 1 || strings.Count(body, "chunk-two") != 1 {
		t.Fatalf("downstream body duplicated content: %q", body)
	}
	if !result.Committed || result.StreamState != StreamStateFailedPartial {
		t.Fatalf("result = %#v", result)
	}
	if sink.failureCount() != 1 || sink.failures[0].RouteID != "route-1" {
		t.Fatalf("sink failures = %#v", sink.failures)
	}
	rendered := err.Error() + fmt.Sprintf("%+v", result)
	if strings.Contains(rendered, "upstream reset") {
		t.Fatalf("diagnostics leaked the upstream error text: %s", rendered)
	}
}

func TestStreamCoordinatorSplitSemanticEventCommitsOnce(t *testing.T) {
	first := fakeStreamAttempt{
		statusCode: 200,
		chunks: []StreamChunk{
			{Data: []byte("data: {\"type\":\"resp")},
			{Data: []byte("onse.created\"}\n\n")},
			responsesDelta("split"),
		},
	}
	executor := &fakeStreamExecutor{script: []fakeStreamAttempt{first}, fallback: okStreamAttempt()}
	coordinator, _ := streamCoordinatorForTest(t, executor, 2)
	sink := &recordingSink{}

	result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
	if err != nil || !result.Success {
		t.Fatalf("ExecuteStream() = %#v, %v; want success on the first route", result, err)
	}
	if executor.callCount() != 1 {
		t.Fatalf("split event caused a failover: calls = %d", executor.callCount())
	}
	if body := sink.body(); !strings.Contains(body, `{"type":"response.created"}`) || !strings.Contains(body, "split") {
		t.Fatalf("downstream body = %q, want the reassembled event", body)
	}
}

func TestStreamCoordinatorAcceptsRuntimeChunkBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		protocol config.RouterProtocol
		chunks   []StreamChunk
		want     string
	}{
		{
			name:     "responses SSEEventData without trailing newline",
			protocol: config.RouterProtocolOpenAIResponses,
			chunks: []StreamChunk{{Data: translatorcommon.SSEEventData(
				"response.created",
				[]byte(`{"type":"response.created"}`),
			)}},
			want: "response.created",
		},
		{
			name:     "chat bare JSON without trailing newline",
			protocol: config.RouterProtocolOpenAIChatCompletions,
			chunks: []StreamChunk{{Data: []byte(
				`{"object":"chat.completion.chunk","choices":[{"delta":{"content":"hello"}}]}`,
			)}},
			want: "hello",
		},
		{
			name:     "split bare JSON without trailing newline",
			protocol: config.RouterProtocolOpenAIResponses,
			chunks: []StreamChunk{
				{Data: []byte(`{"type":"response.output_text`)},
				{Data: []byte(`.delta","delta":"split"}`)},
			},
			want: "split",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &fakeStreamExecutor{fallback: fakeStreamAttempt{
				statusCode: 200,
				chunks:     test.chunks,
			}}
			coordinator, _ := streamCoordinatorForTest(t, executor, 2)
			sink := &recordingSink{}
			request := streamExecutionRequest()
			request.EntryProtocol = test.protocol

			result, err := coordinator.ExecuteStream(context.Background(), request, sink)
			if err != nil || !result.Success {
				t.Fatalf("ExecuteStream() = %#v, %v; want first-route success", result, err)
			}
			if executor.callCount() != 1 || sink.commitCount() != 1 {
				t.Fatalf("calls = %d, commits = %d; want one committed attempt", executor.callCount(), sink.commitCount())
			}
			if body := sink.body(); !strings.Contains(body, test.want) {
				t.Fatalf("downstream body = %q, want %q", body, test.want)
			}
		})
	}
}

func TestStreamCoordinatorOversizedSemanticPreludeDoesNotCommit(t *testing.T) {
	payload := `{"type":"response.output_text.delta","delta":"` +
		strings.Repeat("x", maxUncommittedBytes) + `"}`
	first := fakeStreamAttempt{
		statusCode: 200,
		chunks:     []StreamChunk{{Data: []byte("data: " + payload)}},
	}
	executor := &fakeStreamExecutor{script: []fakeStreamAttempt{first}, fallback: okStreamAttempt()}
	coordinator, _ := streamCoordinatorForTest(t, executor, 2)
	sink := &recordingSink{}

	result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
	if err != nil || !result.Success {
		t.Fatalf("ExecuteStream() = %#v, %v; want failover success", result, err)
	}
	if executor.callCount() != 2 || len(result.Attempts) != 2 {
		t.Fatalf("calls = %d, attempts = %#v; want one protocol failover", executor.callCount(), result.Attempts)
	}
	if result.Attempts[0].FailureCategory != FailureProtocol {
		t.Fatalf("first attempt = %#v, want protocol failure", result.Attempts[0])
	}
	if sink.commitCount() != 1 || sink.commits[0].RouteID != "route-2" {
		t.Fatalf("commits = %#v, want only route-2", sink.commits)
	}
	if strings.Contains(sink.body(), strings.Repeat("x", 128)) {
		t.Fatalf("oversized first-route prelude leaked downstream")
	}
}

func TestStreamCoordinatorPreCommitClientChunkErrorIsTerminal(t *testing.T) {
	executor := &fakeStreamExecutor{
		script: []fakeStreamAttempt{{
			statusCode: 200,
			chunks:     []StreamChunk{statusChunkError(400)},
		}},
		fallback: okStreamAttempt(),
	}
	coordinator, selector := streamCoordinatorForTest(t, executor, 2)
	sink := &recordingSink{}

	result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
	var execErr *ExecutionError
	if !errors.As(err, &execErr) || execErr.Category != FailureClient {
		t.Fatalf("ExecuteStream() error = %v, want terminal client error", err)
	}
	if result.Success || executor.callCount() != 1 || len(result.Attempts) != 1 {
		t.Fatalf("result = %#v, calls = %d; client error must not fail over", result, executor.callCount())
	}
	if sink.commitCount() != 0 || sink.failureCount() != 0 {
		t.Fatalf("terminal pre-commit error touched sink: commits = %d failures = %d", sink.commitCount(), sink.failureCount())
	}
	if status := selector.CircuitStatus("route-1"); status.State != CircuitClosed || status.FailureCount != 0 {
		t.Fatalf("client error altered circuit: %#v", status)
	}
}

func TestStreamCoordinatorExhaustionReturnsLastSafeError(t *testing.T) {
	executor := &fakeStreamExecutor{fallback: fakeStreamAttempt{
		statusCode: 503,
		headers:    http.Header{"Set-Cookie": []string{"upstream-cookie-secret"}},
	}}
	coordinator, _ := streamCoordinatorForTest(t, executor, 3)
	sink := &recordingSink{}
	request := streamExecutionRequest()
	request.Headers.Set("Authorization", "Bearer downstream-token-secret")

	result, err := coordinator.ExecuteStream(context.Background(), request, sink)
	var execErr *ExecutionError
	if !errors.As(err, &execErr) {
		t.Fatalf("ExecuteStream() error = %T %v, want *ExecutionError", err, err)
	}
	if execErr.StatusCode != 503 || execErr.Category != FailureTransient || execErr.RouteID != "route-3" {
		t.Fatalf("last safe error = %#v", execErr)
	}
	if len(result.Attempts) != 3 || executor.callCount() != 3 {
		t.Fatalf("attempts = %#v, calls = %d; want each route once", result.Attempts, executor.callCount())
	}
	seen := map[string]struct{}{}
	for _, attempt := range result.Attempts {
		if _, duplicate := seen[attempt.RouteID]; duplicate {
			t.Fatalf("route %q attempted twice: %#v", attempt.RouteID, result.Attempts)
		}
		seen[attempt.RouteID] = struct{}{}
	}
	if sink.commitCount() != 0 || sink.failureCount() != 0 {
		t.Fatalf("uncommitted exhaustion touched the sink: commits = %d, failures = %d", sink.commitCount(), sink.failureCount())
	}
	rendered := err.Error() + fmt.Sprintf("%+v", result)
	for _, secret := range []string{"upstream-cookie-secret", "downstream-token-secret"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("diagnostics leaked %q: %s", secret, rendered)
		}
	}
}

func TestStreamCoordinatorMalformedStreamRetriesAtMostOnce(t *testing.T) {
	malformed := fakeStreamAttempt{
		statusCode: 200,
		chunks:     []StreamChunk{{Data: []byte("data: {broken\n\n")}},
	}
	executor := &fakeStreamExecutor{
		script:   []fakeStreamAttempt{malformed, malformed},
		fallback: okStreamAttempt(),
	}
	coordinator, _ := streamCoordinatorForTest(t, executor, 3)
	sink := &recordingSink{}

	result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
	var execErr *ExecutionError
	if !errors.As(err, &execErr) || execErr.Category != FailureProtocol {
		t.Fatalf("ExecuteStream() error = %v, want protocol execution error", err)
	}
	if executor.callCount() != 2 || len(result.Attempts) != 2 {
		t.Fatalf("calls = %d, attempts = %#v; want exactly one protocol failover", executor.callCount(), result.Attempts)
	}
	if sink.commitCount() != 0 {
		t.Fatalf("malformed stream committed downstream: %d", sink.commitCount())
	}
}

func TestStreamCoordinatorCancellation(t *testing.T) {
	t.Run("cancellation before commitment", func(t *testing.T) {
		executor := &fakeStreamExecutor{fallback: fakeStreamAttempt{statusCode: 200, block: true}}
		coordinator, selector := streamCoordinatorForTest(t, executor, 2)
		sink := &recordingSink{}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		result, err := coordinator.ExecuteStream(ctx, streamExecutionRequest(), sink)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ExecuteStream() error = %v, want context.Canceled", err)
		}
		if executor.callCount() != 1 || result.Success {
			t.Fatalf("cancellation selected another route: calls = %d, result = %#v", executor.callCount(), result)
		}
		if sink.commitCount() != 0 {
			t.Fatalf("cancelled stream committed downstream: %d", sink.commitCount())
		}
		if status := selector.CircuitStatus("route-1"); status.State != CircuitClosed || status.FailureCount != 0 {
			t.Fatalf("cancellation altered the closed circuit: %#v", status)
		}
		waitForStreamRelease(t, executor, 1)
	})

	t.Run("cancellation after commitment", func(t *testing.T) {
		executor := &fakeStreamExecutor{fallback: fakeStreamAttempt{
			statusCode: 200,
			chunks:     []StreamChunk{responsesDelta("committed")},
			block:      true,
		}}
		coordinator, _ := streamCoordinatorForTest(t, executor, 2)
		sink := &recordingSink{}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		result, err := coordinator.ExecuteStream(ctx, streamExecutionRequest(), sink)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ExecuteStream() error = %v, want context.Canceled", err)
		}
		if executor.callCount() != 1 {
			t.Fatalf("post-commit cancellation selected another route: calls = %d", executor.callCount())
		}
		if !result.Committed || result.StreamState != StreamStateFailedPartial {
			t.Fatalf("result = %#v, want committed partial failure", result)
		}
		waitForStreamRelease(t, executor, 1)
	})

	t.Run("cancellation before the first attempt abandons the selection", func(t *testing.T) {
		executor := &fakeStreamExecutor{fallback: okStreamAttempt()}
		coordinator, _ := streamCoordinatorForTest(t, executor, 1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		sink := &recordingSink{}

		result, err := coordinator.ExecuteStream(ctx, streamExecutionRequest(), sink)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ExecuteStream() error = %v, want context.Canceled", err)
		}
		if executor.callCount() != 0 || len(result.Attempts) != 0 || result.FailureCategory != FailureCancelled {
			t.Fatalf("cancelled request reached the executor: calls = %d, result = %#v", executor.callCount(), result)
		}
	})
}

func TestStreamCoordinatorReleasesFailedOverUpstream(t *testing.T) {
	first := fakeStreamAttempt{
		statusCode: 200,
		chunks:     []StreamChunk{{Data: []byte("data: {broken\n\n")}},
		block:      true,
	}
	executor := &fakeStreamExecutor{script: []fakeStreamAttempt{first}, fallback: okStreamAttempt()}
	coordinator, _ := streamCoordinatorForTest(t, executor, 2)
	sink := &recordingSink{}

	result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
	if err != nil || !result.Success {
		t.Fatalf("ExecuteStream() = %#v, %v; want failover success", result, err)
	}
	waitForStreamRelease(t, executor, 2)
	if executor.closes.Load() < 1 {
		t.Fatalf("abandoned attempt was not closed: closes = %d", executor.closes.Load())
	}
}

func TestStreamCoordinatorSinkErrorsAreTerminal(t *testing.T) {
	t.Run("commit error", func(t *testing.T) {
		executor := &fakeStreamExecutor{fallback: okStreamAttempt()}
		coordinator, _ := streamCoordinatorForTest(t, executor, 3)
		sink := &recordingSink{commitErr: errors.New("downstream gone")}

		result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
		if err == nil || !strings.Contains(err.Error(), "downstream gone") {
			t.Fatalf("ExecuteStream() error = %v, want the sink error", err)
		}
		if executor.callCount() != 1 || result.Success {
			t.Fatalf("sink error caused a failover: calls = %d, result = %#v", executor.callCount(), result)
		}
	})

	t.Run("send error", func(t *testing.T) {
		executor := &fakeStreamExecutor{fallback: okStreamAttempt()}
		coordinator, _ := streamCoordinatorForTest(t, executor, 3)
		sink := &recordingSink{sendErr: errors.New("downstream closed"), sendErrAt: 1}

		result, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
		if err == nil || !strings.Contains(err.Error(), "downstream closed") {
			t.Fatalf("ExecuteStream() error = %v, want the sink error", err)
		}
		if executor.callCount() != 1 || result.Success {
			t.Fatalf("sink error caused a failover: calls = %d, result = %#v", executor.callCount(), result)
		}
	})
}

func TestStreamCoordinatorValidatesRequestMode(t *testing.T) {
	executor := &fakeStreamExecutor{fallback: okStreamAttempt()}
	coordinator, _ := streamCoordinatorForTest(t, executor, 1)
	sink := &recordingSink{}

	nonStream := streamExecutionRequest()
	nonStream.Stream = false
	if _, err := coordinator.ExecuteStream(context.Background(), nonStream, sink); !errors.Is(err, ErrStreamModeMismatch) {
		t.Fatalf("ExecuteStream(non-stream) error = %v, want ErrStreamModeMismatch", err)
	}
	if _, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), nil); !errors.Is(err, ErrStreamSinkNotConfigured) {
		t.Fatalf("ExecuteStream(nil sink) error = %v, want ErrStreamSinkNotConfigured", err)
	}

	unknownProtocol := streamExecutionRequest()
	unknownProtocol.EntryProtocol = config.RouterProtocol("gemini")
	if _, err := coordinator.ExecuteStream(context.Background(), unknownProtocol, sink); !errors.Is(err, ErrStreamProtocolNotSupported) {
		t.Fatalf("ExecuteStream(unknown protocol) error = %v, want ErrStreamProtocolNotSupported", err)
	}

	imageEndpoint := streamExecutionRequest()
	imageEndpoint.Endpoint = config.RouterEndpointImages
	if _, err := coordinator.ExecuteStream(context.Background(), imageEndpoint, sink); !errors.Is(err, ErrEndpointNotSupported) {
		t.Fatalf("ExecuteStream(image endpoint) error = %v, want ErrEndpointNotSupported", err)
	}

	unknownModel := streamExecutionRequest()
	unknownModel.PublicModel = "missing-model"
	if _, err := coordinator.ExecuteStream(context.Background(), unknownModel, sink); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("ExecuteStream(unknown model) error = %v, want ErrModelNotFound", err)
	}

	if executor.callCount() != 0 || sink.commitCount() != 0 {
		t.Fatalf("validation failures reached the executor: calls = %d", executor.callCount())
	}
}

func TestNonStreamCoordinatorRejectsStreamingRequest(t *testing.T) {
	executor := &fakeExecutor{fallback: okAttempt()}
	coordinator, _ := coordinatorForTest(t, executor, 1)

	streaming := textExecutionRequest()
	streaming.Stream = true
	if _, _, err := coordinator.Execute(context.Background(), streaming); !errors.Is(err, ErrStreamModeMismatch) {
		t.Fatalf("Execute(stream) error = %v, want ErrStreamModeMismatch", err)
	}
	if executor.callCount() != 0 {
		t.Fatalf("streaming request reached the non-stream executor: calls = %d", executor.callCount())
	}
}

func TestStreamCoordinatorPinnedSnapshotSurvivesSwap(t *testing.T) {
	first := fakeStreamAttempt{statusCode: 503}
	executor := &fakeStreamExecutor{script: []fakeStreamAttempt{first}, fallback: okStreamAttempt()}
	coordinator, selector := streamCoordinatorForTest(t, executor, 2)

	nextConfig := coordinatorTestConfig(2)
	nextConfig.Router.ModelGroups[0].Routes[1].UpstreamModel = "swapped-model-2"
	next, err := CompileSnapshot(nextConfig, 2)
	if err != nil {
		t.Fatalf("CompileSnapshot(next) error = %v", err)
	}
	executor.hook = func(call int, _ AttemptRequest) {
		if call == 0 {
			if errSwap := selector.SwapSnapshot(next); errSwap != nil {
				t.Errorf("SwapSnapshot() error = %v", errSwap)
			}
		}
	}

	sink := &recordingSink{}
	result, errExecute := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
	if errExecute != nil || !result.Success {
		t.Fatalf("ExecuteStream() = %#v, %v; want success", result, errExecute)
	}
	if result.SnapshotRevision != 1 {
		t.Fatalf("SnapshotRevision = %d, want pinned revision 1", result.SnapshotRevision)
	}
	second := executor.call(1)
	if second.Selection.Route.UpstreamModel != "upstream-model-2" || second.Selection.SnapshotRevision != 1 {
		t.Fatalf("second attempt used the swapped snapshot: %#v", second.Selection)
	}
	if sink.commits[0].SnapshotRevision != 1 {
		t.Fatalf("commit revision = %d, want 1", sink.commits[0].SnapshotRevision)
	}
}

// probeStreamExecutor counts how many half-open probes run concurrently.
type probeStreamExecutor struct {
	inFlight atomic.Int64
	maxSeen  atomic.Int64
	probes   atomic.Int64
	release  chan struct{}
}

func (e *probeStreamExecutor) ExecuteStreamAttempt(ctx context.Context, request AttemptRequest) (*StreamAttempt, error) {
	if request.Selection.HalfOpenProbe {
		e.probes.Add(1)
		current := e.inFlight.Add(1)
		for {
			previous := e.maxSeen.Load()
			if current <= previous || e.maxSeen.CompareAndSwap(previous, current) {
				break
			}
		}
		defer e.inFlight.Add(-1)
		select {
		case <-e.release:
		case <-ctx.Done():
		}
	}
	chunks := make(chan StreamChunk, 2)
	chunks <- responsesDelta("probe")
	close(chunks)
	return &StreamAttempt{StatusCode: 200, Chunks: chunks}, nil
}

func TestStreamCoordinatorSingleHalfOpenProbe(t *testing.T) {
	executor := &probeStreamExecutor{release: make(chan struct{})}
	coordinator, selector := streamCoordinatorForTest(t, executor, 1)
	current := coordinatorTestTime
	var timeMu sync.Mutex
	selector.now = func() time.Time {
		timeMu.Lock()
		defer timeMu.Unlock()
		return current
	}

	// Open the single route's circuit so the next attempts must be probes.
	selector.circuits.Record("route-1", false, AttemptResult{Category: FailureRateLimit, RetryAfter: 30 * time.Second}, current)
	timeMu.Lock()
	current = coordinatorTestTime.Add(31 * time.Second)
	timeMu.Unlock()

	var wg sync.WaitGroup
	results := make([]error, 8)
	for worker := 0; worker < len(results); worker++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), &recordingSink{})
			results[index] = err
		}(worker)
	}
	time.Sleep(50 * time.Millisecond)
	close(executor.release)
	wg.Wait()

	if executor.maxSeen.Load() > 1 {
		t.Fatalf("concurrent half-open probes = %d, want at most 1", executor.maxSeen.Load())
	}
	if executor.probes.Load() == 0 {
		t.Fatalf("no half-open probe was admitted")
	}
	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded == 0 {
		t.Fatalf("no probe request succeeded: %v", results)
	}
}

// raceStreamExecutor produces a mix of terminal, retryable, and committed
// streaming outcomes.
type raceStreamExecutor struct {
	counter atomic.Uint64
}

func (e *raceStreamExecutor) ExecuteStreamAttempt(_ context.Context, _ AttemptRequest) (*StreamAttempt, error) {
	switch e.counter.Add(1) % 6 {
	case 0:
		return &StreamAttempt{StatusCode: 503}, nil
	case 1:
		return &StreamAttempt{StatusCode: 429, Headers: headersWith("Retry-After", "1")}, nil
	case 2:
		return nil, errors.New("connection reset")
	case 3:
		return &StreamAttempt{StatusCode: 401}, nil
	case 4:
		chunks := make(chan StreamChunk, 2)
		chunks <- StreamChunk{Data: []byte("data: {broken\n\n")}
		close(chunks)
		return &StreamAttempt{StatusCode: 200, Chunks: chunks}, nil
	default:
		chunks := make(chan StreamChunk, 3)
		chunks <- StreamChunk{Data: []byte(": keepalive\n\n")}
		chunks <- responsesDelta("ok")
		close(chunks)
		return &StreamAttempt{StatusCode: 200, Chunks: chunks}, nil
	}
}

func TestStreamCoordinatorConcurrentExecutionRace(t *testing.T) {
	executor := &raceStreamExecutor{}
	coordinator, selector := streamCoordinatorForTest(t, executor, 3)
	selector.now = time.Now

	alternate, err := CompileSnapshot(coordinatorTestConfig(3), 2)
	if err != nil {
		t.Fatalf("CompileSnapshot(alternate) error = %v", err)
	}
	base, err := CompileSnapshot(coordinatorTestConfig(3), 3)
	if err != nil {
		t.Fatalf("CompileSnapshot(base) error = %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for iteration := 0; iteration < 200; iteration++ {
			snapshot := base
			if iteration%2 == 0 {
				snapshot = alternate
			}
			if errSwap := selector.SwapSnapshot(snapshot); errSwap != nil {
				t.Errorf("SwapSnapshot() error = %v", errSwap)
			}
			selector.ResetCircuit(fmt.Sprintf("route-%d", iteration%3+1))
		}
	}()

	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < 25; iteration++ {
				sink := &recordingSink{}
				result, errExecute := coordinator.ExecuteStream(context.Background(), streamExecutionRequest(), sink)
				if errExecute == nil && (!result.Success || sink.commitCount() != 1) {
					t.Errorf("nil error with unsuccessful result: %#v, commits = %d", result, sink.commitCount())
				}
				if sink.commitCount() > 1 {
					t.Errorf("downstream committed %d times", sink.commitCount())
				}
				if !result.Committed && sink.commitCount() != 0 {
					t.Errorf("uncommitted result touched the sink: %#v", result)
				}
			}
		}()
	}
	wg.Wait()
}

func waitForStreamRelease(t *testing.T, executor *fakeStreamExecutor, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if executor.finished.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("upstream stream goroutines leaked: finished = %d, want %d", executor.finished.Load(), want)
}
