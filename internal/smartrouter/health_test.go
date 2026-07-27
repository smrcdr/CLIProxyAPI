package smartrouter

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestTransportHealthStoreThresholdRecoveryIsIndependentFromCircuit(t *testing.T) {
	now := time.Now()
	upstream := Upstream{
		ID: "primary",
		HealthCheck: HealthCheck{
			HealthyThreshold:   2,
			UnhealthyThreshold: 2,
		},
	}
	health := NewTransportHealthStore()
	failure := RouterProbeResult{
		UpstreamID:     upstream.ID,
		Category:       FailureTransient,
		TransportError: true,
		CheckedAt:      now,
	}
	if status := health.Record(upstream, failure); status.State != TransportHealthUnknown {
		t.Fatalf("first failure state = %q", status.State)
	}
	failure.CheckedAt = now.Add(time.Second)
	if status := health.Record(upstream, failure); status.State != TransportHealthUnhealthy {
		t.Fatalf("second failure state = %q", status.State)
	}
	if health.CanRoute(upstream.ID) {
		t.Fatal("unhealthy upstream remained routable")
	}

	success := RouterProbeResult{UpstreamID: upstream.ID, Healthy: true, CheckedAt: now.Add(2 * time.Second)}
	if status := health.Record(upstream, success); status.State != TransportHealthUnhealthy {
		t.Fatalf("first recovery state = %q", status.State)
	}
	success.CheckedAt = now.Add(3 * time.Second)
	if status := health.Record(upstream, success); status.State != TransportHealthHealthy {
		t.Fatalf("second recovery state = %q", status.State)
	}
	if !health.CanRoute(upstream.ID) {
		t.Fatal("recovered upstream is not routable")
	}

	circuits := NewCircuitStore(DefaultCircuitPolicy())
	if err := circuits.Record("route-primary", false, AttemptResult{Category: FailureAuth}, now); err != nil {
		t.Fatalf("Record(auth) error = %v", err)
	}
	status := circuits.Status("route-primary", now.Add(4*time.Second))
	if status.State != CircuitOpen || !status.RequiresReset {
		t.Fatalf("health recovery changed auth circuit = %#v", status)
	}
}

func TestSelectorSkipsTransportUnhealthyUpstream(t *testing.T) {
	snapshot := healthTestSnapshot(t, 1)
	selector := NewSelector(NewSnapshotStore(snapshot), nil)
	upstream, _ := snapshot.Upstream("primary")
	selector.health.Record(upstream, RouterProbeResult{
		UpstreamID:     upstream.ID,
		Category:       FailureTransient,
		TransportError: true,
		CheckedAt:      time.Now(),
	})

	plan, err := selector.Begin(SelectionRequest{PublicModel: "model-a"})
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if _, err = plan.Next(); err == nil {
		t.Fatal("Next() selected a transport-unhealthy upstream")
	}
}

type healthPollerProber struct {
	mu    sync.Mutex
	calls int
}

func (p *healthPollerProber) ProbeRouterUpstream(_ context.Context, upstreamID string) (RouterProbeResult, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return RouterProbeResult{
		UpstreamID: upstreamID,
		Healthy:    true,
		CheckedAt:  time.Now(),
	}, nil
}

func (*healthPollerProber) ProbeRouterRoute(context.Context, string) (RouterProbeResult, error) {
	return RouterProbeResult{}, nil
}

func (p *healthPollerProber) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestRouterHealthPollerRunsImmediatelyAndPeriodically(t *testing.T) {
	snapshot := healthTestSnapshot(t, 1)
	upstream, _ := snapshot.Upstream("primary")
	upstream.HealthCheck.Interval = 20 * time.Millisecond
	snapshot.upstreams[upstream.ID] = upstream
	snapshots := NewSnapshotStore(snapshot)
	health := NewTransportHealthStore()
	prober := &healthPollerProber{}
	poller := NewRouterHealthPoller(snapshots, prober, health)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poller.Run(ctx)

	deadline := time.Now().Add(time.Second)
	for prober.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if prober.callCount() < 2 {
		t.Fatalf("probe calls = %d, want at least 2", prober.callCount())
	}
	if status := health.Status("primary"); status.State != TransportHealthHealthy {
		t.Fatalf("transport health = %#v", status)
	}
}

func healthTestSnapshot(t *testing.T, unhealthyThreshold int) *Snapshot {
	t.Helper()
	cfg := &config.Config{
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
					HealthCheck: config.RouterHealthCheck{
						Mode:               "http",
						Path:               "/healthz",
						Interval:           "30s",
						UnhealthyThreshold: unhealthyThreshold,
						HealthyThreshold:   1,
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
							ID:            "route-primary",
							UpstreamID:    "primary",
							UpstreamModel: "upstream-model",
							Priority:      100,
							Weight:        100,
						},
					},
				},
			},
		},
	}
	snapshot, err := CompileSnapshot(cfg, 1)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	return snapshot
}
