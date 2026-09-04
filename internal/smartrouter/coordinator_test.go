package smartrouter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var coordinatorTestTime = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

type fakeAttempt struct {
	response AttemptResponse
	err      error
}

type fakeExecutor struct {
	mu       sync.Mutex
	calls    []AttemptRequest
	script   []fakeAttempt
	fallback fakeAttempt
	hook     func(call int, request AttemptRequest)
}

func (f *fakeExecutor) ExecuteAttempt(_ context.Context, request AttemptRequest) (AttemptResponse, error) {
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
	return attempt.response, attempt.err
}

func (f *fakeExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeExecutor) call(index int) AttemptRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[index]
}

func okAttempt() fakeAttempt {
	return fakeAttempt{response: AttemptResponse{
		StatusCode: 200,
		Headers:    http.Header{"X-Upstream": []string{"yes"}},
		Body:       []byte("ok-body"),
	}}
}

func statusAttempt(statusCode int, headers http.Header) fakeAttempt {
	return fakeAttempt{response: AttemptResponse{StatusCode: statusCode, Headers: headers}}
}

func rejectBadBody(body []byte) error {
	if string(body) == "bad" {
		return errors.New("malformed body: bad")
	}
	return nil
}

func coordinatorTestConfig(routeCount int) *config.Config {
	upstreams := make([]config.RouterUpstream, 0, routeCount)
	routes := make([]config.RouterRoute, 0, routeCount)
	for index := 1; index <= routeCount; index++ {
		upstreams = append(upstreams, config.RouterUpstream{
			ID:       fmt.Sprintf("upstream-%d", index),
			Name:     fmt.Sprintf("Upstream %d", index),
			Protocol: config.RouterProtocolOpenAIResponses,
			BaseURL:  fmt.Sprintf("https://upstream-%d.example/v1", index),
			Capabilities: config.RouterCapabilities{
				Endpoints: []string{config.RouterEndpointResponses},
			},
		})
		routes = append(routes, config.RouterRoute{
			ID:            fmt.Sprintf("route-%d", index),
			UpstreamID:    fmt.Sprintf("upstream-%d", index),
			UpstreamModel: fmt.Sprintf("upstream-model-%d", index),
			Priority:      (routeCount - index + 1) * 100,
			Weight:        1,
		})
	}
	return &config.Config{
		ServiceRole: config.ServiceRoleRouter,
		Router: config.RouterConfig{
			Upstreams: upstreams,
			ModelGroups: []config.RouterModelGroup{
				{
					ID:          "group-x",
					PublicModel: "model-x",
					Capability:  config.RouterCapabilityText,
					Selection: config.RouterSelection{
						Strategy: config.RouterSelectionWeightedRoundRobin,
					},
					Routes: routes,
				},
			},
		},
	}
}

func coordinatorForTest(t *testing.T, executor AttemptExecutor, routeCount int) (*Coordinator, *Selector) {
	t.Helper()
	snapshot, err := CompileSnapshot(coordinatorTestConfig(routeCount), 1)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	selector := NewSelector(NewSnapshotStore(snapshot), NewCircuitStore(DefaultCircuitPolicy()))
	selector.now = func() time.Time { return coordinatorTestTime }
	return NewCoordinator(selector, executor), selector
}

func imageCoordinatorForTest(t *testing.T, executor AttemptExecutor, routeCount int) (*Coordinator, *Selector) {
	t.Helper()
	cfg := coordinatorTestConfig(routeCount)
	cfg.Router.ModelGroups[0].Capability = config.RouterCapabilityImage
	for index := range cfg.Router.Upstreams {
		cfg.Router.Upstreams[index].Capabilities = config.RouterCapabilities{
			Endpoints:       []string{config.RouterEndpointResponses, config.RouterEndpointImages},
			ImageGeneration: true,
		}
	}
	snapshot, err := CompileSnapshot(cfg, 1)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	selector := NewSelector(NewSnapshotStore(snapshot), NewCircuitStore(DefaultCircuitPolicy()))
	selector.now = func() time.Time { return coordinatorTestTime }
	return NewCoordinator(selector, executor), selector
}

