package management

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
)

func TestRouterManagementCRUDAuthRevisionAndSecretRedaction(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "router-management-test-key")
	directory := t.TempDir()
	metadata, err := smartrouter.NewFileRouterMetadataStore(filepath.Join(directory, "metadata.json"))
	if err != nil {
		t.Fatalf("NewFileRouterMetadataStore() error = %v", err)
	}
	masterKey := bytes.Repeat([]byte{0x71}, 32)
	secrets, err := smartrouter.NewEncryptedFileRouterSecretStore(filepath.Join(directory, "secrets.json"), masterKey)
	if err != nil {
		t.Fatalf("NewEncryptedFileRouterSecretStore() error = %v", err)
	}
	audit, err := smartrouter.NewFileRouterAuditSink(filepath.Join(directory, "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewFileRouterAuditSink() error = %v", err)
	}
	service, err := smartrouter.NewRouterManagementService(metadata, secrets, audit, nil)
	if err != nil {
		t.Fatalf("NewRouterManagementService() error = %v", err)
	}
	selector := smartrouter.NewSelector(smartrouter.NewSnapshotStore(smartrouter.EmptySnapshot()), nil)
	handler := NewHandler(&config.Config{
		RemoteManagement: config.RemoteManagement{AllowRemote: true},
	}, "", nil)
	handler.SetRouterManagementService(service, selector)
	handler.SetRouterProber(routerManagementProber{result: smartrouter.RouterProbeResult{
		UpstreamID: "primary",
		Healthy:    true,
		StatusCode: http.StatusNoContent,
		CheckedAt:  time.Unix(100, 0),
		Latency:    25 * time.Millisecond,
	}})
	router := routerManagementTestRouter(handler)

	unauthorized := performRouterManagementRequest(router, http.MethodGet, "/v0/management/router/upstreams", nil, "", "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, body = %s", unauthorized.Code, unauthorized.Body.String())
	}
	schema := performRouterManagementRequest(router, http.MethodGet, "/v0/management/router/schemas/upstream-types", nil, "router-management-test-key", "")
	if schema.Code != http.StatusOK || !strings.Contains(schema.Body.String(), `"write_only":true`) {
		t.Fatalf("upstream schema = %d %s", schema.Code, schema.Body.String())
	}
	assertRouterResponseRedacted(t, schema)

	const secretFixture = "router-secret-fixture"
	createUpstream := performRouterManagementRequest(router, http.MethodPost, "/v0/management/router/upstreams", map[string]any{
		"id":       "primary",
		"name":     "Primary",
		"protocol": "openai-responses",
		"base_url": "https://primary.example.com/v1",
		"auth": map[string]any{
			"type":   "bearer",
			"secret": secretFixture,
		},
		"capabilities": map[string]any{
			"endpoints": []string{"responses"},
			"streaming": true,
		},
	}, "router-management-test-key", `"0"`)
	if createUpstream.Code != http.StatusCreated {
		t.Fatalf("create upstream status = %d, body = %s", createUpstream.Code, createUpstream.Body.String())
	}
	assertRouterResponseRedacted(t, createUpstream, secretFixture)
	if createUpstream.Header().Get("ETag") != `"1"` {
		t.Fatalf("create ETag = %q", createUpstream.Header().Get("ETag"))
	}

	list := performRouterManagementRequest(router, http.MethodGet, "/v0/management/router/upstreams", nil, "router-management-test-key", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", list.Code, list.Body.String())
	}
	assertRouterResponseRedacted(t, list, secretFixture)
	if !strings.Contains(list.Body.String(), `"configured":true`) {
		t.Fatalf("list did not report configured secret: %s", list.Body.String())
	}
	probe := performRouterManagementRequest(router, http.MethodPost, "/v0/management/router/upstreams/primary/test", nil, "router-management-test-key", "")
	if probe.Code != http.StatusOK || !strings.Contains(probe.Body.String(), `"healthy":true`) || !strings.Contains(probe.Body.String(), `"latency_ms":25`) {
		t.Fatalf("upstream probe = %d %s", probe.Code, probe.Body.String())
	}
	assertRouterResponseRedacted(t, probe, secretFixture)

	conflict := performRouterManagementRequest(router, http.MethodPatch, "/v0/management/router/upstreams/primary", map[string]any{
		"name": "Stale Update",
	}, "router-management-test-key", `"0"`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, body = %s", conflict.Code, conflict.Body.String())
	}
	assertRouterResponseRedacted(t, conflict, secretFixture)

	missingPrecondition := performRouterManagementRequest(router, http.MethodPatch, "/v0/management/router/upstreams/primary", map[string]any{
		"name": "No Revision",
	}, "router-management-test-key", "")
	if missingPrecondition.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match status = %d, body = %s", missingPrecondition.Code, missingPrecondition.Body.String())
	}

	createGroup := performRouterManagementRequest(router, http.MethodPost, "/v0/management/router/model-groups", map[string]any{
		"id":           "public-model",
		"public_model": "public-model",
		"capability":   "text",
	}, "router-management-test-key", `"1"`)
	if createGroup.Code != http.StatusCreated {
		t.Fatalf("create group status = %d, body = %s", createGroup.Code, createGroup.Body.String())
	}

	createRoute := performRouterManagementRequest(router, http.MethodPost, "/v0/management/router/model-groups/public-model/routes", map[string]any{
		"id":             "public-model-primary",
		"upstream_id":    "primary",
		"upstream_model": "upstream-model",
		"priority":       100,
		"weight":         100,
	}, "router-management-test-key", `"2"`)
	if createRoute.Code != http.StatusCreated {
		t.Fatalf("create route status = %d, body = %s", createRoute.Code, createRoute.Body.String())
	}
	modelGroup := performRouterManagementRequest(router, http.MethodGet, "/v0/management/router/model-groups/public-model", nil, "router-management-test-key", "")
	if modelGroup.Code != http.StatusOK {
		t.Fatalf("get model group status = %d, body = %s", modelGroup.Code, modelGroup.Body.String())
	}
	for _, field := range []string{`"public_model"`, `"upstream_id"`, `"upstream_model"`, `"affinity_ttl"`, `"enabled":true`} {
		if !strings.Contains(modelGroup.Body.String(), field) {
			t.Fatalf("model group response is missing %s: %s", field, modelGroup.Body.String())
		}
	}
	for _, legacyField := range []string{`"public-model":`, `"upstream-id":`, `"upstream-model":`, `"affinity-ttl":`} {
		if strings.Contains(modelGroup.Body.String(), legacyField) {
			t.Fatalf("model group response exposed internal field %s: %s", legacyField, modelGroup.Body.String())
		}
	}

	referencedDelete := performRouterManagementRequest(router, http.MethodDelete, "/v0/management/router/upstreams/primary", nil, "router-management-test-key", `"3"`)
	if referencedDelete.Code != http.StatusConflict {
		t.Fatalf("referenced delete status = %d, body = %s", referencedDelete.Code, referencedDelete.Body.String())
	}

	state := performRouterManagementRequest(router, http.MethodGet, "/v0/management/router/routes/public-model-primary/state", nil, "router-management-test-key", "")
	if state.Code != http.StatusOK || !strings.Contains(state.Body.String(), `"state":"closed"`) {
		t.Fatalf("route state = %d %s", state.Code, state.Body.String())
	}
	reset := performRouterManagementRequest(router, http.MethodPost, "/v0/management/router/routes/public-model-primary/circuit/reset", nil, "router-management-test-key", "")
	if reset.Code != http.StatusOK {
		t.Fatalf("circuit reset = %d %s", reset.Code, reset.Body.String())
	}

	resolved, err := secrets.ResolveUpstreamSecret(context.Background(), "router/upstreams/primary")
	if err != nil {
		t.Fatalf("ResolveUpstreamSecret() error = %v", err)
	}
	defer clear(resolved)
	if string(resolved) != secretFixture {
		t.Fatalf("resolved secret = %q", resolved)
	}
	secretFile, err := os.ReadFile(filepath.Join(directory, "secrets.json"))
	if err != nil {
		t.Fatalf("ReadFile(secrets) error = %v", err)
	}
	if bytes.Contains(secretFile, []byte(secretFixture)) {
		t.Fatalf("plaintext secret leaked to encrypted file: %s", secretFile)
	}
}

