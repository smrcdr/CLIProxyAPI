package cliproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type routerRuntimeStore struct {
	mu      sync.Mutex
	saves   int
	deletes int
}

func (s *routerRuntimeStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

func (s *routerRuntimeStore) Save(context.Context, *coreauth.Auth) (string, error) {
	s.mu.Lock()
	s.saves++
	s.mu.Unlock()
	return "", nil
}

func (s *routerRuntimeStore) Delete(context.Context, string) error {
	s.mu.Lock()
	s.deletes++
	s.mu.Unlock()
	return nil
}

func (s *routerRuntimeStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves, s.deletes
}

type routerRuntimeSecretResolver struct {
	secrets map[string][]byte
	err     error
	refs    []string
}

func (r *routerRuntimeSecretResolver) ResolveUpstreamSecret(_ context.Context, ref string) ([]byte, error) {
	r.refs = append(r.refs, ref)
	if r.err != nil {
		return nil, r.err
	}
	secret := r.secrets[ref]
	return append([]byte(nil), secret...), nil
}

func TestRouterUpstreamRuntimeRegistersResolvedSecretInMemoryOnly(t *testing.T) {
	store := &routerRuntimeStore{}
	manager := coreauth.NewManager(store, &coreauth.RoundRobinSelector{}, nil)
	cfg := routerRuntimeConfig(config.RouterProtocolOpenAIResponses, config.RouterAuthBearer)
	resolver := &routerRuntimeSecretResolver{
		secrets: map[string][]byte{"router/primary": []byte("runtime-secret")},
	}
	runtime := newRouterUpstreamRuntime(manager, resolver)
	snapshot := compileRouterRuntimeSnapshot(t, cfg)

	if err := runtime.Reconcile(context.Background(), cfg, snapshot); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if len(resolver.refs) != 1 || resolver.refs[0] != "router/primary" {
		t.Fatalf("resolved refs = %v", resolver.refs)
	}
	auth, ok := manager.GetByID("smartrouter-upstream-primary")
	if !ok {
		t.Fatal("runtime auth was not registered")
	}
	if got := auth.Attributes[coreauth.AttributeAPIKey]; got != "runtime-secret" {
		t.Fatalf("runtime api key = %q", got)
	}
	if got := auth.Attributes["smartrouter_managed"]; got != "true" {
		t.Fatalf("smartrouter_managed = %q", got)
	}
	provider := handlers.SmartRouterProviderID("primary")
	executor, ok := manager.Executor(provider)
	if !ok {
		t.Fatal("runtime executor was not registered")
	}
	formatted, ok := executor.(interface {
		RequestToFormat(cliproxyexecutor.Request, cliproxyexecutor.Options) sdktranslator.Format
	})
	if !ok {
		t.Fatalf("executor %T does not expose its request format", executor)
	}
	if got := formatted.RequestToFormat(cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); got != sdktranslator.FormatOpenAIResponse {
		t.Fatalf("executor request format = %v", got)
	}
	if saves, deletes := store.counts(); saves != 0 || deletes != 0 {
		t.Fatalf("persistent store calls = saves %d, deletes %d", saves, deletes)
	}
}

func TestRouterUpstreamRuntimeKeepsPreviousEntriesWhenResolutionFails(t *testing.T) {
	manager := coreauth.NewManager(&routerRuntimeStore{}, &coreauth.RoundRobinSelector{}, nil)
	cfg := routerRuntimeConfig(config.RouterProtocolOpenAIResponses, config.RouterAuthBearer)
	resolver := &routerRuntimeSecretResolver{
		secrets: map[string][]byte{"router/primary": []byte("old-secret")},
	}
	runtime := newRouterUpstreamRuntime(manager, resolver)
	if err := runtime.Reconcile(context.Background(), cfg, compileRouterRuntimeSnapshot(t, cfg)); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}

	cfg.Router.Upstreams[0].Auth.SecretRef = "router/replacement"
	resolver.err = errors.New("backend included old-secret in its error")
	err := runtime.Reconcile(context.Background(), cfg, compileRouterRuntimeSnapshot(t, cfg))
	if err == nil {
		t.Fatal("replacement Reconcile() error = nil")
	}
	if strings.Contains(err.Error(), "old-secret") {
		t.Fatalf("resolution error leaked resolver details: %v", err)
	}
	auth, ok := manager.GetByID("smartrouter-upstream-primary")
	if !ok {
		t.Fatal("previous runtime auth was removed")
	}
	if got := auth.Attributes[coreauth.AttributeAPIKey]; got != "old-secret" {
		t.Fatalf("previous runtime api key = %q", got)
	}
}

