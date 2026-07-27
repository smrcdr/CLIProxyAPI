package smartrouter

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// StreamCoordinator executes one streaming request plan route by route. It may
// select another route only while the downstream response is uncommitted;
// after the first semantic event it forwards the remaining chunks and reports
// a partial failure instead of retrying.
type StreamCoordinator struct {
	selector *Selector
	executor StreamAttemptExecutor
}

func NewStreamCoordinator(selector *Selector, executor StreamAttemptExecutor) *StreamCoordinator {
	return &StreamCoordinator{selector: selector, executor: executor}
}

// StreamExecutionResult extends the non-stream diagnostics with the terminal
// stream state. It carries identifiers and categories only.
type StreamExecutionResult struct {
	ExecutionResult
	StreamState StreamState
	Committed   bool
}

// ExecuteStream runs one streaming request until the stream completes, a
// terminal failure occurs, the client cancels, or the routes are exhausted.
// Once the sink is committed the returned error describes a partial failure
// that has already been reported to the sink.
func (c *StreamCoordinator) ExecuteStream(ctx context.Context, request ExecutionRequest, sink StreamSink) (StreamExecutionResult, error) {
	result := StreamExecutionResult{
		ExecutionResult: ExecutionResult{RequestID: request.RequestID},
		StreamState:     StreamStateNotStarted,
	}
	if c == nil || c.selector == nil || c.executor == nil {
		return result, fmt.Errorf("execute router stream: %w", ErrExecutorNotConfigured)
	}
	if sink == nil {
		return result, fmt.Errorf("execute router stream: %w", ErrStreamSinkNotConfigured)
	}
	if !request.Stream {
		return result, fmt.Errorf("%w: stream execution requires a streaming request", ErrStreamModeMismatch)
	}

	plan, err := c.selector.Begin(SelectionRequest{
		PublicModel: request.PublicModel,
		AffinityKey: request.AffinityKey,
	})
	if err != nil {
		return result, err
	}
	result.SnapshotRevision = plan.SnapshotRevision()

	group := plan.ModelGroup()
	policy, err := ExecutionPolicyFor(group.Capability, true)
	if err != nil {
		return result, err
	}
	endpoint := strings.ToLower(strings.TrimSpace(request.Endpoint))
	if err = validateExecutionTarget(plan.snapshot, group, endpoint); err != nil {
		return result, err
	}
	classifier, err := streamClassifierFor(request.EntryProtocol)
	if err != nil {
		return result, err
	}

	protocolFailures := 0
	var lastFailure *ExecutionError
	for {
		selection, errNext := plan.Next()
		if errNext != nil {
			if lastFailure != nil {
				return result, lastFailure
			}
			return result, errNext
		}

		outcome := c.runStreamAttempt(ctx, plan, selection, request, endpoint, policy, classifier, sink)
		if !outcome.executed {
			result.FailureCategory = FailureCancelled
			return result, cancellationError(ctx, len(result.Attempts))
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
		result.StreamState = outcome.state
		result.Committed = outcome.committed
		if outcome.reportErr != nil {
			return result, fmt.Errorf("record router stream attempt result: %w", outcome.reportErr)
		}
		if outcome.sinkErr != nil {
			return result, outcome.sinkErr
		}

		switch outcome.decision {
		case decisionSuccess:
			result.Success = true
			result.Usage = outcome.usage
			result.UsageMissing = outcome.usageMissing
			return result, nil
		case decisionCancelled:
			return result, cancellationError(ctx, len(result.Attempts))
		case decisionTerminal:
			return result, executionErrorFor(selection, outcome.attemptOutcome)
		default:
			lastFailure = executionErrorFor(selection, outcome.attemptOutcome)
			if outcome.category == FailureProtocol {
				protocolFailures++
				if protocolFailures > policy.MaxProtocolFailovers {
					return result, lastFailure
				}
			}
		}
	}
}

type streamAttemptOutcome struct {
	attemptOutcome
	state        StreamState
	committed    bool
	sinkErr      error
	usage        *CanonicalUsage
	usageMissing bool
}

// runStreamAttempt owns one admitted selection for the duration of one
// upstream stream and guarantees exactly one Report or Abandon call for it.
func (c *StreamCoordinator) runStreamAttempt(
	ctx context.Context,
	plan *RequestPlan,
	selection Selection,
	request ExecutionRequest,
	endpoint string,
	policy ExecutionPolicy,
	classifier streamClassifier,
	sink StreamSink,
) streamAttemptOutcome {
	settled := false
	defer func() {
		if !settled {
			_ = plan.Abandon(selection)
		}
	}()

	if ctx.Err() != nil {
		return streamAttemptOutcome{
			attemptOutcome: attemptOutcome{decision: decisionCancelled, category: FailureCancelled},
			state:          StreamStateNotStarted,
		}
	}

	// A dedicated attempt context guarantees that abandoning this attempt for
	// a failover releases the upstream reader goroutine.
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()

	attempt, execErr := c.executor.ExecuteStreamAttempt(attemptCtx, buildAttemptRequest(request, endpoint, selection))
	if attempt != nil && attempt.Close != nil {
		// Cancel before closing so an executor whose Close drains its own
		// reader goroutine cannot deadlock against this attempt.
		defer func() {
			cancelAttempt()
			attempt.Close()
		}()
	}
	outcome := c.consumeStream(ctx, attempt, execErr, policy, classifier, selection, sink)
	outcome.executed = true
	outcome.reportErr = plan.Report(selection, AttemptResult{
		Success:    outcome.decision == decisionSuccess,
		Category:   outcome.category,
		RetryAfter: outcome.retryAfter,
	})
	settled = true
	return outcome
}

// consumeStream drives one upstream stream through the commitment state
// machine using the caller's context for cancellation.
func (c *StreamCoordinator) consumeStream(
	clientCtx context.Context,
	attempt *StreamAttempt,
	execErr error,
	policy ExecutionPolicy,
	classifier streamClassifier,
	selection Selection,
	sink StreamSink,
) (result streamAttemptOutcome) {
	usage := newStreamUsageAccumulator(classifier.protocol)
	defer func() {
		if result.decision == decisionSuccess {
			result.usage, _ = usage.Result()
			result.usageMissing = result.usage == nil
		}
	}()
	if clientCtx.Err() != nil {
		return streamAttemptOutcome{
			attemptOutcome: attemptOutcome{decision: decisionCancelled, category: FailureCancelled},
			state:          StreamStateNotStarted,
		}
	}
	if execErr != nil {
		// Connection failure before response headers.
		return streamAttemptOutcome{
			attemptOutcome: attemptOutcome{decision: decisionRetry, category: FailureTransient},
			state:          StreamStateNotStarted,
		}
	}
	if attempt == nil {
		return streamAttemptOutcome{
			attemptOutcome: attemptOutcome{decision: decisionRetry, category: FailureProtocol},
			state:          StreamStateNotStarted,
		}
	}

	statusCode := attempt.StatusCode
	if statusCode < 200 || statusCode > 299 {
		classified := policy.classifyStatus(statusCode, attempt.Headers, c.selector.now())
		return streamAttemptOutcome{attemptOutcome: classified, state: StreamStateHeadersReceived}
	}
	if attempt.Chunks == nil {
		return streamAttemptOutcome{
			attemptOutcome: attemptOutcome{decision: decisionRetry, category: FailureProtocol, statusCode: statusCode},
			state:          StreamStateHeadersReceived,
		}
	}

	uncommitted := streamAttemptOutcome{
		attemptOutcome: attemptOutcome{decision: decisionRetry, category: FailureProtocol, statusCode: statusCode},
		state:          StreamStateUncommitted,
	}
	committed := false
	var prelude streamPrelude

	// commit flushes the buffered prelude and switches the attempt into
	// forward-only mode.
	commit := func() error {
		if errCommit := sink.Commit(StreamCommit{
			StatusCode:       statusCode,
			Headers:          cloneHeader(attempt.Headers),
			RouteID:          selection.Route.ID,
			UpstreamID:       selection.Upstream.ID,
			SnapshotRevision: selection.SnapshotRevision,
		}); errCommit != nil {
			return errCommit
		}
		committed = true
		if buffered := prelude.bytes(); len(buffered) > 0 {
			if errSend := sink.Send(buffered); errSend != nil {
				return errSend
			}
		}
		prelude = streamPrelude{}
		return nil
	}

	sinkFailure := func(err error) streamAttemptOutcome {
		return streamAttemptOutcome{
			attemptOutcome: attemptOutcome{
				decision:   decisionTerminal,
				category:   FailureCancelled,
				statusCode: statusCode,
			},
			state:     StreamStateFailedPartial,
			committed: committed,
			sinkErr:   err,
		}
	}

	// partialFailure reports a post-commit upstream failure to the sink. No
	// other route may be started once the downstream is committed.
	partialFailure := func(category FailureCategory, failStatus int) streamAttemptOutcome {
		outcome := streamAttemptOutcome{
			attemptOutcome: attemptOutcome{
				decision:   decisionTerminal,
				category:   category,
				statusCode: statusCode,
			},
			state:     StreamStateFailedPartial,
			committed: true,
		}
		if errFail := sink.Fail(&ExecutionError{
			StatusCode: failStatus,
			Category:   category,
			RouteID:    selection.Route.ID,
			UpstreamID: selection.Upstream.ID,
		}); errFail != nil {
			outcome.sinkErr = errFail
		}
		return outcome
	}

	for {
		select {
		case <-clientCtx.Done():
			outcome := streamAttemptOutcome{
				attemptOutcome: attemptOutcome{
					decision:   decisionCancelled,
					category:   FailureCancelled,
					statusCode: statusCode,
				},
				state:     StreamStateUncommitted,
				committed: committed,
			}
			if committed {
				outcome.state = StreamStateFailedPartial
			}
			return outcome
		case chunk, ok := <-attempt.Chunks:
			if !ok {
				if committed {
					return streamAttemptOutcome{
						attemptOutcome: attemptOutcome{decision: decisionSuccess, statusCode: statusCode},
						state:          StreamStateCompleted,
						committed:      true,
					}
				}
				if prelude.size() > maxUncommittedBytes {
					return uncommitted
				}
				switch prelude.finalize(classifier) {
				case streamEventSemantic:
					if errCommit := commit(); errCommit != nil {
						return sinkFailure(errCommit)
					}
					return streamAttemptOutcome{
						attemptOutcome: attemptOutcome{decision: decisionSuccess, statusCode: statusCode},
						state:          StreamStateCompleted,
						committed:      true,
					}
				default:
					// The upstream closed without a semantic event or with
					// an incomplete/malformed final event. Another route may
					// still serve the request.
					return uncommitted
				}
			}

			if chunk.Err != nil {
				category, errStatus := streamChunkFailure(chunk.Err)
				if committed {
					return partialFailure(category, errStatus)
				}
				retryAfter := time.Duration(0)
				if category == FailureRateLimit {
					retryAfter, _ = ParseRetryAfter(attempt.Headers.Get("Retry-After"), c.selector.now())
				}
				failover := uncommitted
				failover.category = category
				failover.retryAfter = retryAfter
				if category == FailureClient {
					failover.decision = decisionTerminal
				}
				return failover
			}
			if len(chunk.Data) == 0 {
				continue
			}
			usage.Observe(chunk.Data)

			if committed {
				if errSend := sink.Send(chunk.Data); errSend != nil {
					return sinkFailure(errSend)
				}
				continue
			}

			kind := prelude.append(classifier, chunk.Data)
			if kind == streamEventIgnorable {
				// Keepalives, comments, and blank lines must not prevent
				// failover, so completed non-semantic events are dropped
				// instead of being buffered until the stream commits.
				prelude.pruneCompletedEvents()
			}
			if prelude.size() > maxUncommittedBytes {
				return uncommitted
			}

			switch kind {
			case streamEventMalformed:
				return uncommitted
			case streamEventSemantic:
				if errCommit := commit(); errCommit != nil {
					return sinkFailure(errCommit)
				}
			}
		}
	}
}

// classifyStatus applies the retry matrix to a non-2xx upstream status
// observed before any downstream commitment.
func (p ExecutionPolicy) classifyStatus(statusCode int, headers http.Header, now time.Time) attemptOutcome {
	if p.Capability == config.RouterCapabilityImage {
		switch statusCode {
		case 401, 403:
			return attemptOutcome{decision: decisionRetry, category: FailureAuth, statusCode: statusCode}
		case 429:
			retryAfter, _ := ParseRetryAfter(headers.Get("Retry-After"), now)
			return attemptOutcome{decision: decisionRetry, category: FailureRateLimit, statusCode: statusCode, retryAfter: retryAfter}
		case 408, 500, 502, 503, 504:
			return attemptOutcome{decision: decisionTerminal, category: FailureImageAmbiguous, statusCode: statusCode}
		default:
			return attemptOutcome{decision: decisionTerminal, category: FailureClient, statusCode: statusCode}
		}
	}
	switch statusCode {
	case 401, 403:
		return attemptOutcome{decision: decisionRetry, category: FailureAuth, statusCode: statusCode}
	case 429:
		retryAfter, _ := ParseRetryAfter(headers.Get("Retry-After"), now)
		return attemptOutcome{decision: decisionRetry, category: FailureRateLimit, statusCode: statusCode, retryAfter: retryAfter}
	case 408, 500, 502, 503, 504:
		return attemptOutcome{decision: decisionRetry, category: FailureTransient, statusCode: statusCode}
	default:
		return attemptOutcome{decision: decisionTerminal, category: FailureClient, statusCode: statusCode}
	}
}