func routerManagementTestRouter(handler *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v0/management")
	group.Use(handler.Middleware())
	routerGroup := group.Group("/router")
	routerGroup.GET("/schemas/upstream-types", handler.GetRouterUpstreamTypes)
	routerGroup.GET("/schemas/network-policy", handler.GetRouterNetworkPolicySchema)
	routerGroup.GET("/network-policy", handler.GetRouterNetworkPolicy)
	routerGroup.PATCH("/network-policy", handler.PatchRouterNetworkPolicy)
	routerGroup.GET("/upstreams", handler.ListRouterUpstreams)
	routerGroup.POST("/upstreams", handler.CreateRouterUpstream)
	routerGroup.PATCH("/upstreams/:id", handler.PatchRouterUpstream)
	routerGroup.DELETE("/upstreams/:id", handler.DeleteRouterUpstream)
	routerGroup.POST("/upstreams/:id/test", handler.TestRouterUpstream)
	routerGroup.POST("/model-groups", handler.CreateRouterModelGroup)
	routerGroup.GET("/model-groups/:id", handler.GetRouterModelGroup)
	routerGroup.POST("/model-groups/:id/routes", handler.CreateRouterRoute)
	routerGroup.GET("/metrics", handler.GetRouterMetrics)
	routerGroup.GET("/routes/:route_id/state", handler.GetRouterRouteState)
	routerGroup.POST("/routes/:route_id/circuit/reset", handler.ResetRouterRouteCircuit)
	return router
}

