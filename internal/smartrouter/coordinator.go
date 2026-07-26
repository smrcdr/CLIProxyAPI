package smartrouter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var (
	ErrEndpointNotSupported       = errors.New("router endpoint is not supported")
	ErrExecutionPolicyUnavailable = errors.New("router execution policy is not available")
	ErrExecutorNotConfigured      = errors.New("router attempt executor is not configured")
)

// AttemptExecutor performs exactly one upstream attempt for one selected
// route. Implementations own protocol translation, credentials, and network
// transport; the coordinator never sees any of them. A nil error means a
// response with headers was received, regardless of its status code.
type AttemptExecutor interface {
	ExecuteAttempt(context.Context, AttemptRequest) (AttemptResponse, error)
}

// AttemptRequest describes one attempt against one selected route. The
// coordinator hands the executor defensive copies, so an implementation may
// mutate the request freely.
type AttemptRequest struct {
	Selection     Selection
	EntryProtocol config.RouterProtocol
	Endpoint      string
	Body          []byte
	Headers       http.Header
	Query         url.Values
}

// AttemptResponse is the upstream response observed by an executor.
type AttemptResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// ExecutionPolicy captures the retry matrix for one capability and stream
// mode. Phase 3 implements only the text non-stream policy; streaming and
// image policies are added by later phases.
type ExecutionPolicy struct {
	Capability           config.RouterCapability
	Stream               bool
	MaxProtocolFailovers int
}

// ExecutionPolicyFor selects the retry policy for a capability and stream
// mode. Unimplemented combinations return ErrExecutionPolicyUnavailable.
func ExecutionPolicyFor(capability config.RouterCapability, stream bool) (ExecutionPolicy, error) {
	if capability == config.RouterCapabilityText && !stream {
		return ExecutionPolicy{Capability: capability, MaxProtocolFailovers: 1}, nil
	}
	return ExecutionPolicy{}, fmt.Errorf("%w: capability %q stream=%t", ErrExecutionPolicyUnavailable, capability, stream)
}

// ExecutionRequest is one protocol-neutral inference request for the
// coordinator.
type ExecutionRequest struct {
	RequestID     string
	PublicModel   string
	AffinityKey   string
	EntryProtocol config.RouterProtocol
	Endpoint      string
	Stream        bool
	Body          []byte
	Headers       http.Header
	Query         url.Values
	// BodyValidator reports whether a 2xx body is well formed. A nil
	// validator accepts every 2xx body. Its error is never retained.
	BodyValidator func([]byte) error
}

// AttemptRecord is the sanitized diagnostic record of one executed attempt.
// It intentionally has no header or body fields.
type AttemptRecord struct {
	RouteID         string
	UpstreamID      string
	StatusCode      int
	FailureCategory FailureCategory
	HalfOpenProbe   bool
}

// ExecutionResult is the internal result object for one coordinated request.
// It carries identifiers, status codes, and failure categories only; raw
// headers, bodies, and secret values must never be stored in it.
type ExecutionResult struct {
	RequestID        string
	SnapshotRevision uint64
	RouteID          string
	UpstreamID       string
	StatusCode       int
	Success          bool
	FailureCategory  FailureCategory
	Attempts         []AttemptRecord
}

// ExecutionError is the safe upstream error returned when execution fails. It
// retains status and retry metadata without any upstream payload.
type ExecutionError struct {
	StatusCode int
	Category   FailureCategory
	RouteID    string
	UpstreamID string
}

func (e *ExecutionError) Error() string {
	if e == nil {
		return "router execution failed"
	}
	return fmt.Sprintf("router execution failed: route %q upstream %q status %d category %q",
		e.RouteID, e.UpstreamID, e.StatusCode, e.Category)
}

// Coordinator executes one request plan route by route through an injected
// AttemptExecutor and records every circuit outcome exactly once.
type Coordinator struct {
	selector *Selector
	executor AttemptExecutor
}

func NewCoordinator(selector *Selector, executor AttemptExecutor) *Coordinator {
	return &Coordinator{selector: selector, executor: executor}
}

