package smartrouter

import (
	"sort"
	"sync"
	"time"
)

type RouterMetricTotals struct {
	Requests               uint64 `json:"requests"`
	Successes              uint64 `json:"successes"`
	Failures               uint64 `json:"failures"`
	Attempts               uint64 `json:"attempts"`
	Failovers              uint64 `json:"failovers"`
	PartialStreamFailures  uint64 `json:"partial_stream_failures"`
	UsageMissing           uint64 `json:"usage_missing"`
	ImageAmbiguousFailures uint64 `json:"image_ambiguous_failures"`
}

type RouterRequestMetric struct {
	PublicModel    string          `json:"public_model"`
	Endpoint       string          `json:"endpoint"`
	Result         FailureCategory `json:"result,omitempty"`
	Success        bool            `json:"success"`
	Count          uint64          `json:"count"`
	LatencyCount   uint64          `json:"latency_count"`
	LatencyTotalMS uint64          `json:"latency_total_ms"`
	LatencyMaxMS   uint64          `json:"latency_max_ms"`
	TTFTCount      uint64          `json:"ttft_count"`
	TTFTTotalMS    uint64          `json:"ttft_total_ms"`
	TTFTMaxMS      uint64          `json:"ttft_max_ms"`
}

type RouterRouteMetric struct {
	PublicModel string          `json:"public_model"`
	Endpoint    string          `json:"endpoint"`
	RouteID     string          `json:"route_id"`
	UpstreamID  string          `json:"upstream_id"`
	StatusClass string          `json:"status_class"`
	Result      FailureCategory `json:"result,omitempty"`
	Selections  uint64          `json:"selections"`
}

type RouterMetricsSnapshot struct {
	StartedAt  time.Time             `json:"started_at"`
	ObservedAt time.Time             `json:"observed_at"`
	Totals     RouterMetricTotals    `json:"totals"`
	Requests   []RouterRequestMetric `json:"requests"`
	Routes     []RouterRouteMetric   `json:"routes"`
}

type routerRequestMetricKey struct {
	publicModel string
	endpoint    string
	result      FailureCategory
	success     bool
}

type routerRouteMetricKey struct {
	publicModel string
	endpoint    string
	routeID     string
	upstreamID  string
	statusClass string
	result      FailureCategory
}

type RouterMetrics struct {
	mu       sync.RWMutex
	started  time.Time
	totals   RouterMetricTotals
	requests map[routerRequestMetricKey]*RouterRequestMetric
	routes   map[routerRouteMetricKey]*RouterRouteMetric
}

func NewRouterMetrics() *RouterMetrics {
	return &RouterMetrics{
		started:  time.Now().UTC(),
		requests: make(map[routerRequestMetricKey]*RouterRequestMetric),
		routes:   make(map[routerRouteMetricKey]*RouterRouteMetric),
	}
}

func (m *RouterMetrics) Record(
	publicModel string,
	endpoint string,
	result ExecutionResult,
	latency time.Duration,
	timeToFirstToken time.Duration,
	streamState StreamState,
) {
	if m == nil {
		return
	}
	requestKey := routerRequestMetricKey{
		publicModel: publicModel,
		endpoint:    endpoint,
		result:      result.FailureCategory,
		success:     result.Success,
	}
	latencyMS := nonNegativeMilliseconds(latency)
	ttftMS := nonNegativeMilliseconds(timeToFirstToken)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.totals.Requests++
	if result.Success {
		m.totals.Successes++
	} else {
		m.totals.Failures++
	}
	m.totals.Attempts += uint64(len(result.Attempts))
	if len(result.Attempts) > 1 {
		m.totals.Failovers += uint64(len(result.Attempts) - 1)
	}
	if streamState == StreamStateFailedPartial {
		m.totals.PartialStreamFailures++
	}
	if result.UsageMissing {
		m.totals.UsageMissing++
	}
	if result.FailureCategory == FailureImageAmbiguous {
		m.totals.ImageAmbiguousFailures++
	}

	requestMetric := m.requests[requestKey]
	if requestMetric == nil {
		requestMetric = &RouterRequestMetric{
			PublicModel: publicModel,
			Endpoint:    endpoint,
			Result:      result.FailureCategory,
			Success:     result.Success,
		}
		m.requests[requestKey] = requestMetric
	}
	requestMetric.Count++
	requestMetric.LatencyCount++
	requestMetric.LatencyTotalMS += latencyMS
	if latencyMS > requestMetric.LatencyMaxMS {
		requestMetric.LatencyMaxMS = latencyMS
	}
	if timeToFirstToken > 0 {
		requestMetric.TTFTCount++
		requestMetric.TTFTTotalMS += ttftMS
		if ttftMS > requestMetric.TTFTMaxMS {
			requestMetric.TTFTMaxMS = ttftMS
		}
	}

	for _, attempt := range result.Attempts {
		routeKey := routerRouteMetricKey{
			publicModel: publicModel,
			endpoint:    endpoint,
			routeID:     attempt.RouteID,
			upstreamID:  attempt.UpstreamID,
			statusClass: routerStatusClass(attempt.StatusCode),
			result:      attempt.FailureCategory,
		}
		routeMetric := m.routes[routeKey]
		if routeMetric == nil {
			routeMetric = &RouterRouteMetric{
				PublicModel: publicModel,
				Endpoint:    endpoint,
				RouteID:     attempt.RouteID,
				UpstreamID:  attempt.UpstreamID,
				StatusClass: routeKey.statusClass,
				Result:      attempt.FailureCategory,
			}
			m.routes[routeKey] = routeMetric
		}
		routeMetric.Selections++
	}
}

