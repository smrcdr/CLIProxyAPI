package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const smartRouterAffinityHeader = "X-SmartAPI-Affinity-Key"

var (
	ErrSmartRouterProtocolMapping = errors.New("smart router protocol mapping is unavailable")
	ErrSmartRouterConversion      = errors.New("smart router protocol conversion is unavailable")
)

// SmartRouterProtocolExecutor is the existing route-level execution boundary
// used by SmartRouter. BaseAPIHandler implements this interface.
type SmartRouterProtocolExecutor interface {
	ExecuteProtocolWithAuthManager(context.Context, ProtocolExecutionRequest) (ModelExecutionResponse, *interfaces.ErrorMessage)
	ExecuteProtocolCountWithAuthManager(context.Context, ProtocolExecutionRequest) (ModelExecutionResponse, *interfaces.ErrorMessage)
	ExecuteProtocolStreamWithAuthManager(context.Context, ProtocolExecutionRequest) (ModelExecutionStream, *interfaces.ErrorMessage)
}

type smartRouterTranslatorRegistry interface {
	HasRequestTransformer(sdktranslator.Format, sdktranslator.Format) bool
	HasStreamResponseTransformer(sdktranslator.Format, sdktranslator.Format) bool
	HasNonStreamResponseTransformer(sdktranslator.Format, sdktranslator.Format) bool
}

type defaultSmartRouterTranslatorRegistry struct{}

func (defaultSmartRouterTranslatorRegistry) HasRequestTransformer(from, to sdktranslator.Format) bool {
	return sdktranslator.HasRequestTransformer(from, to)
}

func (defaultSmartRouterTranslatorRegistry) HasStreamResponseTransformer(from, to sdktranslator.Format) bool {
	return sdktranslator.HasStreamResponseTransformer(from, to)
}

func (defaultSmartRouterTranslatorRegistry) HasNonStreamResponseTransformer(from, to sdktranslator.Format) bool {
	return sdktranslator.HasNonStreamResponseTransformer(from, to)
}

// SmartRouterAttemptExecutor adapts selected routes to the existing handler,
// auth-manager, executor, and translator pipeline.
type SmartRouterAttemptExecutor struct {
	executor    SmartRouterProtocolExecutor
	translators smartRouterTranslatorRegistry
}

func NewSmartRouterAttemptExecutor(executor SmartRouterProtocolExecutor) *SmartRouterAttemptExecutor {
	return &SmartRouterAttemptExecutor{
		executor:    executor,
		translators: defaultSmartRouterTranslatorRegistry{},
	}
}

func (e *SmartRouterAttemptExecutor) ExecuteAttempt(ctx context.Context, request smartrouter.AttemptRequest) (smartrouter.AttemptResponse, error) {
	if e == nil || e.executor == nil {
		return smartrouter.AttemptResponse{}, fmt.Errorf("execute smart router attempt: %w", smartrouter.ErrExecutorNotConfigured)
	}
	protocolRequest, err := e.protocolRequest(request, false)
	if err != nil {
		if errors.Is(err, ErrSmartRouterConversion) || errors.Is(err, ErrSmartRouterProtocolMapping) {
			return smartrouter.AttemptResponse{StatusCode: http.StatusUnprocessableEntity}, nil
		}
		return smartrouter.AttemptResponse{}, err
	}
	var response ModelExecutionResponse
	var errMsg *interfaces.ErrorMessage
	if request.Endpoint == config.RouterEndpointCountTokens {
		response, errMsg = e.executor.ExecuteProtocolCountWithAuthManager(ctx, protocolRequest)
	} else {
		response, errMsg = e.executor.ExecuteProtocolWithAuthManager(ctx, protocolRequest)
	}
	if errMsg != nil {
		return smartRouterAttemptResponseFromError(errMsg), nil
	}
	return smartrouter.AttemptResponse{
		StatusCode: response.StatusCode,
		Headers:    cloneHeader(response.Headers),
		Body:       restoreSmartRouterPublicModel(response.Body, request.PublicModel),
	}, nil
}

