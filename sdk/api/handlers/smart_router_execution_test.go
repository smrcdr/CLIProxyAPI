package handlers

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestSmartRouterPublicExecutionFailsOverThroughExistingHandler(t *testing.T) {
	const publicModel = "public-model"
	var primaryCalls atomic.Int32
	var secondaryCalls atomic.Int32

	primary := &modelExecutionCaptureExecutor{
		provider: SmartRouterProviderID("primary"),
		execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
			primaryCalls.Add(1)
			return coreexecutor.Response{}, modelExecutionStatusHeaderError{
				statusCode: http.StatusServiceUnavailable,
				message:    "secret upstream failure",
			}
		},
	}
	secondary := &modelExecutionCaptureExecutor{
		provider: SmartRouterProviderID("secondary"),
		execute: func(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
			secondaryCalls.Add(1)
			return coreexecutor.Response{
				Payload: []byte(`{"id":"resp_1","model":"` + req.Model + `"}`),
				Headers: http.Header{
					"X-SmartCLI-Account-Fingerprint": []string{"private-fingerprint"},
					"X-Public-Upstream":              []string{"secondary"},
				},
			}, nil
		},
	}
	handler := newSmartRouterHandler(t, publicModel, primary, secondary)
	assertSmartRouterRouteAvailable(t, handler, publicModel)

	body, headers, errMsg := handler.ExecuteWithAuthManager(
		logging.WithRequestID(context.Background(), "router-request-1"),
		"openai-response",
		publicModel,
		[]byte(`{"model":"public-model","input":"hello"}`),
		"",
	)
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %+v; calls primary=%d secondary=%d", errMsg, primaryCalls.Load(), secondaryCalls.Load())
	}
	if got := string(body); got != `{"id":"resp_1","model":"public-model"}` {
		t.Fatalf("body = %s", got)
	}
	if primaryCalls.Load() != 1 || secondaryCalls.Load() != 1 {
		t.Fatalf("calls = primary %d, secondary %d", primaryCalls.Load(), secondaryCalls.Load())
	}
	primaryRequest, _ := primary.captured()
	if primaryRequest.Model != publicModel {
		t.Fatalf("primary model = %q, want public model to exercise recursion bypass", primaryRequest.Model)
	}
	secondaryRequest, _ := secondary.captured()
	if secondaryRequest.Model != "secondary-upstream-model" {
		t.Fatalf("secondary model = %q", secondaryRequest.Model)
	}
	if headers.Get("X-SmartCLI-Account-Fingerprint") != "" {
		t.Fatalf("internal fingerprint leaked in headers: %#v", headers)
	}
	if headers.Get("X-Public-Upstream") != "secondary" {
		t.Fatalf("public upstream header = %q", headers.Get("X-Public-Upstream"))
	}
	for header, want := range map[string]string{
		"X-SmartRouter-Request-Id": "router-request-1",
		"X-SmartRouter-Upstream":   "secondary",
		"X-SmartRouter-Route":      "secondary-route",
		"X-SmartRouter-Attempts":   "2",
	} {
		if got := headers.Get(header); got != want {
			t.Fatalf("%s = %q, want %q; headers = %#v", header, got, want, headers)
		}
	}
	metrics := handler.smartRouter.metrics.Snapshot()
	if metrics.Totals.Requests != 1 || metrics.Totals.Attempts != 2 || metrics.Totals.Failovers != 1 {
		t.Fatalf("router metrics = %#v", metrics.Totals)
	}
}

func TestSmartRouterPublicStreamRestoresModelAndStripsInternalHeaders(t *testing.T) {
	const publicModel = "stream-public-model"
	streamExecutor := &modelExecutionCaptureExecutor{
		provider: SmartRouterProviderID("primary"),
		stream: func(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{
				Payload: []byte(`data: {"type":"response.completed","response":{"model":"` + req.Model + `"}}` + "\n\n"),
			}
			close(chunks)
			return &coreexecutor.StreamResult{
				Headers: http.Header{
					"X-SmartRouter-Diagnostic": []string{"private"},
					"X-Public-Upstream":        []string{"primary"},
				},
				Chunks: chunks,
			}, nil
		},
	}
	handler := newSmartRouterHandler(t, publicModel, streamExecutor)
	assertSmartRouterRouteAvailable(t, handler, publicModel)

	data, headers, errs := handler.ExecuteStreamWithAuthManager(
		context.Background(),
		"openai-response",
		publicModel,
		[]byte(`{"model":"stream-public-model","input":"hello","stream":true}`),
		"",
	)
	var payload []byte
	for chunk := range data {
		payload = append(payload, chunk...)
	}
	for errMsg := range errs {
		if errMsg != nil {
			t.Fatalf("stream error = %+v", errMsg)
		}
	}
	if got := string(payload); got != "data: {\"type\":\"response.completed\",\"response\":{\"model\":\"stream-public-model\"}}\n\n" {
		t.Fatalf("stream payload = %q", got)
	}
	if headers.Get("X-SmartRouter-Diagnostic") != "" {
		t.Fatalf("internal stream header leaked: %#v", headers)
	}
	if headers.Get("X-Public-Upstream") != "primary" {
		t.Fatalf("public stream header = %q", headers.Get("X-Public-Upstream"))
	}
	if headers.Get("X-SmartRouter-Request-Id") == "" ||
		headers.Get("X-SmartRouter-Upstream") != "primary" ||
		headers.Get("X-SmartRouter-Route") != "primary-route" {
		t.Fatalf("stream diagnostics = %#v", headers)
	}
}