func TestRouterManagementNetworkPolicyRevisionAndValidation(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "router-management-test-key")
	metadata, err := smartrouter.NewFileRouterMetadataStore(filepath.Join(t.TempDir(), "metadata.json"))
	if err != nil {
		t.Fatalf("NewFileRouterMetadataStore() error = %v", err)
	}
	service, err := smartrouter.NewRouterManagementService(metadata, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewRouterManagementService() error = %v", err)
	}
	handler := NewHandler(&config.Config{
		RemoteManagement: config.RemoteManagement{AllowRemote: true},
	}, "", nil)
	handler.SetRouterManagementService(service, smartrouter.NewSelector(smartrouter.NewSnapshotStore(nil), nil))
	router := routerManagementTestRouter(handler)

	schema := performRouterManagementRequest(router, http.MethodGet, "/v0/management/router/schemas/network-policy", nil, "router-management-test-key", "")
	if schema.Code != http.StatusOK || !strings.Contains(schema.Body.String(), `"allowed_private_cidrs"`) {
		t.Fatalf("network policy schema = %d %s", schema.Code, schema.Body.String())
	}
	patched := performRouterManagementRequest(router, http.MethodPatch, "/v0/management/router/network-policy", map[string]any{
		"allow_http":             true,
		"allowed_private_hosts":  []string{"CODEX-POOL."},
		"allowed_private_cidrs":  []string{"172.18.0.1/16"},
		"allowed_redirect_hosts": []string{"AUTH.EXAMPLE.COM."},
	}, "router-management-test-key", `"0"`)
	if patched.Code != http.StatusOK || patched.Header().Get("ETag") != `"1"` {
		t.Fatalf("patch network policy = %d %s", patched.Code, patched.Body.String())
	}

	read := performRouterManagementRequest(router, http.MethodGet, "/v0/management/router/network-policy", nil, "router-management-test-key", "")
	for _, field := range []string{
		`"allow_http":true`,
		`"allowed_private_hosts":["codex-pool"]`,
		`"allowed_private_cidrs":["172.18.0.0/16"]`,
		`"allowed_redirect_hosts":["auth.example.com"]`,
	} {
		if !strings.Contains(read.Body.String(), field) {
			t.Fatalf("network policy response is missing %s: %s", field, read.Body.String())
		}
	}

	invalid := performRouterManagementRequest(router, http.MethodPatch, "/v0/management/router/network-policy", map[string]any{
		"allowed_private_cidrs": []string{"0.0.0.0/0"},
	}, "router-management-test-key", `"1"`)
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid network policy = %d %s", invalid.Code, invalid.Body.String())
	}
}

