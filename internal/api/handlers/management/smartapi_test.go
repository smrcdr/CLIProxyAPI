package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartapiusage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

func TestSmartAPIManagementAuthMaskRevealAndClone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	template := config.ClaudeKey{
		APIKey:                  "sk-opencode-template-1234",
		Priority:                8,
		Prefix:                  "go",
		BaseURL:                 openCodeGoBaseURL,
		ProxyURL:                "http://proxy.internal",
		Models:                  []config.ClaudeModel{{Name: "claude-opus-4-1", Alias: "opus-4.8", ForceMapping: true}},
		Headers:                 map[string]string{"X-Routing": "smart"},
		ExcludedModels:          []string{"old-model"},
		RebuildMidSystemMessage: true,
		DisableCooling:          true,
		Cloak:                   &config.CloakConfig{Mode: "never", SensitiveWords: []string{"internal"}},
		ExperimentalCCHSigning:  true,
	}
	secretHash, err := bcrypt.GenerateFromPassword([]byte("management-test-key"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{SmartManagementEnabled: true, RemoteManagement: config.RemoteManagement{AllowRemote: true, SecretKey: string(secretHash)}, ClaudeKey: []config.ClaudeKey{template}}
	path := filepath.Join(t.TempDir(), "config.yaml")
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(cfg, path, nil)
	router := gin.New()
	group := router.Group("/v0/management")
	group.Use(handler.Middleware())
	group.GET("/smartapi/keys", handler.GetSmartAPIKeys)
	group.GET("/smartapi/keys/:id/reveal", handler.RevealSmartAPIKey)
	group.POST("/smartapi/keys", handler.PostSmartAPIKey)
	group.GET("/smartapi/settings", handler.GetSmartAPISettings)
	group.PUT("/smartapi/settings", handler.PutSmartAPISettings)

	unauthorized := performSmartAPIRequest(router, http.MethodGet, "/v0/management/smartapi/keys", nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}

	list := performSmartAPIRequest(router, http.MethodGet, "/v0/management/smartapi/keys", nil, "management-test-key")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), template.APIKey) {
		t.Fatalf("masked list leaked full key: %s", list.Body.String())
	}
	id := smartAPIKeyID(template.APIKey)
	reveal := performSmartAPIRequest(router, http.MethodGet, "/v0/management/smartapi/keys/"+id+"/reveal", nil, "management-test-key")
	if reveal.Code != http.StatusOK || !strings.Contains(reveal.Body.String(), template.APIKey) {
		t.Fatalf("reveal response = %d %s", reveal.Code, reveal.Body.String())
	}

	newKey := "sk-opencode-new-key-5678"
	added := performSmartAPIRequest(router, http.MethodPost, "/v0/management/smartapi/keys", map[string]any{"api_key": newKey}, "management-test-key")
	if added.Code != http.StatusCreated {
		t.Fatalf("add status = %d: %s", added.Code, added.Body.String())
	}
	if len(cfg.ClaudeKey) != 2 {
		t.Fatalf("key count = %d, want 2", len(cfg.ClaudeKey))
	}
	want := cloneClaudeKey(template)
	want.APIKey = newKey
	if !reflect.DeepEqual(cfg.ClaudeKey[1], want) {
		t.Fatalf("cloned credential differs:\ngot  %#v\nwant %#v", cfg.ClaudeKey[1], want)
	}
	duplicate := performSmartAPIRequest(router, http.MethodPost, "/v0/management/smartapi/keys", map[string]any{"api_key": newKey}, "management-test-key")
	if duplicate.Code != http.StatusConflict || len(cfg.ClaudeKey) != 2 {
		t.Fatalf("duplicate response = %d, keys = %d", duplicate.Code, len(cfg.ClaudeKey))
	}

	toggle := performSmartAPIRequest(router, http.MethodPut, "/v0/management/smartapi/settings", map[string]any{"strip_reasoning": true}, "management-test-key")
	if toggle.Code != http.StatusOK || !cfg.StripReasoning {
		t.Fatalf("toggle response = %d %s, config = %v", toggle.Code, toggle.Body.String(), cfg.StripReasoning)
	}
}