func TestSmartRouterPublicCountUsesSelectedRouteWithoutInference(t *testing.T) {
	const publicModel = "count-public-model"
	var inferenceCalls atomic.Int32
	var countCalls atomic.Int32
	executor := &modelExecutionCaptureExecutor{
		provider: SmartRouterProviderID("primary"),
		execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
			inferenceCalls.Add(1)
			return coreexecutor.Response{}, nil
		},
		count: func(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
			countCalls.Add(1)
			return coreexecutor.Response{
				Payload: []byte(`{"input_tokens":17}`),
				Headers: http.Header{
					"X-SmartRouter-Diagnostic": []string{"private"},
					"X-Public-Upstream":        []string{"primary"},
				},
			}, nil
		},
	}
	handler := newSmartRouterHandler(t, publicModel, executor)
	assertSmartRouterRouteAvailable(t, handler, publicModel)

	body, headers, errMsg := handler.ExecuteCountWithAuthManager(
		context.Background(),
		"claude",
		publicModel,
		[]byte(`{"model":"count-public-model","messages":[{"role":"user","content":"hello"}]}`),
		"",
	)
	if errMsg != nil {
		t.Fatalf("ExecuteCountWithAuthManager() error = %+v", errMsg)
	}
	if got := string(body); got != `{"input_tokens":17}` {
		t.Fatalf("body = %s", got)
	}
	if inferenceCalls.Load() != 0 || countCalls.Load() != 1 {
		t.Fatalf("calls = inference %d count %d", inferenceCalls.Load(), countCalls.Load())
	}
	if headers.Get("X-SmartRouter-Diagnostic") != "" {
		t.Fatalf("internal count header leaked: %#v", headers)
	}
	if headers.Get("X-Public-Upstream") != "primary" {
		t.Fatalf("public count header = %q", headers.Get("X-Public-Upstream"))
	}
}

func TestSmartRouterRejectsCallerSuppliedInternalDiagnostics(t *testing.T) {
	const publicModel = "header-model"
	var calls atomic.Int32
	executor := &modelExecutionCaptureExecutor{
		provider: SmartRouterProviderID("primary"),
		execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
			calls.Add(1)
			return coreexecutor.Response{Payload: []byte(`{"model":"header-model"}`)}, nil
		},
	}
	handler := newSmartRouterHandler(t, publicModel, executor)
	ctx := contextWithHeaders(http.Header{
		"X-SmartRouter-Route": []string{"spoofed-route"},
	})
	_, _, errMsg := handler.ExecuteWithAuthManager(
		ctx,
		"openai-response",
		publicModel,
		[]byte(`{"model":"header-model","input":"hello"}`),
		"",
	)
	if errMsg == nil || errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("error = %#v, want 400", errMsg)
	}
	if calls.Load() != 0 {
		t.Fatalf("forbidden header reached upstream %d time(s)", calls.Load())
	}
	if errMsg.Addon.Get("X-SmartRouter-Request-Id") == "" {
		t.Fatalf("error diagnostics = %#v", errMsg.Addon)
	}
}

