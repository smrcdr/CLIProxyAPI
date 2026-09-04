package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
	"github.com/tidwall/gjson"
	"golang.org/x/net/context"
)

const maxSmartRouterImageResponseBytes = 32 << 20

var (
	ErrSmartRouterImageRequest   = errors.New("smart router image request is unsupported")
	ErrSmartRouterInternalHeader = errors.New("smart router internal header is forbidden")
)

type smartRouterExecution struct {
	selector          *smartrouter.Selector
	coordinator       *smartrouter.Coordinator
	streamCoordinator *smartrouter.StreamCoordinator
	metrics           *smartrouter.RouterMetrics
}

func newSmartRouterExecution(selector *smartrouter.Selector, executor SmartRouterProtocolExecutor) *smartRouterExecution {
	attemptExecutor := NewSmartRouterAttemptExecutor(executor)
	return &smartRouterExecution{
		selector:          selector,
		coordinator:       smartrouter.NewCoordinator(selector, attemptExecutor),
		streamCoordinator: smartrouter.NewStreamCoordinator(selector, attemptExecutor),
		metrics:           selector.Metrics(),
	}
}

func (e *smartRouterExecution) HandlesModel(publicModel string) bool {
	return e != nil && e.selector != nil && e.selector.HandlesModel(publicModel)
}

func (e *smartRouterExecution) HandlesImageModel(publicModel string) bool {
	return e != nil && e.selector != nil && e.selector.HandlesModelCapability(publicModel, config.RouterCapabilityImage)
}

func (e *smartRouterExecution) Execute(ctx context.Context, entryProtocol, publicModel string, body []byte) ([]byte, http.Header, *interfaces.ErrorMessage) {
	protocol, endpoint, ok := smartRouterExecutionTarget(entryProtocol)
	if !ok {
		return nil, nil, smartRouterPublicError(smartrouter.ErrEndpointNotSupported)
	}
	return e.executeNonStream(ctx, protocol, endpoint, publicModel, body)
}

func (e *smartRouterExecution) ExecuteCount(ctx context.Context, entryProtocol, publicModel string, body []byte) ([]byte, http.Header, *interfaces.ErrorMessage) {
	protocol, _, ok := smartRouterExecutionTarget(entryProtocol)
	if !ok {
		return nil, nil, smartRouterPublicError(smartrouter.ErrEndpointNotSupported)
	}
	return e.executeNonStream(ctx, protocol, config.RouterEndpointCountTokens, publicModel, body)
}

func (e *smartRouterExecution) ExecuteImage(ctx context.Context, publicModel string, body []byte) ([]byte, http.Header, *interfaces.ErrorMessage) {
	requestID := smartRouterRequestID(ctx)
	headers := headersFromContext(ctx)
	if err := validateSmartRouterInboundHeaders(headers); err != nil {
		result := smartrouter.ExecutionResult{RequestID: requestID, FailureCategory: smartrouter.FailureClient}
		e.metrics.Record(publicModel, config.RouterEndpointImages, result, 0, 0, "")
		return nil, nil, smartRouterPublicErrorWithResult(err, result)
	}
	if err := validateSmartRouterImageRequest(body); err != nil {
		result := smartrouter.ExecutionResult{RequestID: requestID, FailureCategory: smartrouter.FailureClient}
		e.metrics.Record(publicModel, config.RouterEndpointImages, result, 0, 0, "")
		return nil, nil, smartRouterPublicErrorWithResult(err, result)
	}
	startedAt := time.Now()
	response, result, err := e.coordinator.Execute(ctx, smartrouter.ExecutionRequest{
		RequestID:     requestID,
		PublicModel:   publicModel,
		AffinityKey:   smartRouterAffinityKey(headers),
		EntryProtocol: config.RouterProtocolOpenAIResponses,
		Endpoint:      config.RouterEndpointImages,
		Body:          cloneBytes(body),
		Headers:       headers,
		Query:         queryFromContext(ctx),
		BodyValidator: validateSmartRouterImageResponse,
	})
	e.metrics.Record(publicModel, config.RouterEndpointImages, result, time.Since(startedAt), 0, "")
	if err != nil {
		return nil, nil, smartRouterPublicErrorWithResult(err, result)
	}
	if response == nil {
		return nil, nil, smartRouterPublicErrorWithResult(errors.New("smart router returned no image response"), result)
	}
	return cloneBytes(response.Body), smartRouterDiagnosticHeaders(response.Headers, result), nil
}

