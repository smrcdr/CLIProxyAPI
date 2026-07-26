package cliproxy

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
)

func TestBuilderInitializesRouterSnapshot(t *testing.T) {
	cfg := serviceRouterTestConfig("model-a")
	cfg.AuthDir = t.TempDir()
	service, err := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(t.TempDir() + "/config.yaml").
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if service.routerSnapshots == nil || service.routerSnapshots.Load() == nil {
		t.Fatal("builder did not initialize router snapshot store")
	}
	if service.routerSnapshots.Load().Revision() != 1 {
		t.Fatalf("initial snapshot revision = %d, want 1", service.routerSnapshots.Load().Revision())
	}
	if service.routerSelector == nil {
		t.Fatal("builder did not initialize router selector")
	}
	if _, ok := service.routerSnapshots.Load().ModelGroupForModel("model-a"); !ok {
		t.Fatal("initial snapshot does not contain model-a")
	}
}

func TestBuilderRejectsInvalidRouterConfiguration(t *testing.T) {
	cfg := serviceRouterTestConfig("model-a")
	cfg.Router.Upstreams[0].BaseURL = "not-a-url"
	_, err := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(t.TempDir() + "/config.yaml").
		Build()
	if err == nil {
		t.Fatal("Build() error = nil")
	}
	if !strings.Contains(err.Error(), "absolute HTTP or HTTPS URL") {
		t.Fatalf("Build() error = %q", err)
	}
}

func TestApplyConfigUpdateSwapsRouterSnapshot(t *testing.T) {
	initialConfig := serviceRouterTestConfig("model-a")
	initialSnapshot, err := smartrouter.CompileSnapshot(initialConfig, 1)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	service := &Service{
		cfg:             initialConfig,
		routerSnapshots: smartrouter.NewSnapshotStore(initialSnapshot),
	}

	nextConfig := serviceRouterTestConfig("model-b")
	service.applyConfigUpdateWithAuthSynthesis(nextConfig, false)

	loaded := service.routerSnapshots.Load()
	if loaded.Revision() != 2 {
		t.Fatalf("snapshot revision = %d, want 2", loaded.Revision())
	}
	if _, ok := loaded.ModelGroupForModel("model-b"); !ok {
		t.Fatal("new router model group is missing")
	}
	if service.cfg != nextConfig {
		t.Fatal("service config was not replaced")
	}
}

func TestApplyConfigUpdateRejectsInvalidRouterSnapshot(t *testing.T) {
	initialConfig := serviceRouterTestConfig("model-a")
	initialSnapshot, err := smartrouter.CompileSnapshot(initialConfig, 4)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	service := &Service{
		cfg:             initialConfig,
		routerSnapshots: smartrouter.NewSnapshotStore(initialSnapshot),
	}

	invalid := serviceRouterTestConfig("model-b")
	invalid.Router.ModelGroups[0].Routes[0].UpstreamID = "missing"
	service.applyConfigUpdateWithAuthSynthesis(invalid, false)

	if loaded := service.routerSnapshots.Load(); loaded.Revision() != 4 {
		t.Fatalf("snapshot revision = %d, want unchanged revision 4", loaded.Revision())
	}
	if service.cfg != initialConfig {
		t.Fatal("invalid configuration replaced service config")
	}
}

func TestApplyConfigUpdateKeepsExistingRequestOnPinnedSnapshot(t *testing.T) {
	initialConfig := serviceRouterTestConfig("model-a")
	initialSnapshot, err := smartrouter.CompileSnapshot(initialConfig, 1)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	store := smartrouter.NewSnapshotStore(initialSnapshot)
	service := &Service{
		cfg:             initialConfig,
		routerSnapshots: store,
		routerSelector:  smartrouter.NewSelector(store, nil),
	}
	oldPlan, err := service.routerSelector.Begin(smartrouter.SelectionRequest{PublicModel: "model-a"})
	if err != nil {
		t.Fatalf("Begin(old plan) error = %v", err)
	}

	nextConfig := serviceRouterTestConfig("model-b")
	service.applyConfigUpdateWithAuthSynthesis(nextConfig, false)

	oldSelection, err := oldPlan.Next()
	if err != nil {
		t.Fatalf("oldPlan.Next() error = %v", err)
	}
	if oldSelection.SnapshotRevision != 1 || oldSelection.Route.UpstreamModel != "model-a" {
		t.Fatalf("old selection = %#v", oldSelection)
	}
	newPlan, err := service.routerSelector.Begin(smartrouter.SelectionRequest{PublicModel: "model-b"})
	if err != nil {
		t.Fatalf("Begin(new plan) error = %v", err)
	}
	newSelection, err := newPlan.Next()
	if err != nil {
		t.Fatalf("newPlan.Next() error = %v", err)
	}
	if newSelection.SnapshotRevision != 2 || newSelection.Route.UpstreamModel != "model-b" {
		t.Fatalf("new selection = %#v", newSelection)
	}
}

func TestApplyConfigUpdateResetsCircuitForChangedUpstream(t *testing.T) {
	initialConfig := serviceRouterTestConfig("model-a")
	initialSnapshot, err := smartrouter.CompileSnapshot(initialConfig, 1)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	store := smartrouter.NewSnapshotStore(initialSnapshot)
	service := &Service{
		cfg:             initialConfig,
		routerSnapshots: store,
		routerSelector:  smartrouter.NewSelector(store, nil),
	}
	plan, err := service.routerSelector.Begin(smartrouter.SelectionRequest{PublicModel: "model-a"})
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	selection, err := plan.Next()
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if err := plan.Report(selection, smartrouter.AttemptResult{Category: smartrouter.FailureAuth}); err != nil {
		t.Fatalf("Report(auth) error = %v", err)
	}
	if status := service.routerSelector.CircuitStatus(selection.Route.ID); status.State != smartrouter.CircuitOpen {
		t.Fatalf("status before update = %#v", status)
	}

	nextConfig := serviceRouterTestConfig("model-a")
	nextConfig.Router.Upstreams[0].BaseURL = "https://changed.example.com/v1"
	service.applyConfigUpdateWithAuthSynthesis(nextConfig, false)

	if status := service.routerSelector.CircuitStatus(selection.Route.ID); status.State != smartrouter.CircuitClosed {
		t.Fatalf("status after upstream update = %#v", status)
	}
	newPlan, err := service.routerSelector.Begin(smartrouter.SelectionRequest{PublicModel: "model-a"})
	if err != nil {
		t.Fatalf("Begin(after update) error = %v", err)
	}
	if _, err := newPlan.Next(); err != nil {
		t.Fatalf("Next(after update) error = %v", err)
	}
}

func serviceRouterTestConfig(publicModel string) *config.Config {
	return &config.Config{
		AuthDir:     "",
		ServiceRole: config.ServiceRoleRouter,
		Router: config.RouterConfig{
			Upstreams: []config.RouterUpstream{
				{
					ID:       "primary",
					Name:     "Primary",
					Protocol: config.RouterProtocolOpenAIResponses,
					BaseURL:  "https://example.com/v1",
					Capabilities: config.RouterCapabilities{
						Endpoints: []string{config.RouterEndpointResponses},
					},
				},
			},
			ModelGroups: []config.RouterModelGroup{
				{
					ID:          publicModel,
					PublicModel: publicModel,
					Capability:  config.RouterCapabilityText,
					Routes: []config.RouterRoute{
						{
							ID:            publicModel + "-primary",
							UpstreamID:    "primary",
							UpstreamModel: publicModel,
							Priority:      100,
							Weight:        100,
						},
					},
				},
			},
		},
	}
}
