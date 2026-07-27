package smartrouter

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestRequestPlanExhaustsPriorityTierBeforeBackup(t *testing.T) {
	selector := selectorForConfig(t, selectorTestConfig(config.RouterSelectionWeightedRoundRobin, 1, 1))
	plan, err := selector.Begin(SelectionRequest{PublicModel: "model-a"})
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}

	var routeIDs []string
	for attempt := 0; attempt < 3; attempt++ {
		selection, errNext := plan.Next()
		if errNext != nil {
			t.Fatalf("Next(%d) error = %v", attempt+1, errNext)
		}
		routeIDs = append(routeIDs, selection.Route.ID)
		if errReport := plan.Report(selection, AttemptResult{Category: FailureTransient}); errReport != nil {
			t.Fatalf("Report(%d) error = %v", attempt+1, errReport)
		}
	}
	if routeIDs[0] == routeIDs[1] {
		t.Fatalf("top tier repeated route: %v", routeIDs)
	}
	if routeIDs[0] == "route-backup" || routeIDs[1] == "route-backup" || routeIDs[2] != "route-backup" {
		t.Fatalf("priority order = %v, want both primaries then backup", routeIDs)
	}
	if _, errNext := plan.Next(); !errors.Is(errNext, ErrNoRouteAvailable) {
		t.Fatalf("Next(after exhaustion) error = %v, want ErrNoRouteAvailable", errNext)
	}
}

func TestWeightedRoundRobinDistribution(t *testing.T) {
	selector := selectorForConfig(t, selectorTestConfig(config.RouterSelectionWeightedRoundRobin, 1, 3))
	counts := map[string]int{}
	for request := 0; request < 100; request++ {
		plan, err := selector.Begin(SelectionRequest{PublicModel: "model-a"})
		if err != nil {
			t.Fatalf("Begin(%d) error = %v", request, err)
		}
		selection, err := plan.Next()
		if err != nil {
			t.Fatalf("Next(%d) error = %v", request, err)
		}
		counts[selection.Route.ID]++
		if err := plan.Report(selection, AttemptResult{Success: true}); err != nil {
			t.Fatalf("Report(%d) error = %v", request, err)
		}
	}
	if counts["route-a"] != 25 || counts["route-b"] != 75 {
		t.Fatalf("weighted counts = %#v, want route-a=25 route-b=75", counts)
	}
}

func TestWeightedAffinityIsStableAndWeighted(t *testing.T) {
	selector := selectorForConfig(t, selectorTestConfig(config.RouterSelectionWeightedAffinity, 1, 3))
	selector.now = func() time.Time {
		return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	}

	first := selectForAffinity(t, selector, "stable-client")
	for request := 0; request < 10; request++ {
		if got := selectForAffinity(t, selector, "stable-client"); got != first {
			t.Fatalf("stable affinity changed from %q to %q", first, got)
		}
	}

	counts := map[string]int{}
	const samples = 5000
	for sample := 0; sample < samples; sample++ {
		counts[selectForAffinity(t, selector, fmt.Sprintf("client-%d", sample))]++
	}
	routeBShare := float64(counts["route-b"]) / samples
	if routeBShare < 0.70 || routeBShare > 0.80 {
		t.Fatalf("weighted affinity counts = %#v, route-b share %.3f outside [0.70, 0.80]", counts, routeBShare)
	}
}

func TestSelectorSkipsOpenCircuit(t *testing.T) {
	selector := selectorForConfig(t, selectorTestConfig(config.RouterSelectionWeightedRoundRobin, 1, 1))
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	selector.now = func() time.Time { return now }
	if err := selector.circuits.Record("route-a", false, AttemptResult{Category: FailureAuth}, now); err != nil {
		t.Fatalf("Record(auth) error = %v", err)
	}

	plan, err := selector.Begin(SelectionRequest{PublicModel: "model-a"})
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	first, err := plan.Next()
	if err != nil {
		t.Fatalf("Next(first) error = %v", err)
	}
	if first.Route.ID != "route-b" {
		t.Fatalf("first route = %q, want route-b", first.Route.ID)
	}
}

func TestRequestPlanPinsSnapshotRevision(t *testing.T) {
	firstConfig := selectorTestConfig(config.RouterSelectionWeightedRoundRobin, 1, 1)
	firstSnapshot, err := CompileSnapshot(firstConfig, 4)
	if err != nil {
		t.Fatalf("CompileSnapshot(first) error = %v", err)
	}
	store := NewSnapshotStore(firstSnapshot)
	selector := NewSelector(store, nil)
	oldPlan, err := selector.Begin(SelectionRequest{PublicModel: "model-a"})
	if err != nil {
		t.Fatalf("Begin(old) error = %v", err)
	}

	secondConfig := selectorTestConfig(config.RouterSelectionWeightedRoundRobin, 1, 1)
	secondConfig.Router.ModelGroups[0].Routes[0].UpstreamModel = "new-upstream-a"
	secondConfig.Router.ModelGroups[0].Routes[1].UpstreamModel = "new-upstream-b"
	secondSnapshot, err := CompileSnapshot(secondConfig, 5)
	if err != nil {
		t.Fatalf("CompileSnapshot(second) error = %v", err)
	}
	if err := selector.SwapSnapshot(secondSnapshot); err != nil {
		t.Fatalf("SwapSnapshot() error = %v", err)
	}

	oldSelection, err := oldPlan.Next()
	if err != nil {
		t.Fatalf("oldPlan.Next() error = %v", err)
	}
	if oldSelection.SnapshotRevision != 4 || oldSelection.Route.UpstreamModel != "upstream-a" {
		t.Fatalf("old selection = %#v", oldSelection)
	}

	newPlan, err := selector.Begin(SelectionRequest{PublicModel: "model-a"})
	if err != nil {
		t.Fatalf("Begin(new) error = %v", err)
	}
	newSelection, err := newPlan.Next()
	if err != nil {
		t.Fatalf("newPlan.Next() error = %v", err)
	}
	if newSelection.SnapshotRevision != 5 ||
		(newSelection.Route.UpstreamModel != "new-upstream-a" && newSelection.Route.UpstreamModel != "new-upstream-b") {
		t.Fatalf("new selection = %#v", newSelection)
	}
}