func TestRouterUpstreamRuntimeRemovesDisabledEntries(t *testing.T) {
	manager := coreauth.NewManager(&routerRuntimeStore{}, &coreauth.RoundRobinSelector{}, nil)
	cfg := routerRuntimeConfig(config.RouterProtocolAnthropicMessages, config.RouterAuthNone)
	runtime := newRouterUpstreamRuntime(manager, nil)
	if err := runtime.Reconcile(context.Background(), cfg, compileRouterRuntimeSnapshot(t, cfg)); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	provider := handlers.SmartRouterProviderID("primary")
	if _, ok := manager.Executor(provider); !ok {
		t.Fatal("runtime executor was not registered")
	}

	disabled := false
	cfg.Router.Upstreams[0].Enabled = &disabled
	if err := runtime.Reconcile(context.Background(), cfg, compileRouterRuntimeSnapshot(t, cfg)); err != nil {
		t.Fatalf("disabled Reconcile() error = %v", err)
	}
	if _, ok := manager.GetByID("smartrouter-upstream-primary"); ok {
		t.Fatal("disabled runtime auth is still registered")
	}
	if _, ok := manager.Executor(provider); ok {
		t.Fatal("disabled runtime executor is still registered")
	}
}

func TestRouterRuntimeExecutesChatThroughResponsesUpstream(t *testing.T) {
	var receivedMu sync.Mutex
	var received []byte
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		receivedMu.Lock()
		upstreamCalls++
		receivedMu.Unlock()
		if request.URL.Path != "/responses" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		receivedMu.Lock()
		received = append([]byte(nil), body...)
		receivedMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_1",
			"object":"response",
			"status":"completed",
			"model":"upstream-model",
			"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],
			"usage":{
				"input_tokens":12,
				"output_tokens":5,
				"total_tokens":17,
				"input_tokens_details":{"cached_tokens":3},
				"output_tokens_details":{"reasoning_tokens":2}
			}
		}`))
	}))
	defer upstream.Close()

	cfg := routerRuntimeConfig(config.RouterProtocolOpenAIResponses, config.RouterAuthNone)
	cfg.Router.NetworkPolicy = localRouterTestNetworkPolicy()
	cfg.Router.Upstreams[0].BaseURL = upstream.URL
	cfg.Router.ModelGroups[0].Routes[0].UpstreamModel = "upstream-model"
	snapshot := compileRouterRuntimeSnapshot(t, cfg)
	manager := coreauth.NewManager(&routerRuntimeStore{}, &coreauth.RoundRobinSelector{}, nil)
	runtime := newRouterUpstreamRuntime(manager, nil)
	if err := runtime.Reconcile(context.Background(), cfg, snapshot); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	handler := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, manager)
	handler.SetSmartRouterSelector(smartrouter.NewSelector(smartrouter.NewSnapshotStore(snapshot), nil))

	body, _, errMsg := handler.ExecuteWithAuthManager(
		context.Background(),
		"openai",
		"public-model",
		[]byte(`{
			"model":"public-model",
			"messages":[{"role":"developer","content":"Follow policy"},{"role":"user","content":"hello"}],
			"max_completion_tokens":321,
			"service_tier":"priority"
		}`),
		"",
	)
	if errMsg != nil {
		receivedMu.Lock()
		calls := upstreamCalls
		upstreamBody := append([]byte(nil), received...)
		receivedMu.Unlock()
		t.Fatalf("ExecuteWithAuthManager() error = %+v; upstream calls=%d body=%s", errMsg, calls, upstreamBody)
	}

	receivedMu.Lock()
	upstreamBody := append([]byte(nil), received...)
	receivedMu.Unlock()
	if got := gjson.GetBytes(upstreamBody, "model").String(); got != "upstream-model" {
		t.Fatalf("upstream model = %q; body = %s", got, upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "instructions").String(); got != "Follow policy" {
		t.Fatalf("upstream instructions = %q; body = %s", got, upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "max_output_tokens").Int(); got != 321 {
		t.Fatalf("upstream max output tokens = %d; body = %s", got, upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "service_tier").String(); got != "priority" {
		t.Fatalf("upstream service tier = %q; body = %s", got, upstreamBody)
	}

	if got := gjson.GetBytes(body, "model").String(); got != "public-model" {
		t.Fatalf("public response model = %q; body = %s", got, body)
	}
	if got := gjson.GetBytes(body, "choices.0.message.content").String(); got != "hello" {
		t.Fatalf("public response content = %q; body = %s", got, body)
	}
	if got := gjson.GetBytes(body, "usage.prompt_tokens").Int(); got != 12 {
		t.Fatalf("public prompt tokens = %d; body = %s", got, body)
	}
	if got := gjson.GetBytes(body, "usage.completion_tokens").Int(); got != 5 {
		t.Fatalf("public completion tokens = %d; body = %s", got, body)
	}
	if got := gjson.GetBytes(body, "usage.prompt_tokens_details.cached_tokens").Int(); got != 3 {
		t.Fatalf("public cached tokens = %d; body = %s", got, body)
	}
	if got := gjson.GetBytes(body, "usage.completion_tokens_details.reasoning_tokens").Int(); got != 2 {
		t.Fatalf("public reasoning tokens = %d; body = %s", got, body)
	}
}

func TestRouterRuntimeExecutesImageGenerationThroughConfiguredUpstream(t *testing.T) {
	var receivedMu sync.Mutex
	var received []byte
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		receivedMu.Lock()
		upstreamCalls++
		receivedMu.Unlock()
		if request.URL.Path != "/images/generations" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		receivedMu.Lock()
		received = append([]byte(nil), body...)
		receivedMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-SmartCLI-Account-Fingerprint", "private")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aW1hZ2U="}]}`))
	}))
	defer upstream.Close()

	cfg := routerRuntimeConfig(config.RouterProtocolOpenAIResponses, config.RouterAuthNone)
	cfg.Router.NetworkPolicy = localRouterTestNetworkPolicy()
	cfg.Router.Upstreams[0].BaseURL = upstream.URL
	cfg.Router.Upstreams[0].Capabilities.Endpoints = append(
		cfg.Router.Upstreams[0].Capabilities.Endpoints,
		config.RouterEndpointImages,
	)
	cfg.Router.Upstreams[0].Capabilities.ImageGeneration = true
	cfg.Router.ModelGroups[0].Capability = config.RouterCapabilityImage
	cfg.Router.ModelGroups[0].Routes[0].UpstreamModel = "upstream-image-model"
	snapshot := compileRouterRuntimeSnapshot(t, cfg)
	manager := coreauth.NewManager(&routerRuntimeStore{}, &coreauth.RoundRobinSelector{}, nil)
	runtime := newRouterUpstreamRuntime(manager, nil)
	if err := runtime.Reconcile(context.Background(), cfg, snapshot); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	handler := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, manager)
	handler.SetSmartRouterSelector(smartrouter.NewSelector(smartrouter.NewSnapshotStore(snapshot), nil))

	requestBody := []byte(`{
		"model":"public-model",
		"prompt":"draw a lighthouse",
		"n":1,
		"size":"1536x1024",
		"quality":"high",
		"output_format":"png",
		"response_format":"b64_json"
	}`)
	gin.SetMode(gin.TestMode)
	api := openaihandlers.NewOpenAIAPIHandler(handler)
	router := gin.New()
	router.POST("/v1/images/generations", api.ImagesGenerations)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(requestBody)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("POST /v1/images/generations status = %d, body = %s", response.Code, response.Body.String())
	}

	receivedMu.Lock()
	calls := upstreamCalls
	upstreamBody := append([]byte(nil), received...)
	receivedMu.Unlock()
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if got := gjson.GetBytes(upstreamBody, "model").String(); got != "upstream-image-model" {
		t.Fatalf("upstream model = %q; body = %s", got, upstreamBody)
	}
	for _, path := range []string{"prompt", "n", "size", "quality", "output_format", "response_format"} {
		if got, want := gjson.GetBytes(upstreamBody, path).Raw, gjson.GetBytes(requestBody, path).Raw; got != want {
			t.Fatalf("upstream %s = %s, want %s; body = %s", path, got, want, upstreamBody)
		}
	}
	if response.Body.String() != `{"created":1,"data":[{"b64_json":"aW1hZ2U="}]}` {
		t.Fatalf("public body = %s", response.Body.String())
	}
	if response.Header().Get("X-SmartCLI-Account-Fingerprint") != "" {
		t.Fatalf("internal fingerprint leaked: %#v", response.Header())
	}
}