func TestSmartRouterPublicImageFailsOverOnlyForExplicitRejection(t *testing.T) {
	const publicModel = "image-public-model"
	var primaryCalls atomic.Int32
	var secondaryCalls atomic.Int32
	primary := &modelExecutionCaptureExecutor{
		provider: SmartRouterProviderID("primary"),
		execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
			primaryCalls.Add(1)
			return coreexecutor.Response{}, modelExecutionStatusHeaderError{
				statusCode: http.StatusTooManyRequests,
				message:    "quota",
				headers:    http.Header{"Retry-After": []string{"60"}},
			}
		},
	}
	secondary := &modelExecutionCaptureExecutor{
		provider: SmartRouterProviderID("secondary"),
		execute: func(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
			secondaryCalls.Add(1)
			return coreexecutor.Response{
				Payload: []byte(`{"created":1,"data":[{"b64_json":"aW1hZ2U="}]}`),
				Headers: http.Header{
					"X-SmartCLI-Account-Fingerprint": []string{"private"},
					"X-Public-Upstream":              []string{"secondary"},
				},
			}, nil
		},
	}
	handler := newSmartRouterImageHandler(t, publicModel, primary, secondary)
	request := []byte(`{"model":"image-public-model","prompt":"draw","n":1,"size":"1536x1024","quality":"high","output_format":"png","response_format":"b64_json"}`)

	body, headers, errMsg := handler.ExecuteImageWithAuthManager(context.Background(), "openai-image", publicModel, request, "")
	if errMsg != nil {
		t.Fatalf("ExecuteImageWithAuthManager() error = %+v", errMsg)
	}
	if string(body) != `{"created":1,"data":[{"b64_json":"aW1hZ2U="}]}` {
		t.Fatalf("body = %s", body)
	}
	if primaryCalls.Load() != 1 || secondaryCalls.Load() != 1 {
		t.Fatalf("calls = primary %d secondary %d", primaryCalls.Load(), secondaryCalls.Load())
	}
	secondaryRequest, secondaryOptions := secondary.captured()
	if secondaryRequest.Model != "secondary-upstream-model" || secondaryOptions.SourceFormat.String() != "openai-image" {
		t.Fatalf("secondary request = %#v options = %#v", secondaryRequest, secondaryOptions)
	}
	for _, path := range []string{"prompt", "n", "size", "quality", "output_format", "response_format"} {
		if !gjson.GetBytes(secondaryRequest.Payload, path).Exists() {
			t.Fatalf("image parameter %q was lost: %s", path, secondaryRequest.Payload)
		}
	}
	if headers.Get("X-SmartCLI-Account-Fingerprint") != "" || headers.Get("X-Public-Upstream") != "secondary" {
		t.Fatalf("headers = %#v", headers)
	}
}

func TestSmartRouterPublicImageDoesNotRetryAmbiguousFailure(t *testing.T) {
	const publicModel = "image-ambiguous-model"
	var backupCalls atomic.Int32
	primary := &modelExecutionCaptureExecutor{
		provider: SmartRouterProviderID("primary"),
		execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
			return coreexecutor.Response{}, modelExecutionStatusHeaderError{
				statusCode: http.StatusServiceUnavailable,
				message:    "ambiguous",
			}
		},
	}
	backup := &modelExecutionCaptureExecutor{
		provider: SmartRouterProviderID("secondary"),
		execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
			backupCalls.Add(1)
			return coreexecutor.Response{Payload: []byte(`{"data":[{"b64_json":"duplicate"}]}`)}, nil
		},
	}
	handler := newSmartRouterImageHandler(t, publicModel, primary, backup)

	_, _, errMsg := handler.ExecuteImageWithAuthManager(
		context.Background(),
		"openai-image",
		publicModel,
		[]byte(`{"model":"image-ambiguous-model","prompt":"draw","n":1}`),
		"",
	)
	if errMsg == nil || errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("error = %+v, want 503", errMsg)
	}
	if backupCalls.Load() != 0 {
		t.Fatalf("ambiguous failure reached backup %d time(s)", backupCalls.Load())
	}
}

func TestSmartRouterPublicImageMalformedSuccessReturnsBadGatewayWithoutRetry(t *testing.T) {
	const publicModel = "image-malformed-model"
	var backupCalls atomic.Int32
	primary := &modelExecutionCaptureExecutor{
		provider: SmartRouterProviderID("primary"),
		execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
			return coreexecutor.Response{Payload: []byte(`{"data":[]}`)}, nil
		},
	}
	backup := &modelExecutionCaptureExecutor{
		provider: SmartRouterProviderID("secondary"),
		execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
			backupCalls.Add(1)
			return coreexecutor.Response{Payload: []byte(`{"data":[{"b64_json":"duplicate"}]}`)}, nil
		},
	}
	handler := newSmartRouterImageHandler(t, publicModel, primary, backup)

	_, _, errMsg := handler.ExecuteImageWithAuthManager(
		context.Background(),
		"openai-image",
		publicModel,
		[]byte(`{"model":"image-malformed-model","prompt":"draw","n":1}`),
		"",
	)
	if errMsg == nil || errMsg.StatusCode != http.StatusBadGateway {
		t.Fatalf("error = %+v, want 502", errMsg)
	}
	if backupCalls.Load() != 0 {
		t.Fatalf("malformed success reached backup %d time(s)", backupCalls.Load())
	}
}