func (e *smartRouterExecution) executeNonStream(ctx context.Context, protocol config.RouterProtocol, endpoint, publicModel string, body []byte) ([]byte, http.Header, *interfaces.ErrorMessage) {
	headers := headersFromContext(ctx)
	requestID := smartRouterRequestID(ctx)
	if err := validateSmartRouterInboundHeaders(headers); err != nil {
		result := smartrouter.ExecutionResult{RequestID: requestID, FailureCategory: smartrouter.FailureClient}
		e.metrics.Record(publicModel, endpoint, result, 0, 0, "")
		return nil, nil, smartRouterPublicErrorWithResult(err, result)
	}
	startedAt := time.Now()
	response, result, err := e.coordinator.Execute(ctx, smartrouter.ExecutionRequest{
		RequestID:     requestID,
		PublicModel:   publicModel,
		AffinityKey:   smartRouterAffinityKey(headers),
		EntryProtocol: protocol,
		Endpoint:      endpoint,
		Body:          cloneBytes(body),
		Headers:       headers,
		Query:         queryFromContext(ctx),
		BodyValidator: validateSmartRouterJSON,
	})
	e.metrics.Record(publicModel, endpoint, result, time.Since(startedAt), 0, "")
	if err != nil {
		return nil, nil, smartRouterPublicErrorWithResult(err, result)
	}
	if response == nil {
		return nil, nil, smartRouterPublicErrorWithResult(errors.New("smart router returned no response"), result)
	}
	return cloneBytes(response.Body), smartRouterDiagnosticHeaders(response.Headers, result), nil
}

func (e *smartRouterExecution) ExecuteStream(ctx context.Context, entryProtocol, publicModel string, body []byte) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	protocol, endpoint, ok := smartRouterExecutionTarget(entryProtocol)
	if !ok {
		return smartRouterImmediateStreamError(smartRouterPublicError(smartrouter.ErrEndpointNotSupported))
	}

	headers := headersFromContext(ctx)
	requestID := smartRouterRequestID(ctx)
	if err := validateSmartRouterInboundHeaders(headers); err != nil {
		result := smartrouter.ExecutionResult{RequestID: requestID, FailureCategory: smartrouter.FailureClient}
		e.metrics.Record(publicModel, endpoint, result, 0, 0, "")
		return smartRouterImmediateStreamError(smartRouterPublicErrorWithResult(err, result))
	}
	startedAt := time.Now()
	sink := newSmartRouterChannelSink(ctx, requestID)
	go func() {
		result, err := e.streamCoordinator.ExecuteStream(ctx, smartrouter.ExecutionRequest{
			RequestID:     requestID,
			PublicModel:   publicModel,
			AffinityKey:   smartRouterAffinityKey(headers),
			EntryProtocol: protocol,
			Endpoint:      endpoint,
			Stream:        true,
			Body:          cloneBytes(body),
			Headers:       headers,
			Query:         queryFromContext(ctx),
		}, sink)
		e.metrics.Record(publicModel, endpoint, result.ExecutionResult, time.Since(startedAt), sink.timeToFirstToken(), result.StreamState)
		if !result.Committed {
			sink.startFailure(smartRouterPublicErrorWithResult(err, result.ExecutionResult))
		}
		sink.close()
	}()

	select {
	case start := <-sink.ready:
		if start.err != nil {
			return sink.data, nil, sink.errs
		}
		return sink.data, start.headers, sink.errs
	case <-ctx.Done():
		sink.startFailure(smartRouterPublicError(ctx.Err()))
		start := <-sink.ready
		return sink.data, start.headers, sink.errs
	}
}