func TestSmartAPIAddRequiresCompleteTemplate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{SmartManagementEnabled: true, ClaudeKey: []config.ClaudeKey{{APIKey: "sk-other-provider-1234", BaseURL: "https://example.com", Models: []config.ClaudeModel{{Name: "x", Alias: "x"}}}}}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("smart-management-enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(cfg, path, nil)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/v0/management/smartapi/keys", bytes.NewBufferString(`{"api_key":"sk-new-missing-template"}`))
	context.Request.Header.Set("Content-Type", "application/json")
	handler.PostSmartAPIKey(context)
	if recorder.Code != http.StatusConflict || len(cfg.ClaudeKey) != 1 {
		t.Fatalf("response = %d, keys = %d", recorder.Code, len(cfg.ClaudeKey))
	}
}

func TestSmartAPIManagementOpenAICompatibilityKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const existingKey = "sk-opencode-openai-compat-1234"
	provider := config.OpenAICompatibility{
		Name:    "opencode-go",
		BaseURL: "https://opencode.ai/zen/go/v1",
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
			APIKey:   existingKey,
			ProxyURL: "http://proxy.internal",
		}},
		Models:  []config.OpenAICompatibilityModel{{Name: "qwen3.7-plus", Alias: "opus-4.8", ForceMapping: true}},
		Headers: map[string]string{"X-Routing": "smart"},
	}
	secretHash, err := bcrypt.GenerateFromPassword([]byte("management-test-key"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		SmartManagementEnabled: true,
		RemoteManagement:       config.RemoteManagement{AllowRemote: true, SecretKey: string(secretHash)},
		OpenAICompatibility:    []config.OpenAICompatibility{provider},
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	_, err = manager.Register(context.Background(), &coreauth.Auth{
		ID:       "opencode-go-runtime",
		Provider: "openai-compatible-opencode-go",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAPIKey: existingKey,
		},
		Success: 9,
		Failed:  2,
	})
	if err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(cfg, path, manager)
	router := gin.New()
	group := router.Group("/v0/management")
	group.Use(handler.Middleware())
	group.GET("/smartapi/keys", handler.GetSmartAPIKeys)
	group.GET("/smartapi/keys/:id/reveal", handler.RevealSmartAPIKey)
	group.POST("/smartapi/keys", handler.PostSmartAPIKey)

	list := performSmartAPIRequest(router, http.MethodGet, "/v0/management/smartapi/keys", nil, "management-test-key")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", list.Code, list.Body.String())
	}
	var listed struct {
		Keys []smartAPIKeyView `json:"keys"`
	}
	if err = json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Keys) != 1 {
		t.Fatalf("listed keys = %d, want 1: %s", len(listed.Keys), list.Body.String())
	}
	if !listed.Keys[0].Available || listed.Keys[0].Status != string(coreauth.StatusActive) || listed.Keys[0].Success != 9 || listed.Keys[0].Failed != 2 {
		t.Fatalf("runtime key view = %#v", listed.Keys[0])
	}
	if strings.Contains(list.Body.String(), existingKey) {
		t.Fatalf("masked list leaked full key: %s", list.Body.String())
	}

	id := smartAPIKeyID(existingKey)
	reveal := performSmartAPIRequest(router, http.MethodGet, "/v0/management/smartapi/keys/"+id+"/reveal", nil, "management-test-key")
	if reveal.Code != http.StatusOK || !strings.Contains(reveal.Body.String(), existingKey) {
		t.Fatalf("reveal response = %d %s", reveal.Code, reveal.Body.String())
	}

	const newKey = "sk-opencode-openai-compat-5678"
	added := performSmartAPIRequest(router, http.MethodPost, "/v0/management/smartapi/keys", map[string]any{"api_key": newKey}, "management-test-key")
	if added.Code != http.StatusCreated {
		t.Fatalf("add status = %d: %s", added.Code, added.Body.String())
	}
	if len(cfg.OpenAICompatibility) != 1 || len(cfg.OpenAICompatibility[0].APIKeyEntries) != 2 {
		t.Fatalf("OpenAI-compatible provider after add = %#v", cfg.OpenAICompatibility)
	}
	wantEntry := provider.APIKeyEntries[0]
	wantEntry.APIKey = newKey
	if got := cfg.OpenAICompatibility[0].APIKeyEntries[1]; !reflect.DeepEqual(got, wantEntry) {
		t.Fatalf("cloned API key entry = %#v, want %#v", got, wantEntry)
	}
	if !reflect.DeepEqual(cfg.OpenAICompatibility[0].Models, provider.Models) || !reflect.DeepEqual(cfg.OpenAICompatibility[0].Headers, provider.Headers) {
		t.Fatal("adding an API key changed provider models or headers")
	}

	duplicate := performSmartAPIRequest(router, http.MethodPost, "/v0/management/smartapi/keys", map[string]any{"api_key": newKey}, "management-test-key")
	if duplicate.Code != http.StatusConflict || len(cfg.OpenAICompatibility[0].APIKeyEntries) != 2 {
		t.Fatalf("duplicate response = %d, entries = %d", duplicate.Code, len(cfg.OpenAICompatibility[0].APIKeyEntries))
	}
}

func TestSmartAPIAnalyticsRollingWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().UTC()
	store := smartapiusage.NewStore()
	store.HandleUsage(context.Background(), coreusage.Record{
		Provider:    "openai-compatible-opencode-go",
		RequestedAt: now.Add(-10 * time.Minute),
		Detail: coreusage.Detail{
			InputTokens:         1000,
			OutputTokens:        80,
			CacheReadTokens:     750,
			CacheCreationTokens: 40,
		},
	})
	store.HandleUsage(context.Background(), coreusage.Record{
		Provider:    "openai-compatible-opencode-go",
		RequestedAt: now.Add(-2 * time.Hour),
		Failed:      true,
		Detail: coreusage.Detail{
			InputTokens:     200,
			OutputTokens:    10,
			CacheReadTokens: 50,
		},
	})

	handler := NewHandler(&config.Config{SmartManagementEnabled: true}, "", nil)
	handler.smartAPIUsage = store
	router := gin.New()
	router.GET("/v0/management/smartapi/analytics", handler.GetSmartAPIAnalytics)

	response := performSmartAPIRequest(router, http.MethodGet, "/v0/management/smartapi/analytics?window=30m", nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("analytics status = %d: %s", response.Code, response.Body.String())
	}
	var analytics struct {
		Window           string  `json:"window"`
		Requests         int64   `json:"requests"`
		Failed           int64   `json:"failed"`
		InputTokens      int64   `json:"input_tokens"`
		OutputTokens     int64   `json:"output_tokens"`
		CacheReadTokens  int64   `json:"cache_read_tokens"`
		CacheWriteTokens int64   `json:"cache_write_tokens"`
		UncachedInput    int64   `json:"uncached_input_tokens"`
		TotalTokens      int64   `json:"total_tokens"`
		CacheHitPercent  float64 `json:"cache_hit_percent"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &analytics); err != nil {
		t.Fatal(err)
	}
	if analytics.Window != "30m" || analytics.Requests != 1 || analytics.Failed != 0 {
		t.Fatalf("analytics request totals = %#v", analytics)
	}
	if analytics.InputTokens != 1000 || analytics.OutputTokens != 80 || analytics.CacheReadTokens != 750 || analytics.CacheWriteTokens != 40 {
		t.Fatalf("analytics token totals = %#v", analytics)
	}
	if analytics.UncachedInput != 250 || analytics.TotalTokens != 1080 || analytics.CacheHitPercent != 75 {
		t.Fatalf("analytics derived totals = %#v", analytics)
	}

	invalid := performSmartAPIRequest(router, http.MethodGet, "/v0/management/smartapi/analytics?window=7d", nil, "")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid window status = %d, want 400", invalid.Code)
	}
}

func performSmartAPIRequest(router http.Handler, method, path string, body any, key string) *httptest.ResponseRecorder {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}