// Execute runs one non-stream request until success, a terminal failure, a
// cancellation, or route exhaustion. On success it returns a defensive copy
// of the upstream response; the returned ExecutionResult always carries the
// sanitized attempt diagnostics.
func (c *Coordinator) Execute(ctx context.Context, request ExecutionRequest) (*AttemptResponse, ExecutionResult, error) {
	result := ExecutionResult{RequestID: request.RequestID}
	if c == nil || c.selector == nil || c.executor == nil {
		return nil, result, fmt.Errorf("execute router request: %w", ErrExecutorNotConfigured)
	}

	plan, err := c.selector.Begin(SelectionRequest{
		PublicModel: request.PublicModel,
		AffinityKey: request.AffinityKey,
	})
	if err != nil {
		return nil, result, err
	}
	result.SnapshotRevision = plan.SnapshotRevision()

	group := plan.ModelGroup()
	policy, err := ExecutionPolicyFor(group.Capability, request.Stream)
	if err != nil {
		return nil, result, err
	}
	endpoint := strings.ToLower(strings.TrimSpace(request.Endpoint))
	if err := validateExecutionTarget(plan.snapshot, group, endpoint); err != nil {
		return nil, result, err
	}

	protocolFailures := 0
	var lastFailure *ExecutionError
	for {
		selection, errNext := plan.Next()
		if errNext != nil {
			if lastFailure != nil {
				return nil, result, lastFailure
			}
			return nil, result, errNext
		}

		outcome := c.runAttempt(ctx, plan, selection, request, endpoint, policy)
		if !outcome.executed {
			// The admitted attempt was never executed and has been abandoned.
			result.FailureCategory = FailureCancelled
			return nil, result, cancellationError(ctx, len(result.Attempts))
		}

		result.Attempts = append(result.Attempts, AttemptRecord{
			RouteID:         selection.Route.ID,
			UpstreamID:      selection.Upstream.ID,
			StatusCode:      outcome.statusCode,
			FailureCategory: outcome.category,
			HalfOpenProbe:   selection.HalfOpenProbe,
		})
		result.RouteID = selection.Route.ID
		result.UpstreamID = selection.Upstream.ID
		result.StatusCode = outcome.statusCode
		result.FailureCategory = outcome.category
		if outcome.reportErr != nil {
			return nil, result, fmt.Errorf("record router attempt result: %w", outcome.reportErr)
		}

		switch outcome.decision {
		case decisionSuccess:
			result.Success = true
			return cloneAttemptResponse(outcome.response), result, nil
		case decisionCancelled:
			return nil, result, cancellationError(ctx, len(result.Attempts))
		case decisionTerminal:
			return nil, result, executionErrorFor(selection, outcome)
		default:
			lastFailure = executionErrorFor(selection, outcome)
			if outcome.category == FailureProtocol {
				protocolFailures++
				if protocolFailures > policy.MaxProtocolFailovers {
					return nil, result, lastFailure
				}
			}
		}
	}
}

type attemptDecision int

const (
	decisionRetry attemptDecision = iota
	decisionSuccess
	decisionTerminal
	decisionCancelled
)

type attemptOutcome struct {
	executed   bool
	decision   attemptDecision
	statusCode int
	category   FailureCategory
	retryAfter time.Duration
	response   AttemptResponse
	reportErr  error
}

// runAttempt owns one admitted selection and guarantees exactly one Report or
// Abandon call for it, even if the executor panics.
func (c *Coordinator) runAttempt(ctx context.Context, plan *RequestPlan, selection Selection, request ExecutionRequest, endpoint string, policy ExecutionPolicy) attemptOutcome {
	settled := false
	defer func() {
		if !settled {
			_ = plan.Abandon(selection)
		}
	}()

	if ctx.Err() != nil {
		// Never executed; the deferred abandon releases the selection.
		return attemptOutcome{decision: decisionCancelled, category: FailureCancelled}
	}

	response, execErr := c.executor.ExecuteAttempt(ctx, buildAttemptRequest(request, endpoint, selection))
	outcome := policy.classifyAttempt(ctx, response, execErr, request.BodyValidator, c.selector.now())
	outcome.executed = true
	outcome.reportErr = plan.Report(selection, AttemptResult{
		Success:    outcome.decision == decisionSuccess,
		Category:   outcome.category,
		RetryAfter: outcome.retryAfter,
	})
	settled = true
	return outcome
}

