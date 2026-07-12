package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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