func smartRouterExecutionTarget(entryProtocol string) (config.RouterProtocol, string, bool) {
	switch strings.ToLower(strings.TrimSpace(entryProtocol)) {
	case "openai":
		return config.RouterProtocolOpenAIChatCompletions, config.RouterEndpointChatCompletions, true
	case "openai-response":
		return config.RouterProtocolOpenAIResponses, config.RouterEndpointResponses, true
	case "claude":
		return config.RouterProtocolAnthropicMessages, config.RouterEndpointMessages, true
	default:
		return "", "", false
	}
}

func validateSmartRouterJSON(body []byte) error {
	if !json.Valid(body) {
		return errors.New("invalid upstream JSON")
	}
	return nil
}

func validateSmartRouterImageRequest(body []byte) error {
	if !json.Valid(body) {
		return fmt.Errorf("%w: body must be valid JSON", ErrSmartRouterImageRequest)
	}
	root := gjson.ParseBytes(body)
	if root.Get("stream").Bool() {
		return fmt.Errorf("%w: streaming is not supported", ErrSmartRouterImageRequest)
	}
	if n := root.Get("n"); n.Exists() && (n.Type != gjson.Number || n.Float() != 1) {
		return fmt.Errorf("%w: n must equal 1", ErrSmartRouterImageRequest)
	}
	return nil
}

func validateSmartRouterImageResponse(body []byte) error {
	if len(body) > maxSmartRouterImageResponseBytes {
		return errors.New("upstream image response exceeds size limit")
	}
	if !json.Valid(body) {
		return errors.New("upstream image response is invalid JSON")
	}
	data := gjson.GetBytes(body, "data")
	if !data.IsArray() || len(data.Array()) != 1 {
		return errors.New("upstream image response must contain one output")
	}
	item := data.Array()[0]
	if strings.TrimSpace(item.Get("b64_json").String()) == "" && strings.TrimSpace(item.Get("url").String()) == "" {
		return errors.New("upstream image response has no output")
	}
	return nil
}

func smartRouterAffinityKey(headers http.Header) string {
	return strings.TrimSpace(headers.Get(smartRouterAffinityHeader))
}

func validateSmartRouterInboundHeaders(headers http.Header) error {
	for key := range headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		if lower == strings.ToLower(smartRouterAffinityHeader) {
			continue
		}
		if strings.HasPrefix(lower, "x-smartapi-") ||
			strings.HasPrefix(lower, "x-smartcli-") ||
			strings.HasPrefix(lower, "x-smartrouter-") {
			return fmt.Errorf("%w: %s", ErrSmartRouterInternalHeader, http.CanonicalHeaderKey(key))
		}
	}
	return nil
}

func sanitizeSmartRouterResponseHeaders(source http.Header) http.Header {
	headers := cloneHeader(source)
	for key := range headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		if strings.HasPrefix(lower, "x-smartapi-") ||
			strings.HasPrefix(lower, "x-smartcli-") ||
			strings.HasPrefix(lower, "x-smartrouter-") {
			headers.Del(key)
		}
	}
	return headers
}

func smartRouterDiagnosticHeaders(source http.Header, result smartrouter.ExecutionResult) http.Header {
	headers := sanitizeSmartRouterResponseHeaders(source)
	if headers == nil {
		headers = make(http.Header)
	}
	if result.RequestID != "" {
		headers.Set("X-SmartRouter-Request-Id", result.RequestID)
	}
	if result.UpstreamID != "" {
		headers.Set("X-SmartRouter-Upstream", result.UpstreamID)
	}
	if result.RouteID != "" {
		headers.Set("X-SmartRouter-Route", result.RouteID)
	}
	if len(result.Attempts) > 0 {
		headers.Set("X-SmartRouter-Attempts", strconv.Itoa(len(result.Attempts)))
	}
	return headers
}

func smartRouterRequestID(ctx context.Context) string {
	requestID := strings.TrimSpace(logging.GetRequestID(ctx))
	if requestID == "" {
		requestID = logging.GenerateRequestID()
	}
	return requestID
}