func (e *SmartRouterAttemptExecutor) ExecuteStreamAttempt(ctx context.Context, request smartrouter.AttemptRequest) (*smartrouter.StreamAttempt, error) {
	if e == nil || e.executor == nil {
		return nil, fmt.Errorf("execute smart router stream attempt: %w", smartrouter.ErrExecutorNotConfigured)
	}
	protocolRequest, err := e.protocolRequest(request, true)
	if err != nil {
		if errors.Is(err, ErrSmartRouterConversion) || errors.Is(err, ErrSmartRouterProtocolMapping) {
			return &smartrouter.StreamAttempt{StatusCode: http.StatusUnprocessableEntity}, nil
		}
		return nil, err
	}
	stream, errMsg := e.executor.ExecuteProtocolStreamWithAuthManager(ctx, protocolRequest)
	if errMsg != nil {
		attempt := smartRouterStreamAttemptFromError(errMsg)
		return &attempt, nil
	}
	if stream.Chunks == nil {
		return &smartrouter.StreamAttempt{
			StatusCode: stream.StatusCode,
			Headers:    cloneHeader(stream.Headers),
		}, nil
	}

	chunks := make(chan smartrouter.StreamChunk)
	go func() {
		defer close(chunks)
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-stream.Chunks:
				if !ok {
					return
				}
				converted := smartrouter.StreamChunk{
					Data: restoreSmartRouterStreamPublicModel(chunk.Payload, request.PublicModel),
					Err:  smartRouterStreamChunkError(chunk.Err),
				}
				select {
				case <-ctx.Done():
					return
				case chunks <- converted:
				}
			}
		}
	}()

	return &smartrouter.StreamAttempt{
		StatusCode: stream.StatusCode,
		Headers:    cloneHeader(stream.Headers),
		Chunks:     chunks,
	}, nil
}

func (e *SmartRouterAttemptExecutor) protocolRequest(request smartrouter.AttemptRequest, stream bool) (ProtocolExecutionRequest, error) {
	if request.Endpoint == config.RouterEndpointImages {
		if stream {
			return ProtocolExecutionRequest{}, fmt.Errorf("%w: image streaming", ErrSmartRouterConversion)
		}
		if request.Selection.Upstream.Protocol == config.RouterProtocolAnthropicMessages {
			return ProtocolExecutionRequest{}, fmt.Errorf("%w: image upstream %q", ErrSmartRouterProtocolMapping, request.Selection.Upstream.Protocol)
		}
		return e.imageProtocolRequest(request), nil
	}
	entryFormat, err := smartRouterProtocolFormat(request.EntryProtocol)
	if err != nil {
		return ProtocolExecutionRequest{}, err
	}
	upstreamFormat, err := smartRouterProtocolFormat(request.Selection.Upstream.Protocol)
	if err != nil {
		return ProtocolExecutionRequest{}, err
	}
	if err = e.preflight(entryFormat, upstreamFormat, stream, request.Body); err != nil {
		return ProtocolExecutionRequest{}, err
	}

	headers := sanitizeSmartRouterAttemptHeaders(request.Headers)
	for key, value := range request.Selection.Upstream.Headers {
		headers.Set(key, value)
	}
	if request.Selection.Upstream.TrustedPool && request.Selection.Upstream.ForwardSmartAPIAffinity && strings.TrimSpace(request.AffinityKey) != "" {
		headers.Set(smartRouterAffinityHeader, strings.TrimSpace(request.AffinityKey))
	}

	return ProtocolExecutionRequest{
		EntryProtocol:      entryFormat.String(),
		ExitProtocol:       entryFormat.String(),
		ForcedProvider:     SmartRouterProviderID(request.Selection.Upstream.ID),
		AuthSelectionModel: request.Selection.Route.UpstreamModel,
		Model:              request.Selection.Route.UpstreamModel,
		Stream:             stream,
		Body:               cloneBytes(request.Body),
		Headers:            headers,
		Query:              cloneURLValues(request.Query),
	}, nil
}

func (e *SmartRouterAttemptExecutor) imageProtocolRequest(request smartrouter.AttemptRequest) ProtocolExecutionRequest {
	headers := sanitizeSmartRouterAttemptHeaders(request.Headers)
	for key, value := range request.Selection.Upstream.Headers {
		headers.Set(key, value)
	}
	if request.Selection.Upstream.TrustedPool && request.Selection.Upstream.ForwardSmartAPIAffinity && strings.TrimSpace(request.AffinityKey) != "" {
		headers.Set(smartRouterAffinityHeader, strings.TrimSpace(request.AffinityKey))
	}
	return ProtocolExecutionRequest{
		EntryProtocol:      "openai-image",
		ExitProtocol:       "openai-image",
		ForcedProvider:     SmartRouterProviderID(request.Selection.Upstream.ID),
		AuthSelectionModel: request.Selection.Route.UpstreamModel,
		Model:              request.Selection.Route.UpstreamModel,
		Body:               cloneBytes(request.Body),
		Headers:            headers,
		Query:              cloneURLValues(request.Query),
	}
}

func (e *SmartRouterAttemptExecutor) preflight(entry, upstream sdktranslator.Format, stream bool, body []byte) error {
	if entry == upstream {
		return nil
	}
	if e.translators == nil || !e.translators.HasRequestTransformer(entry, upstream) {
		return fmt.Errorf("%w: request %q to %q", ErrSmartRouterConversion, entry, upstream)
	}
	if stream {
		if !e.translators.HasStreamResponseTransformer(entry, upstream) {
			return fmt.Errorf("%w: streaming response %q to %q", ErrSmartRouterConversion, upstream, entry)
		}
		return validateSmartRouterConversionFeatures(entry, upstream, body)
	}
	if !e.translators.HasNonStreamResponseTransformer(entry, upstream) {
		return fmt.Errorf("%w: response %q to %q", ErrSmartRouterConversion, upstream, entry)
	}
	return validateSmartRouterConversionFeatures(entry, upstream, body)
}

