package smartrouter

import (
	"context"
	"errors"
	"time"
)

var (
	ErrRouterProbeUnavailable = errors.New("router probe runtime is unavailable")
	ErrRouterProbeUnsupported = errors.New("router upstream has no non-inference health check")
)

// RouterProbeResult is an allowlisted management diagnostic. It intentionally
// excludes upstream response headers, response bodies, and transport errors.
type RouterProbeResult struct {
	UpstreamID     string
	RouteID        string
	Healthy        bool
	StatusCode     int
	Category       FailureCategory
	TransportError bool
	CheckedAt      time.Time
	Latency        time.Duration
}

// RouterProber performs configured non-inference health checks.
type RouterProber interface {
	ProbeRouterUpstream(context.Context, string) (RouterProbeResult, error)
	ProbeRouterRoute(context.Context, string) (RouterProbeResult, error)
}