func TestSmartRouterImageValidation(t *testing.T) {
	valid := []byte(`{"data":[{"b64_json":"aW1hZ2U="}]}`)
	if err := validateSmartRouterImageRequest([]byte(`{"model":"image","prompt":"draw","n":1}`)); err != nil {
		t.Fatalf("valid request error = %v", err)
	}
	if err := validateSmartRouterImageResponse(valid); err != nil {
		t.Fatalf("valid response error = %v", err)
	}

	for name, body := range map[string][]byte{
		"stream":     []byte(`{"stream":true,"n":1}`),
		"multiple":   []byte(`{"n":2}`),
		"non-number": []byte(`{"n":"1"}`),
	} {
		if err := validateSmartRouterImageRequest(body); err == nil {
			t.Fatalf("%s request unexpectedly passed", name)
		}
	}
	for name, body := range map[string][]byte{
		"invalid":   []byte(`not-json`),
		"empty":     []byte(`{"data":[]}`),
		"multiple":  []byte(`{"data":[{"url":"one"},{"url":"two"}]}`),
		"no-output": []byte(`{"data":[{}]}`),
	} {
		if err := validateSmartRouterImageResponse(body); err == nil {
			t.Fatalf("%s response unexpectedly passed", name)
		}
	}
	oversized := []byte(`{"data":[{"b64_json":"` + strings.Repeat("a", maxSmartRouterImageResponseBytes) + `"}]}`)
	if err := validateSmartRouterImageResponse(oversized); err == nil {
		t.Fatal("oversized response unexpectedly passed")
	}
}

func newSmartRouterHandler(t *testing.T, publicModel string, executors ...*modelExecutionCaptureExecutor) *BaseAPIHandler {
	return newSmartRouterHandlerForCapability(t, publicModel, config.RouterCapabilityText, executors...)
}

func newSmartRouterImageHandler(t *testing.T, publicModel string, executors ...*modelExecutionCaptureExecutor) *BaseAPIHandler {
	return newSmartRouterHandlerForCapability(t, publicModel, config.RouterCapabilityImage, executors...)
}

func newSmartRouterHandlerForCapability(t *testing.T, publicModel string, capability config.RouterCapability, executors ...*modelExecutionCaptureExecutor) *BaseAPIHandler {
	t.Helper()
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	cfg := &config.Config{
		ServiceRole: config.ServiceRoleRouter,
		Router: config.RouterConfig{
			Upstreams: make([]config.RouterUpstream, 0, len(executors)),
			ModelGroups: []config.RouterModelGroup{
				{
					ID:          publicModel,
					PublicModel: publicModel,
					Capability:  capability,
				},
			},
		},
	}
	for index, executor := range executors {
		upstreamID := "primary"
		upstreamModel := publicModel
		if index > 0 {
			upstreamID = "secondary"
			upstreamModel = "secondary-upstream-model"
		}
		capabilities := config.RouterCapabilities{
			Endpoints: []string{config.RouterEndpointResponses},
			Streaming: true,
		}
		if capability == config.RouterCapabilityImage {
			capabilities.Endpoints = append(capabilities.Endpoints, config.RouterEndpointImages)
			capabilities.ImageGeneration = true
			capabilities.Streaming = false
		}
		cfg.Router.Upstreams = append(cfg.Router.Upstreams, config.RouterUpstream{
			ID:           upstreamID,
			Name:         upstreamID,
			Protocol:     config.RouterProtocolOpenAIResponses,
			BaseURL:      "https://" + upstreamID + ".example.com/v1",
			Capabilities: capabilities,
		})
		cfg.Router.ModelGroups[0].Routes = append(cfg.Router.ModelGroups[0].Routes, config.RouterRoute{
			ID:            upstreamID + "-route",
			UpstreamID:    upstreamID,
			UpstreamModel: upstreamModel,
			Priority:      100 - index,
			Weight:        100,
		})
		manager.RegisterExecutor(executor)
		manager.RegisterRuntime(&coreauth.Auth{
			ID:       "router-auth-" + upstreamID,
			Provider: executor.Identifier(),
			Status:   coreauth.StatusActive,
			Attributes: map[string]string{
				"smartrouter_managed": "true",
			},
		})
	}
	snapshot, err := smartrouter.CompileSnapshot(cfg, 1)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, manager)
	handler.SetSmartRouterSelector(smartrouter.NewSelector(smartrouter.NewSnapshotStore(snapshot), nil))
	return handler
}

func assertSmartRouterRouteAvailable(t *testing.T, handler *BaseAPIHandler, publicModel string) {
	t.Helper()
	plan, err := handler.smartRouter.selector.Begin(smartrouter.SelectionRequest{PublicModel: publicModel})
	if err != nil {
		t.Fatalf("selector.Begin() error = %v", err)
	}
	selection, err := plan.Next()
	if err != nil {
		t.Fatalf("plan.Next() error = %v", err)
	}
	if err := plan.Abandon(selection); err != nil {
		t.Fatalf("plan.Abandon() error = %v", err)
	}
}