func routerRuntimeConfig(protocol config.RouterProtocol, authType config.RouterAuthType) *config.Config {
	cfg := serviceRouterTestConfig("public-model")
	cfg.Router.Upstreams[0].Protocol = protocol
	cfg.Router.Upstreams[0].Auth = config.RouterUpstreamAuth{Type: authType}
	switch authType {
	case config.RouterAuthBearer, config.RouterAuthBasic:
		cfg.Router.Upstreams[0].Auth.SecretRef = "router/primary"
	case config.RouterAuthAPIKeyHeader:
		cfg.Router.Upstreams[0].Auth.SecretRef = "router/primary"
		cfg.Router.Upstreams[0].Auth.Header = "X-API-Key"
	}
	switch protocol {
	case config.RouterProtocolOpenAIChatCompletions:
		cfg.Router.Upstreams[0].Capabilities.Endpoints = []string{config.RouterEndpointChatCompletions}
	case config.RouterProtocolOpenAIResponses:
		cfg.Router.Upstreams[0].Capabilities.Endpoints = []string{config.RouterEndpointResponses}
	case config.RouterProtocolAnthropicMessages:
		cfg.Router.Upstreams[0].Capabilities.Endpoints = []string{config.RouterEndpointMessages}
	}
	return cfg
}

func compileRouterRuntimeSnapshot(t *testing.T, cfg *config.Config) *smartrouter.Snapshot {
	t.Helper()
	snapshot, err := smartrouter.CompileSnapshot(cfg, 1)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	return snapshot
}