func smartRouterPublicError(err error) *interfaces.ErrorMessage {
	status := http.StatusBadGateway
	var executionErr *smartrouter.ExecutionError
	switch {
	case errors.As(err, &executionErr) &&
		executionErr.Category == smartrouter.FailureImageAmbiguous &&
		executionErr.StatusCode >= http.StatusOK &&
		executionErr.StatusCode < http.StatusMultipleChoices:
		status = http.StatusBadGateway
	case errors.As(err, &executionErr) && executionErr.StatusCode > 0:
		status = executionErr.StatusCode
	case errors.Is(err, smartrouter.ErrEndpointNotSupported),
		errors.Is(err, ErrSmartRouterConversion),
		errors.Is(err, ErrSmartRouterProtocolMapping),
		errors.Is(err, ErrSmartRouterImageRequest),
		errors.Is(err, smartrouter.ErrExecutionPolicyUnavailable):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, ErrSmartRouterInternalHeader):
		status = http.StatusBadRequest
	case errors.Is(err, smartrouter.ErrNoRouteAvailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, context.Canceled):
		status = 499
	}
	return &interfaces.ErrorMessage{
		StatusCode: status,
		Error:      errors.New("smart router request failed"),
	}
}

func smartRouterPublicErrorWithResult(err error, result smartrouter.ExecutionResult) *interfaces.ErrorMessage {
	message := smartRouterPublicError(err)
	message.Addon = smartRouterDiagnosticHeaders(nil, result)
	return message
}

type smartRouterStreamStart struct {
	headers http.Header
	err     *interfaces.ErrorMessage
}

type smartRouterChannelSink struct {
	ctx            context.Context
	requestID      string
	startedAt      time.Time
	firstPayloadAt time.Time
	timingMu       sync.Mutex
	ready          chan smartRouterStreamStart
	data           chan []byte
	errs           chan *interfaces.ErrorMessage
	once           sync.Once
}

func newSmartRouterChannelSink(ctx context.Context, requestID string) *smartRouterChannelSink {
	return &smartRouterChannelSink{
		ctx:       ctx,
		requestID: requestID,
		startedAt: time.Now(),
		ready:     make(chan smartRouterStreamStart, 1),
		data:      make(chan []byte, 16),
		errs:      make(chan *interfaces.ErrorMessage, 1),
	}
}

func (s *smartRouterChannelSink) Commit(commit smartrouter.StreamCommit) error {
	start := smartRouterStreamStart{headers: smartRouterDiagnosticHeaders(commit.Headers, smartrouter.ExecutionResult{
		RequestID:  s.requestID,
		RouteID:    commit.RouteID,
		UpstreamID: commit.UpstreamID,
	})}
	s.once.Do(func() {
		s.ready <- start
	})
	return nil
}

func (s *smartRouterChannelSink) Send(payload []byte) error {
	s.timingMu.Lock()
	if s.firstPayloadAt.IsZero() {
		s.firstPayloadAt = time.Now()
	}
	s.timingMu.Unlock()
	select {
	case s.data <- cloneBytes(payload):
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *smartRouterChannelSink) timeToFirstToken() time.Duration {
	if s == nil {
		return 0
	}
	s.timingMu.Lock()
	defer s.timingMu.Unlock()
	if s.firstPayloadAt.IsZero() || s.startedAt.IsZero() {
		return 0
	}
	return s.firstPayloadAt.Sub(s.startedAt)
}

func (s *smartRouterChannelSink) Fail(err *smartrouter.ExecutionError) error {
	select {
	case s.errs <- smartRouterPublicError(err):
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *smartRouterChannelSink) startFailure(err *interfaces.ErrorMessage) {
	s.once.Do(func() {
		s.errs <- err
		s.ready <- smartRouterStreamStart{err: err}
	})
}

func (s *smartRouterChannelSink) close() {
	close(s.data)
	close(s.errs)
}

func smartRouterImmediateStreamError(err *interfaces.ErrorMessage) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- err
	close(data)
	close(errs)
	return data, nil, errs
}

var _ smartrouter.StreamSink = (*smartRouterChannelSink)(nil)