func validateSmartRouterConversionFeatures(entry, upstream sdktranslator.Format, body []byte) error {
	root := gjson.ParseBytes(body)
	unsupported := ""
	switch {
	case entry == sdktranslator.FormatOpenAI && upstream == sdktranslator.FormatOpenAIResponse:
		unsupported = firstExistingSmartRouterFeature(root, "stop")
	case entry == sdktranslator.FormatOpenAI && upstream == sdktranslator.FormatClaude:
		unsupported = firstExistingSmartRouterFeature(root, "response_format", "service_tier")
	case entry == sdktranslator.FormatOpenAIResponse && upstream == sdktranslator.FormatClaude:
		unsupported = firstExistingSmartRouterFeature(root, "response_format", "text.format", "service_tier")
	case entry == sdktranslator.FormatClaude && upstream == sdktranslator.FormatOpenAI:
		unsupported = firstExistingSmartRouterFeature(root, "output_config.format", "service_tier")
	case entry == sdktranslator.FormatClaude && upstream == sdktranslator.FormatOpenAIResponse:
		unsupported = firstExistingSmartRouterFeature(root, "stop_sequences", "output_config.format", "service_tier")
	}
	if unsupported != "" {
		return fmt.Errorf("%w: feature %q is not representable from %q to %q", ErrSmartRouterConversion, unsupported, entry, upstream)
	}
	if unsupported = unsupportedSmartRouterToolFeature(entry, upstream, root); unsupported != "" {
		return fmt.Errorf("%w: feature %q is not representable from %q to %q", ErrSmartRouterConversion, unsupported, entry, upstream)
	}
	return nil
}

func firstExistingSmartRouterFeature(root gjson.Result, paths ...string) string {
	for _, path := range paths {
		if root.Get(path).Exists() {
			return path
		}
	}
	return ""
}

func unsupportedSmartRouterToolFeature(entry, upstream sdktranslator.Format, root gjson.Result) string {
	tools := root.Get("tools")
	if tools.Exists() && tools.IsArray() {
		unsupported := false
		tools.ForEach(func(_, tool gjson.Result) bool {
			toolType := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
			switch entry {
			case sdktranslator.FormatOpenAI:
				unsupported = toolType != "" && toolType != "function"
			case sdktranslator.FormatOpenAIResponse:
				switch upstream {
				case sdktranslator.FormatClaude:
					unsupported = !smartRouterResponsesToolSupportedByClaude(tool)
				default:
					unsupported = toolType != "" && toolType != "function"
				}
			case sdktranslator.FormatClaude:
				unsupported = toolType != ""
			}
			return !unsupported
		})
		if unsupported {
			return "tools[].type"
		}
	}

	switch {
	case entry == sdktranslator.FormatOpenAI && upstream == sdktranslator.FormatOpenAIResponse:
		if smartRouterChatHasRichToolResult(root.Get("messages")) {
			return "messages[].content"
		}
	case entry == sdktranslator.FormatOpenAIResponse && upstream == sdktranslator.FormatOpenAI:
		if smartRouterResponsesHasRichToolResult(root.Get("input")) {
			return "input[].output"
		}
	case entry == sdktranslator.FormatClaude:
		if smartRouterClaudeHasRichToolResult(root.Get("messages")) {
			return "messages[].content[].content"
		}
	}
	return ""
}

func smartRouterResponsesToolSupportedByClaude(tool gjson.Result) bool {
	switch strings.ToLower(strings.TrimSpace(tool.Get("type").String())) {
	case "", "function", "web_search":
		return true
	case "namespace":
		children := tool.Get("tools")
		if !children.Exists() || !children.IsArray() {
			return false
		}
		supported := true
		children.ForEach(func(_, child gjson.Result) bool {
			childType := strings.ToLower(strings.TrimSpace(child.Get("type").String()))
			supported = childType == "" || childType == "function"
			return supported
		})
		return supported
	default:
		return false
	}
}

func smartRouterChatHasRichToolResult(messages gjson.Result) bool {
	rich := false
	messages.ForEach(func(_, message gjson.Result) bool {
		role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
		content := message.Get("content")
		rich = (role == "tool" || role == "function") && content.Exists() && content.Type != gjson.String
		return !rich
	})
	return rich
}