func (m *RouterMetrics) Snapshot() RouterMetricsSnapshot {
	if m == nil {
		now := time.Now().UTC()
		return RouterMetricsSnapshot{StartedAt: now, ObservedAt: now}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot := RouterMetricsSnapshot{
		StartedAt:  m.started,
		ObservedAt: time.Now().UTC(),
		Totals:     m.totals,
		Requests:   make([]RouterRequestMetric, 0, len(m.requests)),
		Routes:     make([]RouterRouteMetric, 0, len(m.routes)),
	}
	for _, metric := range m.requests {
		snapshot.Requests = append(snapshot.Requests, *metric)
	}
	for _, metric := range m.routes {
		snapshot.Routes = append(snapshot.Routes, *metric)
	}
	sort.Slice(snapshot.Requests, func(i, j int) bool {
		left, right := snapshot.Requests[i], snapshot.Requests[j]
		if left.PublicModel != right.PublicModel {
			return left.PublicModel < right.PublicModel
		}
		if left.Endpoint != right.Endpoint {
			return left.Endpoint < right.Endpoint
		}
		if left.Success != right.Success {
			return left.Success
		}
		return left.Result < right.Result
	})
	sort.Slice(snapshot.Routes, func(i, j int) bool {
		left, right := snapshot.Routes[i], snapshot.Routes[j]
		if left.RouteID != right.RouteID {
			return left.RouteID < right.RouteID
		}
		if left.Endpoint != right.Endpoint {
			return left.Endpoint < right.Endpoint
		}
		if left.StatusClass != right.StatusClass {
			return left.StatusClass < right.StatusClass
		}
		return left.Result < right.Result
	})
	return snapshot
}

func (m *RouterMetrics) Reconcile(snapshot *Snapshot) {
	if m == nil || snapshot == nil {
		return
	}
	models := make(map[string]struct{}, len(snapshot.groupIDByModel))
	for publicModel := range snapshot.groupIDByModel {
		models[publicModel] = struct{}{}
	}
	routes := make(map[string]struct{}, len(snapshot.routesByID))
	for routeID := range snapshot.routesByID {
		routes[routeID] = struct{}{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.requests {
		if _, exists := models[key.publicModel]; !exists {
			delete(m.requests, key)
		}
	}
	for key := range m.routes {
		if _, modelExists := models[key.publicModel]; !modelExists {
			delete(m.routes, key)
			continue
		}
		if _, routeExists := routes[key.routeID]; !routeExists {
			delete(m.routes, key)
		}
	}
}

func nonNegativeMilliseconds(duration time.Duration) uint64 {
	if duration <= 0 {
		return 0
	}
	return uint64(duration.Milliseconds())
}

func routerStatusClass(statusCode int) string {
	switch {
	case statusCode <= 0:
		return "transport"
	case statusCode < 200:
		return "1xx"
	case statusCode < 300:
		return "2xx"
	case statusCode < 400:
		return "3xx"
	case statusCode < 500:
		return "4xx"
	default:
		return "5xx"
	}
}
