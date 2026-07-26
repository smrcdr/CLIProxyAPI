package smartrouter

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCompileSnapshotBuildsSortedIndexes(t *testing.T) {
	cfg := snapshotTestConfig()
	snapshot, err := CompileSnapshot(cfg, 7)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	if snapshot.Revision() != 7 {
		t.Fatalf("Revision() = %d, want 7", snapshot.Revision())
	}

	group, ok := snapshot.ModelGroupForModel("model-a")
	if !ok {
		t.Fatal("ModelGroupForModel() did not find model-a")
	}
	if len(group.Routes) != 3 {
		t.Fatalf("len(Routes) = %d, want 3", len(group.Routes))
	}
	gotOrder := []string{group.Routes[0].ID, group.Routes[1].ID, group.Routes[2].ID}
	wantOrder := []string{"route-a", "route-b", "route-backup"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("route order = %v, want %v", gotOrder, wantOrder)
		}
	}

	available := snapshot.AvailableRoutesForModel("model-a")
	if len(available) != 2 {
		t.Fatalf("len(AvailableRoutesForModel) = %d, want 2", len(available))
	}
	if available[0].ID != "route-a" || available[1].ID != "route-backup" {
		t.Fatalf("available routes = %#v", available)
	}
	if route, okRoute := snapshot.Route("route-backup"); !okRoute || route.Priority != 50 {
		t.Fatalf("Route(route-backup) = %#v, %t", route, okRoute)
	}
}

func TestSnapshotIsDefensivelyImmutable(t *testing.T) {
	cfg := snapshotTestConfig()
	snapshot, err := CompileSnapshot(cfg, 1)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}

	cfg.Router.Upstreams[0].Headers["X-Test"] = "mutated-input"
	cfg.Router.Upstreams[0].Capabilities.Endpoints[0] = "mutated-input"
	cfg.Router.ModelGroups[0].Routes[0].UpstreamModel = "mutated-input"

	upstream, ok := snapshot.Upstream("primary")
	if !ok {
		t.Fatal("Upstream(primary) not found")
	}
	if upstream.Headers["X-Test"] != "original" {
		t.Fatalf("snapshot header = %q, want original", upstream.Headers["X-Test"])
	}
	if upstream.Capabilities.Endpoints[0] != config.RouterEndpointResponses {
		t.Fatalf("snapshot endpoint = %q, want responses", upstream.Capabilities.Endpoints[0])
	}
	group, _ := snapshot.ModelGroupForModel("model-a")
	if group.Routes[0].UpstreamModel != "upstream-a" {
		t.Fatalf("snapshot upstream model = %q, want upstream-a", group.Routes[0].UpstreamModel)
	}

	upstream.Headers["X-Test"] = "mutated-output"
	upstream.Capabilities.Endpoints[0] = "mutated-output"
	group.Routes[0].UpstreamModel = "mutated-output"

	upstreamAgain, _ := snapshot.Upstream("primary")
	groupAgain, _ := snapshot.ModelGroupForModel("model-a")
	if upstreamAgain.Headers["X-Test"] != "original" {
		t.Fatalf("stored header changed to %q", upstreamAgain.Headers["X-Test"])
	}
	if upstreamAgain.Capabilities.Endpoints[0] != config.RouterEndpointResponses {
		t.Fatalf("stored endpoint changed to %q", upstreamAgain.Capabilities.Endpoints[0])
	}
	if groupAgain.Routes[0].UpstreamModel != "upstream-a" {
		t.Fatalf("stored route changed to %q", groupAgain.Routes[0].UpstreamModel)
	}
}

func TestSnapshotStoreSwapsWholeRevision(t *testing.T) {
	first, err := CompileSnapshot(snapshotTestConfig(), 1)
	if err != nil {
		t.Fatalf("CompileSnapshot(first) error = %v", err)
	}
	secondConfig := snapshotTestConfig()
	secondConfig.Router.ModelGroups[0].PublicModel = "model-b"
	second, err := CompileSnapshot(secondConfig, 2)
	if err != nil {
		t.Fatalf("CompileSnapshot(second) error = %v", err)
	}

	store := NewSnapshotStore(first)
	if got := store.Load().Revision(); got != 1 {
		t.Fatalf("initial revision = %d, want 1", got)
	}
	if errSwap := store.Swap(second); errSwap != nil {
		t.Fatalf("Swap() error = %v", errSwap)
	}
	loaded := store.Load()
	if loaded.Revision() != 2 {
		t.Fatalf("loaded revision = %d, want 2", loaded.Revision())
	}
	if _, ok := loaded.ModelGroupForModel("model-a"); ok {
		t.Fatal("old model remained after snapshot swap")
	}
	if _, ok := loaded.ModelGroupForModel("model-b"); !ok {
		t.Fatal("new model missing after snapshot swap")
	}
	if errSwap := store.Swap(nil); errSwap == nil {
		t.Fatal("Swap(nil) error = nil")
	}
}

func TestCompileSnapshotRejectsInvalidConfiguration(t *testing.T) {
	cfg := snapshotTestConfig()
	cfg.Router.ModelGroups[0].Routes[0].UpstreamID = "missing"

	if _, err := CompileSnapshot(cfg, 1); err == nil {
		t.Fatal("CompileSnapshot() error = nil")
	}
}

func snapshotTestConfig() *config.Config {
	disabled := false
	return &config.Config{
		ServiceRole: config.ServiceRoleRouter,
		Router: config.RouterConfig{
			Upstreams: []config.RouterUpstream{
				{
					ID:       "primary",
					Name:     "Primary",
					Protocol: config.RouterProtocolOpenAIResponses,
					BaseURL:  "https://primary.example/v1",
					Headers:  map[string]string{"X-Test": "original"},
					Capabilities: config.RouterCapabilities{
						Endpoints: []string{config.RouterEndpointResponses},
						Streaming: true,
					},
				},
				{
					ID:       "disabled",
					Name:     "Disabled",
					Enabled:  &disabled,
					Protocol: config.RouterProtocolOpenAIResponses,
					BaseURL:  "https://disabled.example/v1",
					Capabilities: config.RouterCapabilities{
						Endpoints: []string{config.RouterEndpointResponses},
					},
				},
			},
			ModelGroups: []config.RouterModelGroup{
				{
					ID:          "model-a",
					PublicModel: "model-a",
					Capability:  config.RouterCapabilityText,
					Routes: []config.RouterRoute{
						{
							ID:            "route-backup",
							UpstreamID:    "primary",
							UpstreamModel: "upstream-backup",
							Priority:      50,
							Weight:        100,
						},
						{
							ID:            "route-b",
							UpstreamID:    "disabled",
							UpstreamModel: "upstream-b",
							Priority:      100,
							Weight:        50,
						},
						{
							ID:            "route-a",
							UpstreamID:    "primary",
							UpstreamModel: "upstream-a",
							Priority:      100,
							Weight:        50,
						},
					},
				},
			},
		},
	}
}