func smartRouterResponsesHasRichToolResult(input gjson.Result) bool {
	rich := false
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "function_call_output" {
			return true
		}
		output := item.Get("output")
		rich = output.Exists() && output.Type != gjson.String
		return !rich
	})
	return rich
}

func smartRouterClaudeHasRichToolResult(messages gjson.Result) bool {
	rich := false
	messages.ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, part gjson.Result) bool {
			content := part.Get("content")
			rich = part.Get("type").String() == "tool_result" && content.Exists() && content.Type != gjson.String
			return !rich
		})
		return !rich
	})
	return rich
}

func smartRouterProtocolFormat(protocol config.RouterProtocol) (sdktranslator.Format, error) {
	switch protocol {
	case config.RouterProtocolOpenAIChatCompletions:
		return sdktranslator.FormatOpenAI, nil
	case config.RouterProtocolOpenAIResponses:
		return sdktranslator.FormatOpenAIResponse, nil
	case config.RouterProtocolAnthropicMessages:
		return sdktranslator.FormatClaude, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrSmartRouterProtocolMapping, protocol)
	}
}

// SmartRouterProviderID derives a stable internal provider ID from an upstream
// ID. Public model names are intentionally absent from this namespace.
func SmartRouterProviderID(upstreamID string) string {
	return "smartrouter-" + strings.ToLower(strings.TrimSpace(upstreamID))
}

func smartRouterErrorStatus(errMsg *interfaces.ErrorMessage) int {
	status := http.StatusBadGateway
	if errMsg != nil && errMsg.StatusCode > 0 {
		status = errMsg.StatusCode
	}
	return status
}

func smartRouterAttemptResponseFromError(errMsg *interfaces.ErrorMessage) smartrouter.AttemptResponse {
	return smartrouter.AttemptResponse{
		StatusCode: smartRouterErrorStatus(errMsg),
		Headers:    cloneHeader(errMsg.Addon),
	}
}

func smartRouterStreamAttemptFromError(errMsg *interfaces.ErrorMessage) smartrouter.StreamAttempt {
	return smartrouter.StreamAttempt{
		StatusCode: smartRouterErrorStatus(errMsg),
		Headers:    cloneHeader(errMsg.Addon),
	}
}

type smartRouterChunkStatusError struct {
	statusCode int
}

func (e smartRouterChunkStatusError) Error() string {
	return http.StatusText(e.statusCode)
}

func (e smartRouterChunkStatusError) StatusCode() int {
	return e.statusCode
}

func smartRouterStreamChunkError(source *ModelExecutionStreamError) error {
	if source == nil {
		return nil
	}
	if source.StatusCode > 0 {
		return smartRouterChunkStatusError{statusCode: source.StatusCode}
	}
	return errors.New("upstream stream failed")
}

func sanitizeSmartRouterAttemptHeaders(source http.Header) http.Header {
	headers := cloneHeader(source)
	if headers == nil {
		headers = make(http.Header)
	}
	for key := range headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		switch lower {
		case "authorization", "proxy-authorization", "cookie", "set-cookie", "host", "content-length":
			headers.Del(key)
		default:
			if strings.HasPrefix(lower, "x-smartapi-") || strings.HasPrefix(lower, "x-smartrouter-") {
				headers.Del(key)
			}
		}
	}
	return headers
}

func restoreSmartRouterPublicModel(body []byte, publicModel string) []byte {
	if len(body) == 0 || strings.TrimSpace(publicModel) == "" || !json.Valid(body) {
		return cloneBytes(body)
	}
	out := cloneBytes(body)
	for _, path := range []string{"model", "response.model", "message.model"} {
		if gjson.GetBytes(out, path).Exists() {
			if updated, err := sjson.SetBytes(out, path, publicModel); err == nil {
				out = updated
			}
		}
	}
	return out
}

func restoreSmartRouterStreamPublicModel(chunk []byte, publicModel string) []byte {
	if len(chunk) == 0 || strings.TrimSpace(publicModel) == "" {
		return cloneBytes(chunk)
	}
	if json.Valid(chunk) {
		return restoreSmartRouterPublicModel(chunk, publicModel)
	}

	lines := bytes.Split(chunk, []byte{'\n'})
	changed := false
	for index, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		if !json.Valid(payload) {
			continue
		}
		restored := restoreSmartRouterPublicModel(payload, publicModel)
		if bytes.Equal(restored, payload) {
			continue
		}
		prefixIndex := bytes.Index(line, []byte("data:"))
		lines[index] = append(append(cloneBytes(line[:prefixIndex]), "data: "...), restored...)
		changed = true
	}
	if !changed {
		return cloneBytes(chunk)
	}
	return bytes.Join(lines, []byte{'\n'})
}

var _ smartrouter.AttemptExecutor = (*SmartRouterAttemptExecutor)(nil)
var _ smartrouter.StreamAttemptExecutor = (*SmartRouterAttemptExecutor)(nil)
