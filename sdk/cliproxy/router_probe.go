package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func (s *Service) ProbeRouterUpstream(ctx context.Context, upstreamID string) (smartrouter.RouterProbeResult, error) {
	if s == nil || s.routerSnapshots == nil {
		return smartrouter.RouterProbeResult{}, smartrouter.ErrRouterProbeUnavailable
	}
	snapshot := s.routerSnapshots.Load()
	upstream, ok := snapshot.Upstream(strings.TrimSpace(upstreamID))
	if !ok {
		return smartrouter.RouterProbeResult{}, fmt.Errorf("probe router upstream: upstream not found")
	}
	return s.probeRouterUpstream(ctx, "", upstream)
}

func (s *Service) ProbeRouterRoute(ctx context.Context, routeID string) (smartrouter.RouterProbeResult, error) {
	if s == nil || s.routerSnapshots == nil {
		return smartrouter.RouterProbeResult{}, smartrouter.ErrRouterProbeUnavailable
	}
	snapshot := s.routerSnapshots.Load()
	route, ok := snapshot.Route(strings.TrimSpace(routeID))
	if !ok {
		return smartrouter.RouterProbeResult{}, fmt.Errorf("probe router route: route not found")
	}
	upstream, ok := snapshot.Upstream(route.UpstreamID)
	if !ok {
		return smartrouter.RouterProbeResult{}, fmt.Errorf("probe router route: upstream not found")
	}
	return s.probeRouterUpstream(ctx, route.ID, upstream)
}

func (s *Service) probeRouterUpstream(ctx context.Context, routeID string, upstream smartrouter.Upstream) (smartrouter.RouterProbeResult, error) {
	if upstream.HealthCheck.Mode != "http" {
		return smartrouter.RouterProbeResult{}, smartrouter.ErrRouterProbeUnsupported
	}
	if ctx == nil {
		ctx = context.Background()
	}
	target, errTarget := routerHealthTarget(upstream.BaseURL, upstream.HealthCheck.Path)
	if errTarget != nil {
		return smartrouter.RouterProbeResult{}, fmt.Errorf("probe router upstream: invalid health target")
	}
	auth, executor, ok := s.activeRouterUpstream(upstream.ID)
	if !ok {
		return smartrouter.RouterProbeResult{}, smartrouter.ErrRouterProbeUnavailable
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if errRequest != nil {
		return smartrouter.RouterProbeResult{}, fmt.Errorf("probe router upstream: create request")
	}
	request.Header.Set("Accept", "application/json")
	startedAt := time.Now()
	response, errResponse := executor.HttpRequest(ctx, auth, request)
	checkedAt := time.Now()
	result := smartrouter.RouterProbeResult{
		UpstreamID: upstream.ID,
		RouteID:    routeID,
		CheckedAt:  checkedAt,
		Latency:    checkedAt.Sub(startedAt),
	}
	if errResponse != nil {
		result.TransportError = true
		result.Category = smartrouter.FailureTransient
		if errors.Is(errResponse, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			result.Category = smartrouter.FailureCancelled
		}
		return result, nil
	}
	if response == nil {
		result.TransportError = true
		result.Category = smartrouter.FailureTransient
		return result, nil
	}
	defer func() {
		_ = response.Body.Close()
	}()
	result.StatusCode = response.StatusCode
	result.Healthy = response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
	if !result.Healthy {
		result.Category = smartrouter.ClassifyHTTPFailure(response.StatusCode)
		if result.Category == smartrouter.FailureNone {
			result.Category = smartrouter.FailureProtocol
		}
	}
	return result, nil
}

func (s *Service) activeRouterUpstream(upstreamID string) (*coreauth.Auth, coreauth.ProviderExecutor, bool) {
	if s == nil || s.routerUpstreamRuntime == nil || s.coreManager == nil {
		return nil, nil, false
	}
	s.routerUpstreamRuntime.mu.Lock()
	authID := s.routerUpstreamRuntime.authIDs[strings.TrimSpace(upstreamID)]
	s.routerUpstreamRuntime.mu.Unlock()
	if authID == "" {
		return nil, nil, false
	}
	auth, ok := s.coreManager.GetByID(authID)
	if !ok || auth == nil {
		return nil, nil, false
	}
	executor, ok := s.coreManager.Executor(auth.Provider)
	return auth, executor, ok
}

func routerHealthTarget(baseURL, healthPath string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid router upstream base URL")
	}
	parsed.Path = strings.TrimSpace(healthPath)
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