func textExecutionRequest() ExecutionRequest {
	return ExecutionRequest{
		RequestID:     "req-1",
		PublicModel:   "model-x",
		AffinityKey:   "affinity-1",
		EntryProtocol: config.RouterProtocolOpenAIResponses,
		Endpoint:      config.RouterEndpointResponses,
		Body:          []byte(`{"model":"model-x"}`),
		Headers:       http.Header{"X-Test": []string{"yes"}},
		Query:         url.Values{"beta": []string{"true"}},
		BodyValidator: rejectBadBody,
	}
}

func imageExecutionRequest() ExecutionRequest {
	return ExecutionRequest{
		RequestID:     "image-req-1",
		PublicModel:   "model-x",
		AffinityKey:   "image-affinity-1",
		EntryProtocol: config.RouterProtocolOpenAIResponses,
		Endpoint:      config.RouterEndpointImages,
		Body:          []byte(`{"model":"model-x","prompt":"draw","n":1}`),
		BodyValidator: rejectBadBody,
	}
}

func TestCoordinatorReportsUsageOnlyFromSuccessfulAttempt(t *testing.T) {
	executor := &fakeExecutor{script: []fakeAttempt{
		{response: AttemptResponse{
			StatusCode: http.StatusServiceUnavailable,
			Body:       []byte(`{"usage":{"input_tokens":999,"output_tokens":999}}`),
		}},
		{response: AttemptResponse{
			StatusCode: http.StatusOK,
			Body:       []byte(`{"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}}`),
		}},
	}}
	coordinator, _ := coordinatorForTest(t, executor, 2)

	_, result, err := coordinator.Execute(context.Background(), textExecutionRequest())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.UsageMissing || result.Usage == nil {
		t.Fatalf("usage = %#v, missing = %t", result.Usage, result.UsageMissing)
	}
	assertUsageToken(t, "input", result.Usage.InputTokens, usageToken(11))
	assertUsageToken(t, "output", result.Usage.OutputTokens, usageToken(4))
	assertUsageToken(t, "total", result.Usage.TotalTokens, usageToken(15))
}

