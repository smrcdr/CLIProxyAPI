package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
)

func TestServiceRouterProbeUsesConfiguredNonInferenceHealthPath(t *testing.T) {
	var requestedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestedPath = request.URL.Path
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	cfg := serviceRouterTestConfig("model-a")
	cfg.AuthDir = t.TempDir()
	cfg.Router.NetworkPolicy = localRouterTestNetworkPolicy()
	cfg.Router.Upstreams[0].BaseURL = upstream.URL + "/v1"
	cfg.Router.Upstreams[0].HealthCheck.Mode = "http"
	cfg.Router.Upstreams[0].HealthCheck.Path = "/healthz"
	cfg.Router.Upstreams[0].HealthCheck.Interval = "30s"
	cfg.Router.Upstreams[0].HealthCheck.UnhealthyThreshold = 3
	cfg.Router.Upstreams[0].HealthCheck.HealthyThreshold = 2
	service, err := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if err = service.routerUpstreamRuntime.Reconcile(context.Background(), service.cfg, service.routerSnapshots.Load()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	result, err := service.ProbeRouterUpstream(context.Background(), "primary")
	if err != nil {
		t.Fatalf("ProbeRouterUpstream() error = %v", err)
	}
	if !result.Healthy || result.StatusCode != http.StatusNoContent || result.UpstreamID != "primary" {
		t.Fatalf("upstream probe = %#v", result)
	}
	if requestedPath != "/healthz" {
		t.Fatalf("health path = %q, want /healthz", requestedPath)
	}

	result, err = service.ProbeRouterRoute(context.Background(), "model-a-primary")
	if err != nil {
		t.Fatalf("ProbeRouterRoute() error = %v", err)
	}
	if result.RouteID != "model-a-primary" || !result.Healthy {
		t.Fatalf("route probe = %#v", result)
	}
}

func localRouterTestNetworkPolicy() config.RouterNetworkPolicy {
	return config.RouterNetworkPolicy{
		AllowHTTP:           true,
		AllowedPrivateCIDRs: []string{"127.0.0.0/8"},
	}
}

func TestServiceRouterProbeRejectsInferenceFallback(t *testing.T) {
	cfg := serviceRouterTestConfig("model-a")
	cfg.AuthDir = t.TempDir()
	service, err := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	_, err = service.ProbeRouterUpstream(context.Background(), "primary")
	if !errors.Is(err, smartrouter.ErrRouterProbeUnsupported) {
		t.Fatalf("ProbeRouterUpstream() error = %v", err)
	}
}