// classifyAttempt applies the policy's retry matrix to one observed attempt.
// Client cancellation takes precedence over whatever the executor returned.
func (p ExecutionPolicy) classifyAttempt(ctx context.Context, response AttemptResponse, execErr error, validator func([]byte) error, now time.Time) attemptOutcome {
	statusCode := 0
	if execErr == nil {
		statusCode = response.StatusCode
	}
	if ctx.Err() != nil {
		return attemptOutcome{decision: decisionCancelled, category: FailureCancelled, statusCode: statusCode}
	}
	if execErr != nil {
		// Transport failure before response headers.
		return attemptOutcome{decision: decisionRetry, category: FailureTransient}
	}

	if statusCode >= 200 && statusCode <= 299 {
		if validator != nil {
			if err := validator(response.Body); err != nil {
				// Malformed success. The validator error is dropped because it
				// may quote the response body.
				return attemptOutcome{decision: decisionRetry, category: FailureProtocol, statusCode: statusCode}
			}
		}
		return attemptOutcome{decision: decisionSuccess, statusCode: statusCode, response: response}
	}

	switch statusCode {
	case 401, 403:
		return attemptOutcome{decision: decisionRetry, category: FailureAuth, statusCode: statusCode}
	case 429:
		retryAfter, _ := ParseRetryAfter(response.Headers.Get("Retry-After"), now)
		return attemptOutcome{decision: decisionRetry, category: FailureRateLimit, statusCode: statusCode, retryAfter: retryAfter}
	case 408, 500, 502, 503, 504:
		return attemptOutcome{decision: decisionRetry, category: FailureTransient, statusCode: statusCode}
	case 400, 404, 409, 422:
		return attemptOutcome{decision: decisionTerminal, category: FailureClient, statusCode: statusCode}
	default:
		// Every other status is terminal for the text non-stream policy.
		return attemptOutcome{decision: decisionTerminal, category: FailureClient, statusCode: statusCode}
	}
}

// validateExecutionTarget checks endpoint and capability support before the
// first route selection.
func validateExecutionTarget(snapshot *Snapshot, group ModelGroup, endpoint string) error {
	if !capabilitySupportsEndpoint(group.Capability, endpoint) {
		return fmt.Errorf("%w: endpoint %q for capability %q", ErrEndpointNotSupported, endpoint, group.Capability)
	}
	for _, route := range group.Routes {
		if !route.Enabled {
			continue
		}
		upstream, ok := snapshot.Upstream(route.UpstreamID)
		if !ok || !upstream.Enabled {
			continue
		}
		if upstreamSupportsCapability(upstream.Capabilities, group.Capability) {
			return nil
		}
	}
	return fmt.Errorf("%w: no enabled upstream serves endpoint %q", ErrEndpointNotSupported, endpoint)
}

func capabilitySupportsEndpoint(capability config.RouterCapability, endpoint string) bool {
	switch capability {
	case config.RouterCapabilityText:
		switch endpoint {
		case config.RouterEndpointResponses,
			config.RouterEndpointChatCompletions,
			config.RouterEndpointMessages,
			config.RouterEndpointCountTokens:
			return true
		}
	case config.RouterCapabilityImage:
		return endpoint == config.RouterEndpointImages
	}
	return false
}

func upstreamSupportsCapability(capabilities config.RouterCapabilities, capability config.RouterCapability) bool {
	switch capability {
	case config.RouterCapabilityText:
		return capabilities.Supports(config.RouterEndpointResponses) ||
			capabilities.Supports(config.RouterEndpointChatCompletions) ||
			capabilities.Supports(config.RouterEndpointMessages)
	case config.RouterCapabilityImage:
		return capabilities.ImageGeneration && capabilities.Supports(config.RouterEndpointImages)
	}
	return false
}

func buildAttemptRequest(request ExecutionRequest, endpoint string, selection Selection) AttemptRequest {
	attemptSelection := selection
	attemptSelection.Upstream = cloneUpstream(selection.Upstream)
	return AttemptRequest{
		Selection:     attemptSelection,
		EntryProtocol: request.EntryProtocol,
		Endpoint:      endpoint,
		Body:          cloneBytes(request.Body),
		Headers:       cloneHeader(request.Headers),
		Query:         cloneQuery(request.Query),
	}
}

func executionErrorFor(selection Selection, outcome attemptOutcome) *ExecutionError {
	return &ExecutionError{
		StatusCode: outcome.statusCode,
		Category:   outcome.category,
		RouteID:    selection.Route.ID,
		UpstreamID: selection.Upstream.ID,
	}
}

func cancellationError(ctx context.Context, attempts int) error {
	cause := ctx.Err()
	if cause == nil {
		cause = context.Canceled
	}
	return fmt.Errorf("router execution cancelled after %d attempt(s): %w", attempts, cause)
}

func cloneAttemptResponse(source AttemptResponse) *AttemptResponse {
	return &AttemptResponse{
		StatusCode: source.StatusCode,
		Headers:    cloneHeader(source.Headers),
		Body:       cloneBytes(source.Body),
	}
}

func cloneBytes(source []byte) []byte {
	if source == nil {
		return nil
	}
	return append([]byte(nil), source...)
}

func cloneHeader(source http.Header) http.Header {
	if source == nil {
		return nil
	}
	return source.Clone()
}

func cloneQuery(source url.Values) url.Values {
	if source == nil {
		return nil
	}
	cloned := make(url.Values, len(source))
	for key, values := range source {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}