func TestCoordinatorMarksSuccessfulResponseWithoutUsage(t *testing.T) {
	executor := &fakeExecutor{fallback: fakeAttempt{response: AttemptResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"id":"response_without_usage"}`),
	}}}
	coordinator, _ := coordinatorForTest(t, executor, 1)

	_, result, err := coordinator.Execute(context.Background(), textExecutionRequest())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.UsageMissing || result.Usage != nil {
		t.Fatalf("usage = %#v, missing = %t", result.Usage, result.UsageMissing)
	}
}

func TestCoordinatorRetryMatrix(t *testing.T) {
	type matrixCase struct {
		name         string
		first        fakeAttempt
		wantSuccess  bool
		wantFailover bool
		wantCategory FailureCategory
		wantStatus   int
		checkCircuit func(t *testing.T, status CircuitStatus)
	}

	closedCircuit := func(t *testing.T, status CircuitStatus) {
		t.Helper()
		if status.State != CircuitClosed || status.FailureCount != 0 {
			t.Fatalf("route-1 circuit = %#v, want untouched closed circuit", status)
		}
	}
	transientCircuit := func(t *testing.T, status CircuitStatus) {
		t.Helper()
		if status.State != CircuitClosed || status.FailureCount != 1 {
			t.Fatalf("route-1 circuit = %#v, want closed with one recorded failure", status)
		}
	}
	authCircuit := func(t *testing.T, status CircuitStatus) {
		t.Helper()
		if status.State != CircuitOpen || !status.RequiresReset {
			t.Fatalf("route-1 circuit = %#v, want open awaiting reset", status)
		}
	}
	rateCircuit := func(t *testing.T, status CircuitStatus) {
		t.Helper()
		want := coordinatorTestTime.Add(90 * time.Second)
		if status.State != CircuitOpen || !status.OpenUntil.Equal(want) {
			t.Fatalf("route-1 circuit = %#v, want open until %s", status, want)
		}
	}

	terminal := func(statusCode int) matrixCase {
		return matrixCase{
			name:         fmt.Sprintf("status %d terminal", statusCode),
			first:        statusAttempt(statusCode, nil),
			wantCategory: FailureClient,
			wantStatus:   statusCode,
			checkCircuit: closedCircuit,
		}
	}
	transient := func(statusCode int) matrixCase {
		return matrixCase{
			name:         fmt.Sprintf("status %d retries", statusCode),
			first:        statusAttempt(statusCode, nil),
			wantFailover: true,
			wantCategory: FailureTransient,
			wantStatus:   statusCode,
			checkCircuit: transientCircuit,
		}
	}

	cases := []matrixCase{
		{
			name:         "status 200 valid body succeeds",
			first:        okAttempt(),
			wantSuccess:  true,
			wantCategory: FailureNone,
			wantStatus:   200,
		},
		{
			name: "status 200 malformed body retries",
			first: fakeAttempt{response: AttemptResponse{
				StatusCode: 200,
				Body:       []byte("bad"),
			}},
			wantFailover: true,
			wantCategory: FailureProtocol,
			wantStatus:   200,
			checkCircuit: transientCircuit,
		},
		terminal(400),
		terminal(404),
		terminal(409),
		terminal(422),
		terminal(402),
		terminal(501),
		terminal(302),
		{
			name:         "status 401 retries",
			first:        statusAttempt(401, nil),
			wantFailover: true,
			wantCategory: FailureAuth,
			wantStatus:   401,
			checkCircuit: authCircuit,
		},
		{
			name:         "status 403 retries",
			first:        statusAttempt(403, nil),
			wantFailover: true,
			wantCategory: FailureAuth,
			wantStatus:   403,
			checkCircuit: authCircuit,
		},
		{
			name:         "status 429 retries with Retry-After",
			first:        statusAttempt(429, http.Header{"Retry-After": []string{"90"}}),
			wantFailover: true,
			wantCategory: FailureRateLimit,
			wantStatus:   429,
			checkCircuit: rateCircuit,
		},
		transient(408),
		transient(500),
		transient(502),
		transient(503),
		transient(504),
		{
			name:         "transport error before headers retries",
			first:        fakeAttempt{err: errors.New("connection refused")},
			wantFailover: true,
			wantCategory: FailureTransient,
			wantStatus:   0,
			checkCircuit: transientCircuit,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			executor := &fakeExecutor{script: []fakeAttempt{test.first}, fallback: okAttempt()}
			coordinator, selector := coordinatorForTest(t, executor, 2)
			response, result, err := coordinator.Execute(context.Background(), textExecutionRequest())

			if test.wantSuccess {
				if err != nil {
					t.Fatalf("Execute() error = %v, want success", err)
				}
				if response == nil || !result.Success || len(result.Attempts) != 1 || executor.callCount() != 1 {
					t.Fatalf("success result = %#v, attempts = %d", result, executor.callCount())
				}
				return
			}
			if len(result.Attempts) == 0 {
				t.Fatalf("no attempts recorded: %#v", result)
			}
			first := result.Attempts[0]
			if first.RouteID != "route-1" || first.FailureCategory != test.wantCategory || first.StatusCode != test.wantStatus {
				t.Fatalf("first attempt = %#v, want route-1 category %q status %d", first, test.wantCategory, test.wantStatus)
			}
			if test.checkCircuit != nil {
				test.checkCircuit(t, selector.CircuitStatus("route-1"))
			}

			if test.wantFailover {
				if err != nil {
					t.Fatalf("Execute() error = %v, want failover success", err)
				}
				if response == nil || !result.Success || len(result.Attempts) != 2 || executor.callCount() != 2 {
					t.Fatalf("failover result = %#v, calls = %d", result, executor.callCount())
				}
				if result.Attempts[1].RouteID != "route-2" || result.RouteID != "route-2" {
					t.Fatalf("failover attempts = %#v", result.Attempts)
				}
				return
			}

			if response != nil || result.Success {
				t.Fatalf("terminal result unexpectedly succeeded: %#v", result)
			}
			if len(result.Attempts) != 1 || executor.callCount() != 1 {
				t.Fatalf("terminal case attempted failover: attempts = %#v, calls = %d", result.Attempts, executor.callCount())
			}
			var execErr *ExecutionError
			if !errors.As(err, &execErr) {
				t.Fatalf("Execute() error = %T %v, want *ExecutionError", err, err)
			}
			if execErr.StatusCode != test.wantStatus || execErr.Category != test.wantCategory || execErr.RouteID != "route-1" {
				t.Fatalf("execution error = %#v", execErr)
			}
		})
	}
}

func TestCoordinatorImageRetryMatrixAvoidsAmbiguousDuplicates(t *testing.T) {
	tests := []struct {
		name          string
		first         fakeAttempt
		wantFailover  bool
		wantCategory  FailureCategory
		wantStatus    int
		wantSucceeded bool
	}{
		{
			name:          "valid image succeeds",
			first:         okAttempt(),
			wantSucceeded: true,
			wantStatus:    http.StatusOK,
		},
		{
			name:         "401 safely fails over",
			first:        statusAttempt(http.StatusUnauthorized, nil),
			wantFailover: true,
			wantCategory: FailureAuth,
			wantStatus:   http.StatusUnauthorized,
		},
		{
			name:         "403 safely fails over",
			first:        statusAttempt(http.StatusForbidden, nil),
			wantFailover: true,
			wantCategory: FailureAuth,
			wantStatus:   http.StatusForbidden,
		},
		{
			name:         "429 safely fails over",
			first:        statusAttempt(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"60"}}),
			wantFailover: true,
			wantCategory: FailureRateLimit,
			wantStatus:   http.StatusTooManyRequests,
		},
		{
			name:         "transport failure is ambiguous",
			first:        fakeAttempt{err: errors.New("connection reset")},
			wantCategory: FailureImageAmbiguous,
		},
		{
			name:         "timeout status is ambiguous",
			first:        statusAttempt(http.StatusRequestTimeout, nil),
			wantCategory: FailureImageAmbiguous,
			wantStatus:   http.StatusRequestTimeout,
		},
		{
			name:         "500 is ambiguous",
			first:        statusAttempt(http.StatusInternalServerError, nil),
			wantCategory: FailureImageAmbiguous,
			wantStatus:   http.StatusInternalServerError,
		},
		{
			name:         "503 is ambiguous",
			first:        statusAttempt(http.StatusServiceUnavailable, nil),
			wantCategory: FailureImageAmbiguous,
			wantStatus:   http.StatusServiceUnavailable,
		},
		{
			name:         "malformed success is ambiguous",
			first:        fakeAttempt{response: AttemptResponse{StatusCode: http.StatusOK, Body: []byte("bad")}},
			wantCategory: FailureImageAmbiguous,
			wantStatus:   http.StatusOK,
		},
		{
			name:         "validation status is terminal",
			first:        statusAttempt(http.StatusUnprocessableEntity, nil),
			wantCategory: FailureClient,
			wantStatus:   http.StatusUnprocessableEntity,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &fakeExecutor{script: []fakeAttempt{test.first}, fallback: okAttempt()}
			coordinator, _ := imageCoordinatorForTest(t, executor, 2)
			response, result, err := coordinator.Execute(context.Background(), imageExecutionRequest())

			if test.wantSucceeded {
				if err != nil || response == nil || !result.Success || executor.callCount() != 1 {
					t.Fatalf("Execute() = %#v, %v; calls = %d", result, err, executor.callCount())
				}
				return
			}
			if test.wantFailover {
				if err != nil || response == nil || !result.Success || executor.callCount() != 2 {
					t.Fatalf("Execute() = %#v, %v; calls = %d, want safe failover", result, err, executor.callCount())
				}
				if result.Attempts[0].FailureCategory != test.wantCategory || result.Attempts[0].StatusCode != test.wantStatus {
					t.Fatalf("first attempt = %#v", result.Attempts[0])
				}
				return
			}

			if response != nil || result.Success || executor.callCount() != 1 {
				t.Fatalf("ambiguous/terminal execution = %#v, response = %#v, calls = %d", result, response, executor.callCount())
			}
			var execErr *ExecutionError
			if !errors.As(err, &execErr) {
				t.Fatalf("Execute() error = %T %v, want *ExecutionError", err, err)
			}
			if execErr.Category != test.wantCategory || execErr.StatusCode != test.wantStatus {
				t.Fatalf("execution error = %#v", execErr)
			}
		})
	}
}

func TestExecutionPolicyRejectsStreamingImages(t *testing.T) {
	if _, err := ExecutionPolicyFor(config.RouterCapabilityImage, true); !errors.Is(err, ErrExecutionPolicyUnavailable) {
		t.Fatalf("ExecutionPolicyFor(image, stream) error = %v, want ErrExecutionPolicyUnavailable", err)
	}
}

func TestCoordinatorPrimarySuccessAttemptDetails(t *testing.T) {
	executor := &fakeExecutor{fallback: okAttempt()}
	coordinator, _ := coordinatorForTest(t, executor, 2)
	var validatedBody string
	request := textExecutionRequest()
	request.BodyValidator = func(body []byte) error {
		validatedBody = string(body)
		return nil
	}

	response, result, err := coordinator.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if executor.callCount() != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.callCount())
	}

	attempt := executor.call(0)
	if attempt.Selection.Route.ID != "route-1" ||
		attempt.Selection.Route.UpstreamModel != "upstream-model-1" ||
		attempt.Selection.Upstream.ID != "upstream-1" {
		t.Fatalf("attempt selection = %#v", attempt.Selection)
	}
	if attempt.EntryProtocol != config.RouterProtocolOpenAIResponses ||
		attempt.Endpoint != config.RouterEndpointResponses ||
		string(attempt.Body) != `{"model":"model-x"}` ||
		attempt.Headers.Get("X-Test") != "yes" ||
		attempt.Query.Get("beta") != "true" {
		t.Fatalf("attempt request = %#v", attempt)
	}

	if validatedBody != "ok-body" {
		t.Fatalf("validator saw %q, want ok-body", validatedBody)
	}
	if response.StatusCode != 200 || string(response.Body) != "ok-body" || response.Headers.Get("X-Upstream") != "yes" {
		t.Fatalf("response = %#v", response)
	}
	if result.RequestID != "req-1" || result.SnapshotRevision != 1 ||
		result.RouteID != "route-1" || result.UpstreamID != "upstream-1" ||
		result.StatusCode != 200 || !result.Success || result.FailureCategory != FailureNone {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].HalfOpenProbe {
		t.Fatalf("attempt records = %#v", result.Attempts)
	}
}

func TestCoordinatorMalformedSuccessRetriesAtMostOnce(t *testing.T) {
	badAttempt := fakeAttempt{response: AttemptResponse{StatusCode: 200, Body: []byte("bad")}}

	t.Run("second malformed body is terminal", func(t *testing.T) {
		executor := &fakeExecutor{script: []fakeAttempt{badAttempt, badAttempt}, fallback: okAttempt()}
		coordinator, _ := coordinatorForTest(t, executor, 3)
		response, result, err := coordinator.Execute(context.Background(), textExecutionRequest())
		if response != nil || result.Success {
			t.Fatalf("malformed result unexpectedly succeeded: %#v", result)
		}
		if executor.callCount() != 2 || len(result.Attempts) != 2 {
			t.Fatalf("calls = %d, attempts = %#v; want exactly one protocol failover", executor.callCount(), result.Attempts)
		}
		var execErr *ExecutionError
		if !errors.As(err, &execErr) || execErr.Category != FailureProtocol || execErr.StatusCode != 200 {
			t.Fatalf("Execute() error = %v, want protocol execution error", err)
		}
	})

	t.Run("one protocol failover may succeed", func(t *testing.T) {
		executor := &fakeExecutor{script: []fakeAttempt{badAttempt}, fallback: okAttempt()}
		coordinator, _ := coordinatorForTest(t, executor, 3)
		response, result, err := coordinator.Execute(context.Background(), textExecutionRequest())
		if err != nil || response == nil || !result.Success {
			t.Fatalf("Execute() = %#v, %v; want success after one failover", result, err)
		}
		if len(result.Attempts) != 2 || result.Attempts[0].FailureCategory != FailureProtocol {
			t.Fatalf("attempts = %#v", result.Attempts)
		}
	})
}

func TestCoordinatorCancellationDuringAttempt(t *testing.T) {
	executor := &fakeExecutor{script: []fakeAttempt{statusAttempt(503, nil)}, fallback: okAttempt()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executor.hook = func(call int, _ AttemptRequest) {
		if call == 0 {
			cancel()
		}
	}
	coordinator, selector := coordinatorForTest(t, executor, 2)

	response, result, err := coordinator.Execute(ctx, textExecutionRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context.Canceled", err)
	}
	if response != nil || result.Success || executor.callCount() != 1 {
		t.Fatalf("cancelled execution = %#v, calls = %d", result, executor.callCount())
	}
	if len(result.Attempts) != 1 || result.Attempts[0].FailureCategory != FailureCancelled || result.FailureCategory != FailureCancelled {
		t.Fatalf("cancelled attempts = %#v", result.Attempts)
	}
	if status := selector.CircuitStatus("route-1"); status.State != CircuitClosed || status.FailureCount != 0 {
		t.Fatalf("cancellation altered closed circuit: %#v", status)
	}
}

func TestCoordinatorImageCancellationDuringAttemptDoesNotRetry(t *testing.T) {
	executor := &fakeExecutor{script: []fakeAttempt{statusAttempt(http.StatusTooManyRequests, nil)}, fallback: okAttempt()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executor.hook = func(call int, _ AttemptRequest) {
		if call == 0 {
			cancel()
		}
	}
	coordinator, selector := imageCoordinatorForTest(t, executor, 2)

	response, result, err := coordinator.Execute(ctx, imageExecutionRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context.Canceled", err)
	}
	if response != nil || result.Success || executor.callCount() != 1 {
		t.Fatalf("cancelled image execution = %#v, calls = %d", result, executor.callCount())
	}
	if len(result.Attempts) != 1 || result.Attempts[0].FailureCategory != FailureCancelled {
		t.Fatalf("cancelled image attempts = %#v", result.Attempts)
	}
	if status := selector.CircuitStatus("route-1"); status.State != CircuitClosed || status.FailureCount != 0 {
		t.Fatalf("image cancellation altered closed circuit: %#v", status)
	}
}

func TestCoordinatorCancellationBeforeAttemptAbandonsSelection(t *testing.T) {
	executor := &fakeExecutor{
		script:   []fakeAttempt{statusAttempt(429, http.Header{"Retry-After": []string{"30"}})},
		fallback: okAttempt(),
	}
	coordinator, selector := coordinatorForTest(t, executor, 1)
	current := coordinatorTestTime
	selector.now = func() time.Time { return current }

	_, _, err := coordinator.Execute(context.Background(), textExecutionRequest())
	var execErr *ExecutionError
	if !errors.As(err, &execErr) || execErr.Category != FailureRateLimit {
		t.Fatalf("Execute(first) error = %v, want rate-limit execution error", err)
	}

	current = coordinatorTestTime.Add(31 * time.Second)
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	response, result, errCancelled := coordinator.Execute(cancelledCtx, textExecutionRequest())
	if !errors.Is(errCancelled, context.Canceled) {
		t.Fatalf("Execute(cancelled) error = %v, want context.Canceled", errCancelled)
	}
	if response != nil || len(result.Attempts) != 0 || result.FailureCategory != FailureCancelled {
		t.Fatalf("cancelled result = %#v", result)
	}
	if executor.callCount() != 1 {
		t.Fatalf("executor was called for an abandoned selection: calls = %d", executor.callCount())
	}
	status := selector.CircuitStatus("route-1")
	if status.State != CircuitOpen || status.ProbeInFlight {
		t.Fatalf("abandoned probe was not released: %#v", status)
	}

	current = coordinatorTestTime.Add(63 * time.Second)
	response, result, errRetry := coordinator.Execute(context.Background(), textExecutionRequest())
	if errRetry != nil || response == nil || !result.Success {
		t.Fatalf("Execute(after abandon) = %#v, %v; want probe success", result, errRetry)
	}
	if len(result.Attempts) != 1 || !result.Attempts[0].HalfOpenProbe {
		t.Fatalf("probe attempts = %#v, want one half-open probe", result.Attempts)
	}
	if statusAfter := selector.CircuitStatus("route-1"); statusAfter.State != CircuitClosed {
		t.Fatalf("circuit after probe success = %#v", statusAfter)
	}
}

func TestCoordinatorExhaustionReturnsLastSafeError(t *testing.T) {
	executor := &fakeExecutor{fallback: fakeAttempt{response: AttemptResponse{
		StatusCode: 503,
		Headers:    http.Header{"Set-Cookie": []string{"upstream-cookie-secret"}},
		Body:       []byte("upstream-body-secret"),
	}}}
	coordinator, _ := coordinatorForTest(t, executor, 3)
	request := textExecutionRequest()
	request.Headers.Set("Authorization", "Bearer downstream-token-secret")
	request.Headers.Set("Cookie", "session=downstream-cookie-secret")

	response, result, err := coordinator.Execute(context.Background(), request)
	if response != nil || result.Success {
		t.Fatalf("exhausted execution unexpectedly succeeded: %#v", result)
	}
	if len(result.Attempts) != 3 || executor.callCount() != 3 {
		t.Fatalf("attempts = %#v, calls = %d; want all three routes once", result.Attempts, executor.callCount())
	}
	seen := map[string]struct{}{}
	for _, attempt := range result.Attempts {
		if _, duplicate := seen[attempt.RouteID]; duplicate {
			t.Fatalf("route %q attempted twice: %#v", attempt.RouteID, result.Attempts)
		}
		seen[attempt.RouteID] = struct{}{}
	}
	var execErr *ExecutionError
	if !errors.As(err, &execErr) {
		t.Fatalf("Execute() error = %T %v, want *ExecutionError", err, err)
	}
	if execErr.StatusCode != 503 || execErr.Category != FailureTransient || execErr.RouteID != "route-3" {
		t.Fatalf("last safe error = %#v", execErr)
	}

	rendered := err.Error() + fmt.Sprintf("%+v", result)
	for _, secret := range []string{
		"upstream-cookie-secret",
		"upstream-body-secret",
		"downstream-token-secret",
		"downstream-cookie-secret",
	} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("diagnostics leaked %q: %s", secret, rendered)
		}
	}
}

func TestCoordinatorValidatesEndpointAndMode(t *testing.T) {
	executor := &fakeExecutor{fallback: okAttempt()}
	coordinator, _ := coordinatorForTest(t, executor, 1)

	unknownModel := textExecutionRequest()
	unknownModel.PublicModel = "missing-model"
	if _, _, err := coordinator.Execute(context.Background(), unknownModel); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("Execute(unknown model) error = %v, want ErrModelNotFound", err)
	}

	imageEndpoint := textExecutionRequest()
	imageEndpoint.Endpoint = config.RouterEndpointImages
	if _, _, err := coordinator.Execute(context.Background(), imageEndpoint); !errors.Is(err, ErrEndpointNotSupported) {
		t.Fatalf("Execute(image endpoint) error = %v, want ErrEndpointNotSupported", err)
	}

	emptyEndpoint := textExecutionRequest()
	emptyEndpoint.Endpoint = ""
	if _, _, err := coordinator.Execute(context.Background(), emptyEndpoint); !errors.Is(err, ErrEndpointNotSupported) {
		t.Fatalf("Execute(empty endpoint) error = %v, want ErrEndpointNotSupported", err)
	}

	// Streaming is served by StreamCoordinator, so this path rejects it
	// instead of reporting a missing policy.
	streaming := textExecutionRequest()
	streaming.Stream = true
	if _, _, err := coordinator.Execute(context.Background(), streaming); !errors.Is(err, ErrStreamModeMismatch) {
		t.Fatalf("Execute(stream) error = %v, want ErrStreamModeMismatch", err)
	}

	if executor.callCount() != 0 {
		t.Fatalf("validation failures reached the executor: calls = %d", executor.callCount())
	}

	normalized := textExecutionRequest()
	normalized.Endpoint = " RESPONSES "
	if _, result, err := coordinator.Execute(context.Background(), normalized); err != nil || !result.Success {
		t.Fatalf("Execute(normalized endpoint) = %#v, %v; want success", result, err)
	}
	if executor.call(0).Endpoint != config.RouterEndpointResponses {
		t.Fatalf("normalized endpoint = %q", executor.call(0).Endpoint)
	}
}

func TestCoordinatorPinnedSnapshotSurvivesSwap(t *testing.T) {
	executor := &fakeExecutor{script: []fakeAttempt{statusAttempt(503, nil)}, fallback: okAttempt()}
	coordinator, selector := coordinatorForTest(t, executor, 2)

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

	response, result, errExecute := coordinator.Execute(context.Background(), textExecutionRequest())
	if errExecute != nil || response == nil || !result.Success {
		t.Fatalf("Execute() = %#v, %v; want success", result, errExecute)
	}
	if result.SnapshotRevision != 1 {
		t.Fatalf("SnapshotRevision = %d, want pinned revision 1", result.SnapshotRevision)
	}
	second := executor.call(1)
	if second.Selection.Route.UpstreamModel != "upstream-model-2" || second.Selection.SnapshotRevision != 1 {
		t.Fatalf("second attempt used swapped snapshot: %#v", second.Selection)
	}
}

func TestCoordinatorDefensiveCopies(t *testing.T) {
	shared := []byte("ok-body")
	executor := &fakeExecutor{script: []fakeAttempt{
		statusAttempt(503, nil),
		{response: AttemptResponse{StatusCode: 200, Headers: http.Header{"X-Upstream": []string{"yes"}}, Body: shared}},
	}}
	executor.hook = func(call int, request AttemptRequest) {
		if call == 0 {
			if len(request.Body) > 0 {
				request.Body[0] = 'X'
			}
			request.Headers.Set("X-Test", "mutated")
			request.Query.Set("beta", "mutated")
		}
	}
	coordinator, _ := coordinatorForTest(t, executor, 2)
	request := textExecutionRequest()

	response, result, err := coordinator.Execute(context.Background(), request)
	if err != nil || response == nil || !result.Success {
		t.Fatalf("Execute() = %#v, %v; want success", result, err)
	}

	second := executor.call(1)
	if string(second.Body) != `{"model":"model-x"}` ||
		second.Headers.Get("X-Test") != "yes" ||
		second.Query.Get("beta") != "true" {
		t.Fatalf("second attempt saw mutated request: %#v", second)
	}
	if string(request.Body) != `{"model":"model-x"}` ||
		request.Headers.Get("X-Test") != "yes" ||
		request.Query.Get("beta") != "true" {
		t.Fatalf("caller request was mutated: %#v", request)
	}

	shared[0] = 'Z'
	if string(response.Body) != "ok-body" {
		t.Fatalf("response body shares executor memory: %q", response.Body)
	}
}

type raceExecutor struct {
	counter atomic.Uint64
}

func (e *raceExecutor) ExecuteAttempt(_ context.Context, _ AttemptRequest) (AttemptResponse, error) {
	switch e.counter.Add(1) % 5 {
	case 0:
		return AttemptResponse{StatusCode: 503}, nil
	case 1:
		return AttemptResponse{StatusCode: 429, Headers: http.Header{"Retry-After": []string{"1"}}}, nil
	case 2:
		return AttemptResponse{}, errors.New("connection reset")
	case 3:
		return AttemptResponse{StatusCode: 401}, nil
	default:
		return AttemptResponse{StatusCode: 200, Body: []byte("ok")}, nil
	}
}

func TestCoordinatorConcurrentExecutionRace(t *testing.T) {
	executor := &raceExecutor{}
	coordinator, selector := coordinatorForTest(t, executor, 3)
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
				response, result, errExecute := coordinator.Execute(context.Background(), textExecutionRequest())
				if errExecute == nil && (response == nil || !result.Success) {
					t.Errorf("nil error with unsuccessful result: %#v", result)
				}
				if errExecute != nil && response != nil {
					t.Errorf("error %v with non-nil response", errExecute)
				}
			}
		}()
	}
	wg.Wait()
}