type routerManagementProber struct {
	result smartrouter.RouterProbeResult
	err    error
}

func (p routerManagementProber) ProbeRouterUpstream(context.Context, string) (smartrouter.RouterProbeResult, error) {
	return p.result, p.err
}

func (p routerManagementProber) ProbeRouterRoute(context.Context, string) (smartrouter.RouterProbeResult, error) {
	return p.result, p.err
}

func TestRouterManagementRequiresWriteOnlySecretForAuthenticatedUpstream(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "router-management-test-key")
	directory := t.TempDir()
	metadata, err := smartrouter.NewFileRouterMetadataStore(filepath.Join(directory, "metadata.json"))
	if err != nil {
		t.Fatalf("NewFileRouterMetadataStore() error = %v", err)
	}
	secrets, err := smartrouter.NewEncryptedFileRouterSecretStore(filepath.Join(directory, "secrets.json"), bytes.Repeat([]byte{0x73}, 32))
	if err != nil {
		t.Fatalf("NewEncryptedFileRouterSecretStore() error = %v", err)
	}
	service, err := smartrouter.NewRouterManagementService(metadata, secrets, nil, nil)
	if err != nil {
		t.Fatalf("NewRouterManagementService() error = %v", err)
	}
	handler := NewHandler(&config.Config{
		RemoteManagement: config.RemoteManagement{AllowRemote: true},
	}, "", nil)
	handler.SetRouterManagementService(service, nil)
	router := routerManagementTestRouter(handler)

	response := performRouterManagementRequest(router, http.MethodPost, "/v0/management/router/upstreams", map[string]any{
		"id":       "missing-secret",
		"name":     "Missing secret",
		"protocol": "openai-responses",
		"base_url": "https://example.com/v1",
		"auth": map[string]any{
			"type": "bearer",
		},
		"capabilities": map[string]any{
			"endpoints": []string{"responses"},
		},
	}, "router-management-test-key", `"0"`)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create without secret status = %d, body = %s", response.Code, response.Body.String())
	}
	document, err := metadata.LoadRouterMetadata(context.Background())
	if err != nil {
		t.Fatalf("LoadRouterMetadata() error = %v", err)
	}
	if document.Revision != 0 || len(document.Router.Upstreams) != 0 {
		t.Fatalf("missing-secret request changed metadata: %#v", document)
	}
}

func performRouterManagementRequest(router *gin.Engine, method, path string, body any, key, ifMatch string) *httptest.ResponseRecorder {
	var requestBody *bytes.Reader
	if body == nil {
		requestBody = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		requestBody = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, path, requestBody)
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("X-Management-Key", key)
	}
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func assertRouterResponseRedacted(t *testing.T, response *httptest.ResponseRecorder, fixtures ...string) {
	t.Helper()
	body := response.Body.String()
	for _, fixture := range fixtures {
		if strings.Contains(body, fixture) {
			t.Fatalf("response leaked fixture %q: %s", fixture, body)
		}
	}
	for _, forbidden := range []string{"secret_ref", "secret-ref", "ciphertext", "nonce"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, body)
		}
	}
}

func TestParseRouterIfMatch(t *testing.T) {
	for value, expected := range map[string]uint64{
		"7":     7,
		`"8"`:   8,
		`W/"9"`: 9,
	} {
		revision, err := parseRouterIfMatch(value)
		if err != nil || revision != expected {
			t.Fatalf("parseRouterIfMatch(%q) = %d, %v", value, revision, err)
		}
	}
	for _, value := range []string{"", `"x"`, "-1"} {
		if _, err := parseRouterIfMatch(value); err == nil {
			t.Fatalf("parseRouterIfMatch(%q) unexpectedly passed", value)
		}
	}
}

func TestRouterMasterKeyEncodingExampleIsValid(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	if _, err := smartrouter.ParseRouterMasterKey(key); err != nil {
		t.Fatalf("ParseRouterMasterKey() error = %v", err)
	}
}
