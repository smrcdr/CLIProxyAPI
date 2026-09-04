package cliproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRouterManagementPersistsAndReloadsLiveConfiguration(t *testing.T) {
	t.Setenv("SMART_ROUTER_MASTER_KEY", "")
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	initial := serviceRouterTestConfig("model-a")
	initial.AuthDir = filepath.Join(directory, "auth")

	first, err := NewBuilder().
		WithConfig(initial).
		WithConfigPath(configPath).
		Build()
	if err != nil {
		t.Fatalf("first Build() error = %v", err)
	}
	if first.routerManagement == nil {
		t.Fatal("builder did not initialize router management")
	}

	next := serviceRouterTestConfig("model-b")
	document, err := first.routerManagement.Replace(context.Background(), smartrouter.RouterMutation{
		ExpectedRevision: 1,
		Router:           next.Router,
		Action:           "replace",
		ResourceType:     "router",
	})
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if document.Revision != 2 {
		t.Fatalf("persisted revision = %d, want 2", document.Revision)
	}
	if !first.routerSelector.HandlesModel("model-b") || first.routerSelector.HandlesModel("model-a") {
		t.Fatal("management mutation did not replace the live selector snapshot")
	}

	stale := serviceRouterTestConfig("model-from-stale-yaml")
	stale.AuthDir = initial.AuthDir
	restarted, err := NewBuilder().
		WithConfig(stale).
		WithConfigPath(configPath).
		Build()
	if err != nil {
		t.Fatalf("restart Build() error = %v", err)
	}
	if restarted.routerSnapshots.Load().Revision() != 2 {
		t.Fatalf("restart snapshot revision = %d, want 2", restarted.routerSnapshots.Load().Revision())
	}
	if !restarted.routerSelector.HandlesModel("model-b") || restarted.routerSelector.HandlesModel("model-from-stale-yaml") {
		t.Fatal("restart did not prefer persisted router metadata")
	}
}

func TestBuilderRequiresRouterMasterKeyForManagedSecrets(t *testing.T) {
	t.Setenv("SMART_ROUTER_MASTER_KEY", "")
	cfg := serviceRouterTestConfig("model-a")
	cfg.AuthDir = t.TempDir()
	cfg.Router.Upstreams[0].Auth = config.RouterUpstreamAuth{
		Type:      config.RouterAuthBearer,
		SecretRef: "router/upstreams/primary",
	}

	_, err := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		Build()
	if err == nil {
		t.Fatal("Build() unexpectedly accepted managed secret without master key")
	}
	if !strings.Contains(err.Error(), "SMART_ROUTER_MASTER_KEY") {
		t.Fatalf("Build() error = %v", err)
	}
}

func TestBuilderRejectsLocalCredentialFilesInRouterAuthDirectory(t *testing.T) {
	directory := t.TempDir()
	authDirectory := filepath.Join(directory, "auth")
	if err := os.MkdirAll(authDirectory, 0o700); err != nil {
		t.Fatalf("MkdirAll(auth) error = %v", err)
	}
	const fileName = "codex-private-account.json"
	const secretFixture = "local-token-fixture"
	if err := os.WriteFile(filepath.Join(authDirectory, fileName), []byte(`{"access_token":"`+secretFixture+`"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(auth) error = %v", err)
	}
	cfg := serviceRouterTestConfig("model-a")
	cfg.AuthDir = authDirectory

	_, err := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(directory, "config.yaml")).
		Build()
	if err == nil {
		t.Fatal("Build() unexpectedly accepted a local credential file")
	}
	if !strings.Contains(err.Error(), "local credential JSON") {
		t.Fatalf("Build() error = %v", err)
	}
	if strings.Contains(err.Error(), fileName) || strings.Contains(err.Error(), secretFixture) {
		t.Fatalf("Build() error leaked credential details: %v", err)
	}
}

func TestBuilderAllowsRouterOperationalAffinityStateFile(t *testing.T) {
	directory := t.TempDir()
	authDirectory := filepath.Join(directory, "auth")
	if err := os.MkdirAll(authDirectory, 0o700); err != nil {
		t.Fatalf("MkdirAll(auth) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDirectory, "smartapi-affinity.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("WriteFile(affinity) error = %v", err)
	}
	cfg := serviceRouterTestConfig("model-a")
	cfg.AuthDir = authDirectory

	if _, err := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(directory, "config.yaml")).
		Build(); err != nil {
		t.Fatalf("Build() error = %v", err)
	}
}

func TestBuilderRejectsPreloadedCoreCredentialsInRouterRole(t *testing.T) {
	manager := coreauth.NewManager(&routerRuntimeStore{}, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterRuntime(&coreauth.Auth{
		ID:       "preloaded-local-account",
		Provider: "codex",
	})
	cfg := serviceRouterTestConfig("model-a")
	cfg.AuthDir = t.TempDir()

	_, err := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		WithCoreAuthManager(manager).
		Build()
	if err == nil || !strings.Contains(err.Error(), "preloaded local credentials") {
		t.Fatalf("Build() error = %v", err)
	}
}

func TestRouterManagementEncryptsSecretAndAppliesItToRuntime(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x63}, 32))
	t.Setenv("SMART_ROUTER_MASTER_KEY", key)
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	cfg := serviceRouterTestConfig("model-a")
	cfg.AuthDir = filepath.Join(directory, "auth")

	service, err := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	const secret = "runtime-only-router-secret"
	const reference = "router/upstreams/primary"
	next := serviceRouterTestConfig("model-a")
	next.Router.Upstreams[0].Auth = config.RouterUpstreamAuth{
		Type:      config.RouterAuthBearer,
		SecretRef: reference,
	}
	document, err := service.routerManagement.Replace(context.Background(), smartrouter.RouterMutation{
		ExpectedRevision: 1,
		Router:           next.Router,
		Secrets: []smartrouter.RouterSecretMutation{
			{Reference: reference, Value: []byte(secret)},
		},
		Action:       "patch",
		ResourceType: "upstream",
		ResourceID:   "primary",
	})
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if document.Revision != 2 {
		t.Fatalf("persisted revision = %d, want 2", document.Revision)
	}
	auth, ok := service.coreManager.GetByID("smartrouter-upstream-primary")
	if !ok {
		t.Fatal("runtime upstream auth is missing")
	}
	if got := auth.Attributes[coreauth.AttributeAPIKey]; got != secret {
		t.Fatalf("runtime secret = %q", got)
	}

	metadataBytes, err := os.ReadFile(filepath.Join(directory, "router", "metadata.json"))
	if err != nil {
		t.Fatalf("ReadFile(metadata) error = %v", err)
	}
	secretBytes, err := os.ReadFile(filepath.Join(directory, "router", "secrets.json"))
	if err != nil {
		t.Fatalf("ReadFile(secrets) error = %v", err)
	}
	if bytes.Contains(metadataBytes, []byte(secret)) || bytes.Contains(secretBytes, []byte(secret)) {
		t.Fatal("plaintext router secret leaked to persistent storage")
	}
}