func TestPublicModelsReturnsOnlyStaticallyRoutableGroups(t *testing.T) {
	cfg := selectorTestConfig(config.RouterSelectionWeightedRoundRobin, 1, 1)
	disabled := false
	cfg.Router.Upstreams = append(cfg.Router.Upstreams, config.RouterUpstream{
		ID:       "disabled",
		Name:     "Disabled",
		Enabled:  &disabled,
		Protocol: config.RouterProtocolOpenAIResponses,
		BaseURL:  "https://disabled.example/v1",
		Capabilities: config.RouterCapabilities{
			Endpoints: []string{config.RouterEndpointResponses},
		},
	})
	cfg.Router.ModelGroups = append(cfg.Router.ModelGroups,
		config.RouterModelGroup{
			ID:          "model-disabled",
			PublicModel: "model-disabled",
			Enabled:     &disabled,
			Capability:  config.RouterCapabilityText,
			Routes: []config.RouterRoute{{
				ID:            "route-disabled-model",
				UpstreamID:    "upstream-a",
				UpstreamModel: "upstream-disabled",
				Weight:        1,
			}},
		},
		config.RouterModelGroup{
			ID:          "model-z",
			PublicModel: "model-z",
			Capability:  config.RouterCapabilityText,
			Routes: []config.RouterRoute{{
				ID:            "route-disabled-upstream",
				UpstreamID:    "disabled",
				UpstreamModel: "upstream-z",
				Weight:        1,
			}},
		},
	)
	selector := selectorForConfig(t, cfg)

	models := selector.PublicModels()
	if len(models) != 1 || models[0] != "model-a" {
		t.Fatalf("PublicModels() = %v, want [model-a]", models)
	}
}

func TestRequestPlanRejectsDuplicateReport(t *testing.T) {
	selector := selectorForConfig(t, selectorTestConfig(config.RouterSelectionWeightedRoundRobin, 1, 1))
	plan, err := selector.Begin(SelectionRequest{PublicModel: "model-a"})
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	selection, err := plan.Next()
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if err := plan.Report(selection, AttemptResult{Success: true}); err != nil {
		t.Fatalf("Report(first) error = %v", err)
	}
	if err := plan.Report(selection, AttemptResult{Success: true}); !errors.Is(err, ErrSelectionClosed) {
		t.Fatalf("Report(duplicate) error = %v, want ErrSelectionClosed", err)
	}
}

func selectForAffinity(t *testing.T, selector *Selector, affinityKey string) string {
	t.Helper()
	plan, err := selector.Begin(SelectionRequest{
		PublicModel: "model-a",
		AffinityKey: affinityKey,
	})
	if err != nil {
		t.Fatalf("Begin(%q) error = %v", affinityKey, err)
	}
	selection, err := plan.Next()
	if err != nil {
		t.Fatalf("Next(%q) error = %v", affinityKey, err)
	}
	if err := plan.Report(selection, AttemptResult{Success: true}); err != nil {
		t.Fatalf("Report(%q) error = %v", affinityKey, err)
	}
	return selection.Route.ID
}

func selectorForConfig(t *testing.T, cfg *config.Config) *Selector {
	t.Helper()
	snapshot, err := CompileSnapshot(cfg, 1)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	return NewSelector(NewSnapshotStore(snapshot), NewCircuitStore(DefaultCircuitPolicy()))
}

func selectorTestConfig(strategy config.RouterSelectionStrategy, weightA, weightB int) *config.Config {
	return &config.Config{
		ServiceRole: config.ServiceRoleRouter,
		Router: config.RouterConfig{
			Upstreams: []config.RouterUpstream{
				{
					ID:       "upstream-a",
					Name:     "Upstream A",
					Protocol: config.RouterProtocolOpenAIResponses,
					BaseURL:  "https://a.example/v1",
					Capabilities: config.RouterCapabilities{
						Endpoints: []string{config.RouterEndpointResponses},
					},
				},
				{
					ID:       "upstream-b",
					Name:     "Upstream B",
					Protocol: config.RouterProtocolOpenAIResponses,
					BaseURL:  "https://b.example/v1",
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
					Selection: config.RouterSelection{
						Strategy:    strategy,
						AffinityTTL: "720h",
					},
					Routes: []config.RouterRoute{
						{
							ID:            "route-a",
							UpstreamID:    "upstream-a",
							UpstreamModel: "upstream-a",
							Priority:      100,
							Weight:        weightA,
						},
						{
							ID:            "route-b",
							UpstreamID:    "upstream-b",
							UpstreamModel: "upstream-b",
							Priority:      100,
							Weight:        weightB,
						},
						{
							ID:            "route-backup",
							UpstreamID:    "upstream-b",
							UpstreamModel: "upstream-backup",
							Priority:      50,
							Weight:        1,
						},
					},
				},
			},
		},
	}
}
